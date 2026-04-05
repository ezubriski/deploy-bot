package reposcanner

import (
	"testing"

	"github.com/ezubriski/deploy-bot/internal/config"
)

func TestParseRepoConfig_Valid(t *testing.T) {
	data := []byte(`{
		"apps": [
			{
				"app": "myapp",
				"environment": "dev",
				"kustomize_path": "apps/myapp/kustomization.yaml",
				"ecr_repo": "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
				"tag_pattern": "^v\\d+\\.\\d+\\.\\d+$"
			},
			{
				"app": "myapp",
				"environment": "prod",
				"kustomize_path": "apps/myapp/prod/kustomization.yaml",
				"ecr_repo": "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp"
			}
		]
	}`)

	apps, errs := parseRepoConfig(data, "org/myapp", config.RepoDiscoveryConfig{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(apps) != 2 {
		t.Fatalf("expected 2 apps, got %d", len(apps))
	}
	if apps[0].App != "myapp" || apps[0].Environment != "dev" {
		t.Errorf("app[0] = %+v", apps[0])
	}
	if apps[1].Environment != "prod" {
		t.Errorf("app[1].Environment = %q, want prod", apps[1].Environment)
	}
	if apps[0].SourceRepo != "org/myapp" {
		t.Errorf("SourceRepo = %q, want org/myapp", apps[0].SourceRepo)
	}
}

func TestParseRepoConfig_MissingRequiredFields(t *testing.T) {
	data := []byte(`{
		"apps": [
			{"app": "", "environment": "dev", "kustomize_path": "path", "ecr_repo": "repo"},
			{"app": "myapp", "environment": "", "kustomize_path": "path", "ecr_repo": "repo"},
			{"app": "myapp", "environment": "dev", "kustomize_path": "", "ecr_repo": "repo"},
			{"app": "myapp", "environment": "dev", "kustomize_path": "path", "ecr_repo": ""}
		]
	}`)

	apps, errs := parseRepoConfig(data, "org/repo", config.RepoDiscoveryConfig{})
	if len(errs) != 4 {
		t.Fatalf("expected 4 errors, got %d: %v", len(errs), errs)
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 valid apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_InvalidRegex(t *testing.T) {
	data := []byte(`{
		"apps": [
			{
				"app": "myapp",
				"environment": "dev",
				"kustomize_path": "path",
				"ecr_repo": "repo",
				"tag_pattern": "[invalid"
			}
		]
	}`)

	apps, errs := parseRepoConfig(data, "org/repo", config.RepoDiscoveryConfig{})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 valid apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_PartialValid(t *testing.T) {
	data := []byte(`{
		"apps": [
			{"app": "good", "environment": "dev", "kustomize_path": "path/dev", "ecr_repo": "repo"},
			{"app": "", "environment": "dev", "kustomize_path": "path/broken", "ecr_repo": "repo"},
			{"app": "also-good", "environment": "prod", "kustomize_path": "path/prod", "ecr_repo": "repo"}
		]
	}`)

	apps, errs := parseRepoConfig(data, "org/repo", config.RepoDiscoveryConfig{})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if len(apps) != 2 {
		t.Fatalf("expected 2 valid apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_InvalidJSON(t *testing.T) {
	data := []byte(`not json`)
	apps, errs := parseRepoConfig(data, "org/repo", config.RepoDiscoveryConfig{})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_EmptyApps(t *testing.T) {
	data := []byte(`{"apps": []}`)
	apps, errs := parseRepoConfig(data, "org/repo", config.RepoDiscoveryConfig{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_WithAPIVersion(t *testing.T) {
	data := []byte(`{
		"apiVersion": "deploy-bot/v1",
		"apps": [
			{
				"app": "myapp",
				"environment": "dev",
				"kustomize_path": "path",
				"ecr_repo": "repo"
			}
		]
	}`)

	apps, errs := parseRepoConfig(data, "org/repo", config.RepoDiscoveryConfig{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(apps))
	}
}

func TestParseRepoConfig_UnknownAPIVersion(t *testing.T) {
	data := []byte(`{
		"apiVersion": "deploy-bot/v99",
		"apps": [
			{
				"app": "myapp",
				"environment": "dev",
				"kustomize_path": "path",
				"ecr_repo": "repo"
			}
		]
	}`)

	apps, errs := parseRepoConfig(data, "org/repo", config.RepoDiscoveryConfig{})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_AutoDeployFields(t *testing.T) {
	data := []byte(`{
		"apps": [
			{
				"app": "myapp",
				"environment": "dev",
				"kustomize_path": "path",
				"ecr_repo": "repo",
				"auto_deploy": true,
				"auto_deploy_approver_group": "C01234567"
			}
		]
	}`)

	apps, errs := parseRepoConfig(data, "org/repo", config.RepoDiscoveryConfig{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !apps[0].AutoDeploy {
		t.Error("expected auto_deploy = true")
	}
	if apps[0].AutoDeployApproverGroup != "C01234567" {
		t.Errorf("auto_deploy_approver_group = %q", apps[0].AutoDeployApproverGroup)
	}
}

func TestParseRepoConfig_V2_RepoNaming_DeriveFields(t *testing.T) {
	data := []byte(`{
		"apiVersion": "deploy-bot/v2",
		"apps": [
			{
				"environment": "dev",
				"ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service"
			},
			{
				"environment": "prod",
				"ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service"
			}
		]
	}`)

	rd := config.RepoDiscoveryConfig{EnforceRepoNaming: true}
	apps, errs := parseRepoConfig(data, "org/my-service", rd)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(apps) != 2 {
		t.Fatalf("expected 2 apps, got %d", len(apps))
	}
	if apps[0].App != "my-service" {
		t.Errorf("apps[0].App = %q, want my-service", apps[0].App)
	}
	if apps[0].KustomizePath != "dev/my-service/kustomization.yaml" {
		t.Errorf("apps[0].KustomizePath = %q, want dev/my-service/kustomization.yaml", apps[0].KustomizePath)
	}
	if apps[1].KustomizePath != "prod/my-service/kustomization.yaml" {
		t.Errorf("apps[1].KustomizePath = %q, want prod/my-service/kustomization.yaml", apps[1].KustomizePath)
	}
}

func TestParseRepoConfig_V2_RepoNaming_RejectsConflictingName(t *testing.T) {
	data := []byte(`{
		"apiVersion": "deploy-bot/v2",
		"apps": [
			{
				"app": "wrong-name",
				"environment": "dev",
				"ecr_repo": "repo"
			}
		]
	}`)

	rd := config.RepoDiscoveryConfig{EnforceRepoNaming: true}
	apps, errs := parseRepoConfig(data, "org/my-service", rd)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_V2_RepoNaming_RejectsConflictingPath(t *testing.T) {
	data := []byte(`{
		"apiVersion": "deploy-bot/v2",
		"apps": [
			{
				"environment": "dev",
				"kustomize_path": "custom/path/kustomization.yaml",
				"ecr_repo": "repo"
			}
		]
	}`)

	rd := config.RepoDiscoveryConfig{EnforceRepoNaming: true}
	apps, errs := parseRepoConfig(data, "org/my-service", rd)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_V2_NoRepoNaming_RequiresFields(t *testing.T) {
	// v2 without enforce_repo_naming still requires app and kustomize_path.
	data := []byte(`{
		"apiVersion": "deploy-bot/v2",
		"apps": [
			{
				"environment": "dev",
				"ecr_repo": "repo"
			}
		]
	}`)

	apps, errs := parseRepoConfig(data, "org/my-service", config.RepoDiscoveryConfig{})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_V1_EnforcedNaming_Rejected(t *testing.T) {
	data := []byte(`{
		"apiVersion": "deploy-bot/v1",
		"apps": [
			{"app": "svc", "environment": "dev", "kustomize_path": "p", "ecr_repo": "r"}
		]
	}`)

	rd := config.RepoDiscoveryConfig{EnforceRepoNaming: true}
	apps, errs := parseRepoConfig(data, "org/svc", rd)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_V1_ExemptRepo_Accepted(t *testing.T) {
	data := []byte(`{
		"apiVersion": "deploy-bot/v1",
		"apps": [
			{"app": "legacy", "environment": "dev", "kustomize_path": "custom/path", "ecr_repo": "r"}
		]
	}`)

	rd := config.RepoDiscoveryConfig{
		EnforceRepoNaming: true,
		ExemptRepos:       []string{"org/legacy"},
	}
	apps, errs := parseRepoConfig(data, "org/legacy", rd)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(apps))
	}
	if apps[0].App != "legacy" {
		t.Errorf("app = %q, want legacy", apps[0].App)
	}
	if apps[0].KustomizePath != "custom/path" {
		t.Errorf("kustomize_path = %q, want custom/path", apps[0].KustomizePath)
	}
}

func TestParseRepoConfig_CustomPathTemplate(t *testing.T) {
	data := []byte(`{
		"apiVersion": "deploy-bot/v2",
		"apps": [{"environment": "dev", "ecr_repo": "r"}]
	}`)

	rd := config.RepoDiscoveryConfig{
		EnforceRepoNaming:     true,
		KustomizePathTemplate: "apps/{repo}/overlays/{env}/kustomization.yaml",
	}
	apps, errs := parseRepoConfig(data, "org/my-service", rd)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(apps))
	}
	if apps[0].KustomizePath != "apps/my-service/overlays/dev/kustomization.yaml" {
		t.Errorf("kustomize_path = %q, want apps/my-service/overlays/dev/kustomization.yaml", apps[0].KustomizePath)
	}
}

func TestParseRepoConfig_DefaultTagPattern(t *testing.T) {
	data := []byte(`{
		"apiVersion": "deploy-bot/v2",
		"apps": [
			{"environment": "dev", "ecr_repo": "r"},
			{"environment": "prod", "ecr_repo": "r", "tag_pattern": "^release-.*$"}
		]
	}`)

	rd := config.RepoDiscoveryConfig{
		EnforceRepoNaming: true,
		DefaultTagPattern: "^v[0-9]+$",
	}
	apps, errs := parseRepoConfig(data, "org/svc", rd)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(apps) != 2 {
		t.Fatalf("expected 2 apps, got %d", len(apps))
	}
	// First app gets the default.
	if apps[0].TagPattern != "^v[0-9]+$" {
		t.Errorf("apps[0].TagPattern = %q, want default", apps[0].TagPattern)
	}
	// Second app overrides with its own.
	if apps[1].TagPattern != "^release-.*$" {
		t.Errorf("apps[1].TagPattern = %q, want ^release-.*$", apps[1].TagPattern)
	}
}
