package config

import (
	"testing"
)

func authEntry(typ string, values ...string) AuthorizationEntry {
	return AuthorizationEntry{Type: typ, Value: values}
}

func TestValidateConfig_Valid(t *testing.T) {
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "org", Repo: "repo"},
		Authorization: []AuthorizationEntry{authEntry(AuthGitHubTeams, "deployers")},
		Slack:         SlackConfig{DeployChannel: "C123"},
		Deployment:    DeploymentConfig{StaleDuration: "2h", LockTTL: "5m", MergeMethod: "squash"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		Postgres:      PostgresConfig{Host: "localhost", Database: "d", User: "u"},
		Apps: []AppConfig{
			{App: "myapp", Environment: "dev", KustomizePath: "apps/myapp", ECRRepo: "123.dkr.ecr.us-east-1.amazonaws.com/myapp", TagPattern: "^v[0-9]+$"},
		},
	}

	errs := ValidateConfig(cfg)
	if len(errs) != 0 {
		t.Errorf("expected no errors, got %d: %v", len(errs), errs)
	}
}

func TestValidateConfig_MissingRequiredFields(t *testing.T) {
	cfg := &Config{}

	errs := ValidateConfig(cfg)

	required := map[string]bool{
		"github.org":                false,
		"github.repo":               false,
		"authorization.":            false,
		"slack.deploy_channel":      false,
		"deployment.stale_duration": false,
		"deployment.lock_ttl":       false,
		"aws.ecr_region":            false,
		"apps.":                     false,
	}

	for _, e := range errs {
		key := e.Section + "." + e.Field
		if _, ok := required[key]; ok {
			required[key] = true
		}
	}

	for key, found := range required {
		if !found {
			t.Errorf("expected error for %s", key)
		}
	}
}

func TestValidateConfig_InvalidDurations(t *testing.T) {
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "o", Repo: "r"},
		Authorization: []AuthorizationEntry{authEntry(AuthSlackEmails, "user@example.com")},
		Slack:         SlackConfig{DeployChannel: "C1"},
		Deployment:    DeploymentConfig{StaleDuration: "bad", LockTTL: "also_bad"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		Apps:          []AppConfig{{App: "a", Environment: "dev", KustomizePath: "p", ECRRepo: "r"}},
	}

	errs := ValidateConfig(cfg)

	found := map[string]bool{"deployment.stale_duration": false, "deployment.lock_ttl": false}
	for _, e := range errs {
		key := e.Section + "." + e.Field
		if _, ok := found[key]; ok {
			found[key] = true
		}
	}
	for key, ok := range found {
		if !ok {
			t.Errorf("expected error for %s", key)
		}
	}
}

func TestValidateConfig_InvalidMergeMethod(t *testing.T) {
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "o", Repo: "r"},
		Authorization: []AuthorizationEntry{authEntry(AuthSlackEmails, "user@example.com")},
		Slack:         SlackConfig{DeployChannel: "C1"},
		Deployment:    DeploymentConfig{StaleDuration: "2h", LockTTL: "5m", MergeMethod: "yolo"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		Apps:          []AppConfig{{App: "a", Environment: "dev", KustomizePath: "p", ECRRepo: "r"}},
	}

	errs := ValidateConfig(cfg)
	for _, e := range errs {
		if e.Section == "deployment" && e.Field == "merge_method" {
			return
		}
	}
	t.Error("expected error for invalid merge_method")
}

func TestValidateConfig_InvalidTagPattern(t *testing.T) {
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "o", Repo: "r"},
		Authorization: []AuthorizationEntry{authEntry(AuthSlackEmails, "user@example.com")},
		Slack:         SlackConfig{DeployChannel: "C1"},
		Deployment:    DeploymentConfig{StaleDuration: "2h", LockTTL: "5m"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		Apps:          []AppConfig{{App: "a", Environment: "dev", KustomizePath: "p", ECRRepo: "r", TagPattern: "[invalid"}},
	}

	errs := ValidateConfig(cfg)
	for _, e := range errs {
		if e.Field == "tag_pattern" {
			return
		}
	}
	t.Error("expected error for invalid tag_pattern")
}

func TestValidateConfig_DuplicateApps(t *testing.T) {
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "o", Repo: "r"},
		Authorization: []AuthorizationEntry{authEntry(AuthSlackEmails, "user@example.com")},
		Slack:         SlackConfig{DeployChannel: "C1"},
		Deployment:    DeploymentConfig{StaleDuration: "2h", LockTTL: "5m"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		Apps: []AppConfig{
			{App: "myapp", Environment: "dev", KustomizePath: "p1", ECRRepo: "r1"},
			{App: "myapp", Environment: "dev", KustomizePath: "p2", ECRRepo: "r2"},
		},
	}

	errs := ValidateConfig(cfg)
	for _, e := range errs {
		if e.Field == "app" && e.Section == "apps[1]" {
			return
		}
	}
	t.Error("expected duplicate app error")
}

