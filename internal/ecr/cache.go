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

	"github.com/yourorg/deploy-bot/internal/config"
	"github.com/yourorg/deploy-bot/internal/metrics"
)

const (
	refreshInterval = 5 * time.Minute
	maxRecentTags   = 5
)

type appCache struct {
	mu         sync.RWMutex
	tags       []string // sorted newest first
	pattern    *regexp.Regexp
	ecrRepo    string
	repoName   string
	registryID string
	client     *ecr.Client
}

type Cache struct {
	apps    map[string]*appCache
	metrics *metrics.Metrics
	log     *zap.Logger
}

func NewCache(ctx context.Context, cfg *config.Config, m *metrics.Metrics, log *zap.Logger) (*Cache, error) {
	baseCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	stsClient := sts.NewFromConfig(baseCfg)

	c := &Cache{
		apps:    make(map[string]*appCache),
		metrics: m,
		log:     log,
	}

	for _, app := range cfg.Apps {
		pat, err := regexp.Compile(app.TagPattern)
		if err != nil {
			return nil, fmt.Errorf("compile tag pattern for %s: %w", app.App, err)
		}

		provider := stscreds.NewAssumeRoleProvider(stsClient, cfg.AWS.ECRRoleARN)
		roleCfg := baseCfg.Copy()
		roleCfg.Credentials = aws.NewCredentialsCache(provider)
		roleCfg.Region = cfg.AWS.ECRRegion

		ecrClient := ecr.NewFromConfig(roleCfg)

		ac := &appCache{
			pattern:    pat,
			ecrRepo:    app.ECRRepo,
			repoName:   repoNameFromURI(app.ECRRepo),
			registryID: registryIDFromURI(app.ECRRepo),
			client:     ecrClient,
		}
		c.apps[app.App] = ac
	}

	return c, nil
}

// Populate fetches tags for all apps. Fails open — logs warnings on error.
func (c *Cache) Populate(ctx context.Context) {
	for name, ac := range c.apps {
		if err := c.refresh(ctx, name, ac); err != nil {
			c.log.Warn("ecr cache populate failed", zap.String("app", name), zap.Error(err))
		}
	}
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
				for name, ac := range c.apps {
					if err := c.refresh(ctx, name, ac); err != nil {
						c.log.Warn("ecr cache refresh failed", zap.String("app", name), zap.Error(err))
					}
				}
			}
		}
	}()
}

func (c *Cache) refresh(ctx context.Context, appName string, ac *appCache) error {
	start := time.Now()

	input := &ecr.DescribeImagesInput{
		RepositoryName: aws.String(ac.repoName),
		RegistryId:     aws.String(ac.registryID),
		Filter:         &types.DescribeImagesFilter{TagStatus: types.TagStatusTagged},
	}

	var images []types.ImageDetail
	paginator := ecr.NewDescribeImagesPaginator(ac.client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("describe images: %w", err)
		}
		images = append(images, page.ImageDetails...)
	}

	// Flatten tags with their push time
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

	ac.mu.Lock()
	ac.tags = tags
	ac.mu.Unlock()

	c.metrics.ObserveECRRefresh(appName, time.Since(start))
	c.log.Debug("ecr cache refreshed", zap.String("app", appName), zap.Int("tags", len(tags)))
	return nil
}

// RecentTags returns up to 5 most recent tags for an app.
func (c *Cache) RecentTags(appName string) []string {
	ac, ok := c.apps[appName]
	if !ok {
		return nil
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	if len(ac.tags) <= maxRecentTags {
		out := make([]string, len(ac.tags))
		copy(out, ac.tags)
		return out
	}
	out := make([]string, maxRecentTags)
	copy(out, ac.tags[:maxRecentTags])
	return out
}

// ValidateTag checks if a tag is valid for an app. Checks cache first, falls back to direct ECR lookup.
func (c *Cache) ValidateTag(ctx context.Context, appName, tag string) (bool, error) {
	ac, ok := c.apps[appName]
	if !ok {
		return false, fmt.Errorf("unknown app: %s", appName)
	}

	if !ac.pattern.MatchString(tag) {
		return false, nil
	}

	// Check cache first
	ac.mu.RLock()
	for _, t := range ac.tags {
		if t == tag {
			ac.mu.RUnlock()
			c.metrics.RecordECRCacheHit(appName)
			return true, nil
		}
	}
	ac.mu.RUnlock()

	// Fall back to direct ECR lookup
	c.metrics.RecordECRCacheMiss(appName)
	out, err := ac.client.DescribeImages(ctx, &ecr.DescribeImagesInput{
		RepositoryName: aws.String(ac.repoName),
		RegistryId:     aws.String(ac.registryID),
		ImageIds: []types.ImageIdentifier{
			{ImageTag: aws.String(tag)},
		},
	})
	if err != nil {
		return false, fmt.Errorf("ecr lookup: %w", err)
	}
	return len(out.ImageDetails) > 0, nil
}

// repoNameFromURI extracts the repo name (possibly with path prefix) from an ECR URI.
// e.g. "123456789.dkr.ecr.us-east-1.amazonaws.com/myorg/myapp" → "myorg/myapp"
func repoNameFromURI(uri string) string {
	idx := strings.Index(uri, "/")
	if idx < 0 {
		return uri
	}
	return uri[idx+1:]
}

// registryIDFromURI extracts the AWS account ID from an ECR URI.
// e.g. "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp" → "123456789"
func registryIDFromURI(uri string) string {
	idx := strings.Index(uri, ".")
	if idx < 0 {
		return ""
	}
	return uri[:idx]
}
