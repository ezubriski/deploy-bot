//go:build integration

package integration

import (
	"testing"

	ecr "github.com/ezubriski/deploy-bot/internal/ecr"
)

// TestValidateTag_CacheMiss exercises the ECR API fallback path in ValidateTag.
// A fresh cache is created without calling Populate, so env.tag will not be
// present in memory. ValidateTag must fall through to a live DescribeImages
// call and confirm the tag exists.
func TestValidateTag_CacheMiss(t *testing.T) {
	freshCache, err := ecr.NewCache(env.ctx, env.cfg, env.store.Redis(), env.metrics, env.log)
	if err != nil {
		t.Fatalf("new ecr cache: %v", err)
	}

	ok, err := freshCache.ValidateTag(env.ctx, env.app, env.tag)
	if err != nil {
		t.Fatalf("ValidateTag: %v", err)
	}
	if !ok {
		t.Errorf("expected tag %q to be valid for app %q via direct ECR lookup", env.tag, env.app)
	}
}