func TestValidateConfig_ConflictingKustomizePaths(t *testing.T) {
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "o", Repo: "r"},
		Authorization: []AuthorizationEntry{authEntry(AuthSlackEmails, "user@example.com")},
		Slack:         SlackConfig{DeployChannel: "C1"},
		Deployment:    DeploymentConfig{StaleDuration: "2h", LockTTL: "5m"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		Apps: []AppConfig{
			{App: "frontend", Environment: "dev", KustomizePath: "apps/web/kustomization.yaml", ECRRepo: "r1"},
			{App: "backend", Environment: "dev", KustomizePath: "apps/web/kustomization.yaml", ECRRepo: "r2"},
		},
	}

	errs := ValidateConfig(cfg)
	for _, e := range errs {
		if e.Field == "kustomize_path" && e.Section == "apps[1]" {
			return
		}
	}
	t.Error("expected conflicting kustomize_path error")
}

func TestValidateConfig_SameKustomizePathDifferentAppsIsConflict(t *testing.T) {
	// Same path, different app names and environments — still a conflict.
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "o", Repo: "r"},
		Authorization: []AuthorizationEntry{authEntry(AuthSlackEmails, "user@example.com")},
		Slack:         SlackConfig{DeployChannel: "C1"},
		Deployment:    DeploymentConfig{StaleDuration: "2h", LockTTL: "5m"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		Apps: []AppConfig{
			{App: "myapp", Environment: "dev", KustomizePath: "apps/shared/kustomization.yaml", ECRRepo: "r1"},
			{App: "myapp", Environment: "prod", KustomizePath: "apps/shared/kustomization.yaml", ECRRepo: "r1"},
		},
	}

	errs := ValidateConfig(cfg)
	for _, e := range errs {
		if e.Field == "kustomize_path" {
			return
		}
	}
	t.Error("expected conflicting kustomize_path error for same path across environments")
}

func TestValidateConfig_PostgresMissingFields(t *testing.T) {
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "o", Repo: "r"},
		Authorization: []AuthorizationEntry{authEntry(AuthSlackEmails, "u@e.com")},
		Slack:         SlackConfig{DeployChannel: "C1"},
		Deployment:    DeploymentConfig{StaleDuration: "2h", LockTTL: "5m"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		// Postgres deliberately unset — all three required fields should error.
		Apps: []AppConfig{{App: "a", Environment: "dev", KustomizePath: "p", ECRRepo: "r"}},
	}

	errs := ValidateConfig(cfg)
	wanted := map[string]bool{
		"postgres.host":     false,
		"postgres.database": false,
		"postgres.user":     false,
	}
	for _, e := range errs {
		key := e.Section + "." + e.Field
		if _, ok := wanted[key]; ok {
			wanted[key] = true
		}
	}
	for k, found := range wanted {
		if !found {
			t.Errorf("expected %s validation error", k)
		}
	}
}

func TestValidateConfig_PostgresRetentionBelowFloor(t *testing.T) {
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "o", Repo: "r"},
		Authorization: []AuthorizationEntry{authEntry(AuthSlackEmails, "u@e.com")},
		Slack:         SlackConfig{DeployChannel: "C1"},
		Deployment:    DeploymentConfig{StaleDuration: "2h", LockTTL: "5m"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		Postgres: PostgresConfig{
			Host:     "h",
			Database: "d",
			User:     "u",
			// 1 year — below the 390-day audit floor.
			RetentionHistory: "8760h",
		},
		Apps: []AppConfig{{App: "a", Environment: "dev", KustomizePath: "p", ECRRepo: "r"}},
	}

	errs := ValidateConfig(cfg)
	for _, e := range errs {
		if e.Section == "postgres" && e.Field == "retention_history" {
			return
		}
	}
	t.Error("expected postgres.retention_history audit-floor error")
}

func TestValidateConfig_PostgresRetentionAtFloorOK(t *testing.T) {
	cfg := &Config{
		GitHub:        GitHubConfig{Org: "o", Repo: "r"},
		Authorization: []AuthorizationEntry{authEntry(AuthSlackEmails, "u@e.com")},
		Slack:         SlackConfig{DeployChannel: "C1"},
		Deployment:    DeploymentConfig{StaleDuration: "2h", LockTTL: "5m"},
		AWS:           AWSConfig{ECRRegion: "us-east-1"},
		Postgres: PostgresConfig{
			Host:     "h",
			Database: "d",
			User:     "u",
			// Exactly 390 days in hours: 390*24 = 9360.
			RetentionHistory: "9360h",
		},
		Apps: []AppConfig{{App: "a", Environment: "dev", KustomizePath: "p", ECRRepo: "r"}},
	}

	errs := ValidateConfig(cfg)
	for _, e := range errs {
		if e.Section == "postgres" && e.Field == "retention_history" {
			t.Errorf("did not expect retention_history error at exactly the floor, got %v", e)
		}
	}
}
