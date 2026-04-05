package ecr

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/metrics"
)

const (
	refreshInterval = 5 * time.Minute
	maxRecentTags   = 5
)

type appCache struct {
	mu         sync.RWMutex
	tags       []string // sorted newest first
	pattern    *regexp.Regexp
	repoName   string
	registryID string
}

// Cache holds per-app tag caches backed by a single shared ECR client
// (all apps use the same assumed role).
type Cache struct {
	mu      sync.RWMutex // protects apps map for AddApps
	apps    map[string]*appCache
	client  *ecr.Client // shared across all apps
	metrics *metrics.Metrics
	log     *zap.Logger
}

func NewCache(ctx context.Context, cfg *config.Config, m *metrics.Metrics, log *zap.Logger) (*Cache, error) {
	baseCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	clientCfg := baseCfg.Copy()
	clientCfg.Region = cfg.AWS.ECRRegion
	if cfg.AWS.ECRRoleARN != "" {
		stsClient := sts.NewFromConfig(baseCfg)
		clientCfg.Credentials = stscreds.NewAssumeRoleProvider(stsClient, cfg.AWS.ECRRoleARN)
	}

	c := &Cache{
		apps:    make(map[string]*appCache),
		client:  ecr.NewFromConfig(clientCfg),
		metrics: m,
		log:     log,
	}

	for _, app := range cfg.Apps {
		ac, err := newAppCache(app)
		if err != nil {
			return nil, err
		}
		c.apps[app.App] = ac
	}

	return c, nil
}

// AddApps registers cache entries for any apps in the slice that are not
// already present. Existing entries are left unchanged. Safe to call
// concurrently with other cache operations.
func (c *Cache) AddApps(apps []config.AppConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, app := range apps {
		if _, exists := c.apps[app.App]; exists {
			continue
		}
		ac, err := newAppCache(app)
		if err != nil {
			return err
		}
		c.apps[app.App] = ac
		c.log.Info("ecr cache: registered new app", zap.String("app", app.App))
	}
	return nil
}

func newAppCache(app config.AppConfig) (*appCache, error) {
	pat, err := regexp.Compile(app.TagPattern)
	if err != nil {
		return nil, fmt.Errorf("compile tag pattern for %s: %w", app.App, err)
	}
	return &appCache{
		pattern:    pat,
		repoName:   repoNameFromURI(app.ECRRepo),
		registryID: registryIDFromURI(app.ECRRepo),
	}, nil
}

// Populate fetches tags for all apps concurrently. Apps sharing the same ECR
// repo are deduplicated so the API is called once per unique repository.
// Fails open — logs warnings on error.
func (c *Cache) Populate(ctx context.Context) {
	c.mu.RLock()
	apps := make(map[string]*appCache, len(c.apps))
	for k, v := range c.apps {
		apps[k] = v
	}
	c.mu.RUnlock()

	// Deduplicate by repo — multiple apps may share the same ECR repo.
	type appEntry struct {
		name string
		ac   *appCache
	}
	type repoGroup struct {
		apps []appEntry
	}
	byRepo := map[string]*repoGroup{}
	for name, ac := range apps {
		key := ac.registryID + "/" + ac.repoName
		if g, ok := byRepo[key]; ok {
			g.apps = append(g.apps, appEntry{name, ac})
		} else {
			byRepo[key] = &repoGroup{apps: []appEntry{{name, ac}}}
		}
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, group := range byRepo {
		wg.Add(1)
		sem <- struct{}{}
		go func(g *repoGroup) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			images, err := c.fetchImages(ctx, g.apps[0].ac)
			if err != nil {
				c.log.Warn("ecr cache populate failed", zap.String("app", g.apps[0].name), zap.Error(err))
				return
			}
			for _, entry := range g.apps {
				tags := applyImages(images, entry.ac)
				entry.ac.mu.Lock()
				entry.ac.tags = tags
				entry.ac.mu.Unlock()
				c.metrics.ObserveECRRefresh(entry.name, time.Since(start))
				c.log.Debug("ecr cache refreshed", zap.String("app", entry.name), zap.Int("tags", len(tags)))
			}
		}(group)
	}
	wg.Wait()
}

