package ecr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/observability"
)

const (
	refreshInterval = 5 * time.Minute
	maxRecentTags   = 5
)

type appCache struct {
	mu         sync.RWMutex
	tags       []string // sorted newest first
	newestPush time.Time
	pattern    *regexp.Regexp
	repoName   string
	registryID string
}

// repoTag is a tag with its push timestamp, stored unfiltered in Redis.
type repoTag struct {
	Tag      string    `json:"tag"`
	PushedAt time.Time `json:"pushed_at"`
}

// ecrCacheEntry is the Redis-persisted form of a repo's tag cache.
// Tags are stored unfiltered so apps with different tag patterns sharing
// the same repo can filter on read without separate cache entries.
type ecrCacheEntry struct {
	Tags       []repoTag `json:"tags"`
	NewestPush time.Time `json:"newest_push"`
}

const ecrPrefix = "ecr:"

// Cache holds per-app tag caches backed by a single shared ECR client
// (all apps use the same assumed role). Tags are also persisted to Redis
// so they are shared across replicas and survive restarts.
type Cache struct {
	mu      sync.RWMutex // protects apps map for AddApps
	apps    map[string]*appCache
	client  *ecr.Client // shared across all apps
	rdb     *redis.Client
	metrics *metrics.Metrics
	log     *zap.Logger
}

func NewCache(ctx context.Context, cfg *config.Config, rdb *redis.Client, m *metrics.Metrics, log *zap.Logger) (*Cache, error) {
	baseCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	clientCfg := baseCfg.Copy()
	clientCfg.Region = cfg.AWS.ECRRegion
	observability.InstrumentAWSConfig(&clientCfg)

	c := &Cache{
		apps:    make(map[string]*appCache),
		client:  ecr.NewFromConfig(clientCfg),
		rdb:     rdb,
		metrics: m,
		log:     log,
	}

	for _, app := range cfg.Apps {
		ac, err := newAppCache(app)
		if err != nil {
			return nil, err
		}
		c.apps[app.FullName()] = ac
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
		if _, exists := c.apps[app.FullName()]; exists {
			continue
		}
		ac, err := newAppCache(app)
		if err != nil {
			return err
		}
		c.apps[app.FullName()] = ac
		c.log.Info("ecr cache: registered new app", zap.String("app", app.FullName()))
	}
	return nil
}

func newAppCache(app config.AppConfig) (*appCache, error) {
	pat, err := regexp.Compile(app.TagPattern)
	if err != nil {
		return nil, fmt.Errorf("compile tag pattern for %s: %w", app.FullName(), err)
	}
	return &appCache{
		pattern:    pat,
		repoName:   repoNameFromURI(app.ECRRepo),
		registryID: registryIDFromURI(app.ECRRepo),
	}, nil
}

