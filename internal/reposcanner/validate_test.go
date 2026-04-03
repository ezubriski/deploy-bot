package reposcanner

import (
	"testing"
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

	apps, errs := parseRepoConfig(data, "org/myapp")
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

	apps, errs := parseRepoConfig(data, "org/repo")
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

	apps, errs := parseRepoConfig(data, "org/repo")
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
			{"app": "good", "environment": "dev", "kustomize_path": "path", "ecr_repo": "repo"},
			{"app": "", "environment": "dev", "kustomize_path": "path", "ecr_repo": "repo"},
			{"app": "also-good", "environment": "prod", "kustomize_path": "path", "ecr_repo": "repo"}
		]
	}`)

	apps, errs := parseRepoConfig(data, "org/repo")
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if len(apps) != 2 {
		t.Fatalf("expected 2 valid apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_InvalidJSON(t *testing.T) {
	data := []byte(`not json`)
	apps, errs := parseRepoConfig(data, "org/repo")
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if len(apps) != 0 {
		t.Fatalf("expected 0 apps, got %d", len(apps))
	}
}

func TestParseRepoConfig_EmptyApps(t *testing.T) {
	data := []byte(`{"apps": []}`)
	apps, errs := parseRepoConfig(data, "org/repo")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
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

	apps, errs := parseRepoConfig(data, "org/repo")
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
