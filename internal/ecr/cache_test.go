package ecr

import (
	"context"
	"regexp"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// --- repoNameFromURI ---

func TestRepoNameFromURI(t *testing.T) {
	cases := []struct {
		uri  string
		want string
	}{
		{"123456789012.dkr.ecr.us-west-2.amazonaws.com/nginx", "nginx"},
		{"123456789012.dkr.ecr.us-west-2.amazonaws.com/my/nested/repo", "my/nested/repo"},
		{"noslash", "noslash"},
		{"account.dkr.ecr.region.amazonaws.com/org/app", "org/app"},
	}
	for _, tc := range cases {
		got := repoNameFromURI(tc.uri)
		if got != tc.want {
			t.Errorf("repoNameFromURI(%q) = %q, want %q", tc.uri, got, tc.want)
		}
	}
}

// --- registryIDFromURI ---

func TestRegistryIDFromURI(t *testing.T) {
	cases := []struct {
		uri  string
		want string
	}{
		{"123456789012.dkr.ecr.us-west-2.amazonaws.com/nginx", "123456789012"},
		{"123456789012.dkr.ecr.eu-west-1.amazonaws.com/myapp", "123456789012"},
		{"nodot", ""},
	}
	for _, tc := range cases {
		got := registryIDFromURI(tc.uri)
		if got != tc.want {
			t.Errorf("registryIDFromURI(%q) = %q, want %q", tc.uri, got, tc.want)
		}
	}
}

// newTestCache builds a Cache with pre-populated apps, bypassing NewCache
// (which requires real AWS credentials).
func newTestCache(t *testing.T, apps map[string][]string, patterns map[string]string) *Cache {
	t.Helper()
	m := metrics.New(prometheus.NewRegistry())
	log := zap.NewNop()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	c := &Cache{
		apps:    make(map[string]*appCache, len(apps)),
		client:  nil, // nil is safe as long as tests don't trigger ECR API calls
		rdb:     rdb,
		metrics: m,
		log:     log,
	}

	for name, tags := range apps {
		pat := patterns[name]
		if pat == "" {
			pat = ".*"
		}
		re := regexp.MustCompile(pat)
		tagsCopy := make([]string, len(tags))
		copy(tagsCopy, tags)
		c.apps[name] = &appCache{
			mu:         sync.RWMutex{},
			tags:       tagsCopy,
			pattern:    re,
			repoName:   "nginx",
			registryID: "123456789012",
		}
	}
	return c
}

// --- RecentTags ---

func TestRecentTags(t *testing.T) {
	allTags := []string{"v1.26.1", "v1.26.0", "v1.25.1", "v1.25.0", "v1.24.0", "v1.23.9"}
	c := newTestCache(t,
		map[string][]string{"nginx": allTags},
		nil,
	)

	got := c.RecentTags("nginx")
	if len(got) != maxRecentTags {
		t.Fatalf("RecentTags returned %d tags, want %d", len(got), maxRecentTags)
	}
	// Must be the first maxRecentTags entries (newest first).
	for i := 0; i < maxRecentTags; i++ {
		if got[i] != allTags[i] {
			t.Errorf("RecentTags[%d] = %q, want %q", i, got[i], allTags[i])
		}
	}
}

func TestRecentTags_FewerThanMax(t *testing.T) {
	tags := []string{"v1.0.0", "v0.9.0"}
	c := newTestCache(t, map[string][]string{"nginx": tags}, nil)

	got := c.RecentTags("nginx")
	if len(got) != 2 {
		t.Fatalf("got %d tags, want 2", len(got))
	}
}

func TestRecentTags_UnknownApp(t *testing.T) {
	c := newTestCache(t, map[string][]string{}, nil)
	got := c.RecentTags("no-such-app")
	if got != nil {
		t.Errorf("expected nil for unknown app, got %v", got)
	}
}

// --- Tags (explicit limit) ---

func TestTags_Limit(t *testing.T) {
	tags := []string{"v1.26.1", "v1.26.0", "v1.25.1", "v1.25.0", "v1.24.0"}
	c := newTestCache(t, map[string][]string{"nginx": tags}, nil)

	got := c.Tags("nginx", 3)
	if len(got) != 3 {
		t.Fatalf("Tags(3) returned %d tags, want 3", len(got))
	}
	for i, want := range tags[:3] {
		if got[i] != want {
			t.Errorf("Tags[%d] = %q, want %q", i, got[i], want)
		}
	}
}

func TestTags_LimitExceedsAvailable(t *testing.T) {
	tags := []string{"v1.0.0", "v0.9.0"}
	c := newTestCache(t, map[string][]string{"nginx": tags}, nil)

	got := c.Tags("nginx", 10)
	if len(got) != 2 {
		t.Fatalf("Tags(10) returned %d tags, want 2", len(got))
	}
}

// --- ValidateTag ---

func TestValidateTag_UnknownApp(t *testing.T) {
	c := newTestCache(t, map[string][]string{}, nil)
	_, err := c.ValidateTag(context.Background(), "no-such-app", "v1.0.0")
	if err == nil {
		t.Fatal("expected error for unknown app")
	}
}

func TestValidateTag_PatternMismatch(t *testing.T) {
	c := newTestCache(t,
		map[string][]string{"nginx": {"v1.26.1"}},
		map[string]string{"nginx": `^v\d+\.\d+\.\d+$`},
	)
	// "latest" does not match the semver pattern — no ECR call needed.
	ok, err := c.ValidateTag(context.Background(), "nginx", "latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected false for pattern-mismatched tag")
	}
}

func TestValidateTag_CacheHit(t *testing.T) {
	c := newTestCache(t,
		map[string][]string{"nginx": {"v1.26.1", "v1.25.0"}},
		map[string]string{"nginx": `^v\d+\.\d+\.\d+$`},
	)
	ok, err := c.ValidateTag(context.Background(), "nginx", "v1.25.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected true for tag present in cache")
	}
}