// Populate loads tags for all apps. Tries Redis first (keyed by repo, shared
// across apps and replicas); falls back to ECR API for repos missing from Redis.
func (c *Cache) Populate(ctx context.Context) {
	c.mu.RLock()
	apps := make(map[string]*appCache, len(c.apps))
	for k, v := range c.apps {
		apps[k] = v
	}
	c.mu.RUnlock()

	type appEntry struct {
		name string
		ac   *appCache
	}

	// Group all apps by repo.
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

	// Try Redis per repo, then fall back to ECR API.
	var needsFetch []*repoGroup
	for _, group := range byRepo {
		sample := group.apps[0].ac
		if repoTags, _, ok := c.readRepoTagsFromRedis(ctx, sample); ok {
			for _, entry := range group.apps {
				filtered, newest := filterRepoTags(repoTags, entry.ac)
				entry.ac.mu.Lock()
				entry.ac.tags = filtered
				entry.ac.newestPush = newest
				entry.ac.mu.Unlock()
				c.log.Debug("ecr cache loaded from redis", zap.String("app", entry.name), zap.Int("tags", len(filtered)))
			}
		} else {
			needsFetch = append(needsFetch, group)
		}
	}

	if len(needsFetch) == 0 {
		return
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, group := range needsFetch {
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
			allRepoTags := imagesToRepoTags(images)
			if len(allRepoTags) > 0 {
				c.writeRepoTagsToRedis(ctx, g.apps[0].ac, allRepoTags, allRepoTags[0].PushedAt)
			}
			for _, entry := range g.apps {
				filtered, newest := filterRepoTags(allRepoTags, entry.ac)
				entry.ac.mu.Lock()
				entry.ac.tags = filtered
				entry.ac.newestPush = newest
				entry.ac.mu.Unlock()
				c.metrics.ObserveECRRefresh(entry.name, time.Since(start))
				c.log.Debug("ecr cache refreshed", zap.String("app", entry.name), zap.Int("tags", len(filtered)))
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
		Filter: &types.DescribeImagesFilter{
			TagStatus:   types.TagStatusTagged,
			ImageStatus: types.ImageStatusFilterActive,
		},
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

// imagesToRepoTags extracts all tagged images sorted newest-first (unfiltered).
func imagesToRepoTags(images []types.ImageDetail) []repoTag {
	var all []repoTag
	for _, img := range images {
		if img.ImagePushedAt == nil {
			continue
		}
		for _, t := range img.ImageTags {
			all = append(all, repoTag{Tag: t, PushedAt: *img.ImagePushedAt})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].PushedAt.After(all[j].PushedAt)
	})
	return all
}

func (c *Cache) refresh(ctx context.Context, appName string, ac *appCache) error {
	start := time.Now()

	images, err := c.fetchImages(ctx, ac)
	if err != nil {
		return err
	}

	allRepoTags := imagesToRepoTags(images)
	filteredTags, newest := filterRepoTags(allRepoTags, ac)

	ac.mu.Lock()
	previousNewest := ac.newestPush
	if newest.After(previousNewest) || len(ac.tags) == 0 {
		ac.tags = filteredTags
		ac.newestPush = newest
	}
	ac.mu.Unlock()

	// Write unfiltered repo tags to Redis (shared across apps with same repo).
	if len(allRepoTags) > 0 {
		repoNewest := allRepoTags[0].PushedAt
		c.writeRepoTagsToRedis(ctx, ac, allRepoTags, repoNewest)
	}

	c.metrics.ObserveECRRefresh(appName, time.Since(start))
	c.log.Debug("ecr cache refreshed", zap.String("app", appName), zap.Int("tags", len(filteredTags)))
	return nil
}

func repoKey(ac *appCache) string {
	return ecrPrefix + ac.registryID + "/" + ac.repoName
}

func (c *Cache) writeRepoTagsToRedis(ctx context.Context, ac *appCache, tags []repoTag, newest time.Time) {
	data, err := json.Marshal(ecrCacheEntry{Tags: tags, NewestPush: newest})
	if err != nil {
		return
	}
	c.rdb.Set(ctx, repoKey(ac), data, 0)
}

func (c *Cache) readRepoTagsFromRedis(ctx context.Context, ac *appCache) ([]repoTag, time.Time, bool) {
	data, err := c.rdb.Get(ctx, repoKey(ac)).Bytes()
	if err != nil {
		return nil, time.Time{}, false
	}
	var entry ecrCacheEntry
	if json.Unmarshal(data, &entry) != nil {
		return nil, time.Time{}, false
	}
	return entry.Tags, entry.NewestPush, true
}

// filterRepoTags applies the app's tag pattern to unfiltered repo tags,
// returning matched tags sorted newest-first.
func filterRepoTags(tags []repoTag, ac *appCache) ([]string, time.Time) {
	var filtered []string
	var newest time.Time
	for _, t := range tags {
		if ac.pattern.MatchString(t.Tag) {
			filtered = append(filtered, t.Tag)
			if t.PushedAt.After(newest) {
				newest = t.PushedAt
			}
		}
	}
	return filtered, newest
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