// StartRefresh runs background refresh every 5 minutes until ctx is cancelled.
func (c *Cache) StartRefresh(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.mu.RLock()
				apps := make(map[string]*appCache, len(c.apps))
				for k, v := range c.apps {
					apps[k] = v
				}
				c.mu.RUnlock()

				for name, ac := range apps {
					if err := c.refresh(ctx, name, ac); err != nil {
						c.log.Warn("ecr cache refresh failed", zap.String("app", name), zap.Error(err))
					}
				}
			}
		}
	}()
}

func (c *Cache) fetchImages(ctx context.Context, ac *appCache) ([]types.ImageDetail, error) {
	input := &ecr.DescribeImagesInput{
		RepositoryName: aws.String(ac.repoName),
		RegistryId:     aws.String(ac.registryID),
		Filter:         &types.DescribeImagesFilter{TagStatus: types.TagStatusTagged},
	}

	var images []types.ImageDetail
	paginator := ecr.NewDescribeImagesPaginator(c.client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe images: %w", err)
		}
		images = append(images, page.ImageDetails...)
	}
	return images, nil
}

func applyImages(images []types.ImageDetail, ac *appCache) []string {
	type tagTime struct {
		tag  string
		time time.Time
	}
	var tagged []tagTime
	for _, img := range images {
		if img.ImagePushedAt == nil {
			continue
		}
		for _, t := range img.ImageTags {
			if ac.pattern.MatchString(t) {
				tagged = append(tagged, tagTime{tag: t, time: *img.ImagePushedAt})
			}
		}
	}
	sort.Slice(tagged, func(i, j int) bool {
		return tagged[i].time.After(tagged[j].time)
	})

	tags := make([]string, 0, len(tagged))
	for _, tt := range tagged {
		tags = append(tags, tt.tag)
	}
	return tags
}

func (c *Cache) refresh(ctx context.Context, appName string, ac *appCache) error {
	start := time.Now()

	images, err := c.fetchImages(ctx, ac)
	if err != nil {
		return err
	}

	tags := applyImages(images, ac)

	ac.mu.Lock()
	ac.tags = tags
	ac.mu.Unlock()

	c.metrics.ObserveECRRefresh(appName, time.Since(start))
	c.log.Debug("ecr cache refreshed", zap.String("app", appName), zap.Int("tags", len(tags)))
	return nil
}

// Tags returns up to limit of the most recent tags for an app.
func (c *Cache) Tags(appName string, limit int) []string {
	c.mu.RLock()
	ac, ok := c.apps[appName]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	n := len(ac.tags)
	if n > limit {
		n = limit
	}
	out := make([]string, n)
	copy(out, ac.tags[:n])
	return out
}

// RecentTags returns up to 5 most recent tags for an app.
func (c *Cache) RecentTags(appName string) []string {
	c.mu.RLock()
	ac, ok := c.apps[appName]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	n := len(ac.tags)
	if n > maxRecentTags {
		n = maxRecentTags
	}
	out := make([]string, n)
	copy(out, ac.tags[:n])
	return out
}

// ValidateTag checks if a tag is valid for an app. Checks cache first, falls back to direct ECR lookup.
func (c *Cache) ValidateTag(ctx context.Context, appName, tag string) (bool, error) {
	c.mu.RLock()
	ac, ok := c.apps[appName]
	c.mu.RUnlock()
	if !ok {
		return false, fmt.Errorf("unknown app: %s", appName)
	}

	if !ac.pattern.MatchString(tag) {
		return false, nil
	}

	ac.mu.RLock()
	for _, t := range ac.tags {
		if t == tag {
			ac.mu.RUnlock()
			c.metrics.RecordECRCacheHit(appName)
			return true, nil
		}
	}
	ac.mu.RUnlock()

	c.metrics.RecordECRCacheMiss(appName)
	out, err := c.client.DescribeImages(ctx, &ecr.DescribeImagesInput{
		RepositoryName: aws.String(ac.repoName),
		RegistryId:     aws.String(ac.registryID),
		ImageIds:       []types.ImageIdentifier{{ImageTag: aws.String(tag)}},
	})
	if err != nil {
		return false, fmt.Errorf("ecr lookup: %w", err)
	}
	return len(out.ImageDetails) > 0, nil
}

func repoNameFromURI(uri string) string {
	idx := strings.Index(uri, "/")
	if idx < 0 {
		return uri
	}
	return uri[idx+1:]
}

func registryIDFromURI(uri string) string {
	idx := strings.Index(uri, ".")
	if idx < 0 {
		return ""
	}
	return uri[:idx]
}
