package repoconfig

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantVer string
		wantErr bool
	}{
		{
			name:    "explicit v1",
			input:   `{"apiVersion":"deploy-bot/v1","apps":[]}`,
			wantVer: VersionV1,
		},
		{
			name:    "missing version defaults to v1",
			input:   `{"apps":[]}`,
			wantVer: VersionV1,
		},
		{
			name:    "unknown version",
			input:   `{"apiVersion":"deploy-bot/v99","apps":[]}`,
			wantErr: true,
		},
		{
			name:    "bad prefix",
			input:   `{"apiVersion":"other/v1","apps":[]}`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			input:   `{invalid`,
			wantErr: true,
		},
		{
			name:    "with apps",
			input:   `{"apiVersion":"deploy-bot/v1","apps":[{"app":"a","environment":"dev","kustomize_path":"p","ecr_repo":"r"}]}`,
			wantVer: VersionV1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.APIVersion != tt.wantVer {
				t.Errorf("APIVersion = %q, want %q", cfg.APIVersion, tt.wantVer)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Run("all valid", func(t *testing.T) {
		cfg := &RepoConfigFile{
			APIVersion: VersionV1,
			Apps: []AppEntry{
				{App: "a", Environment: "dev", KustomizePath: "apps/a/dev", ECRRepo: "r"},
				{App: "b", Environment: "prod", KustomizePath: "apps/b/prod", ECRRepo: "r", TagPattern: `^v\d+$`},
			},
		}
		errs := Validate(cfg)
		if len(errs) != 0 {
			t.Errorf("expected 0 errors, got %d: %v", len(errs), errs)
		}
	})

	t.Run("missing required fields", func(t *testing.T) {
		cfg := &RepoConfigFile{
			Apps: []AppEntry{
				{},                                                              // missing app
				{App: "a"},                                                      // missing environment
				{App: "b", Environment: "dev"},                                  // missing kustomize_path
				{App: "c", Environment: "dev", KustomizePath: "p"},              // missing ecr_repo
				{App: "d", Environment: "dev", KustomizePath: "p", ECRRepo: "r"}, // valid
			},
		}
		errs := Validate(cfg)
		if len(errs) != 4 {
			t.Fatalf("expected 4 errors, got %d: %v", len(errs), errs)
		}
		wantFields := []string{"app", "environment", "kustomize_path", "ecr_repo"}
		for i, want := range wantFields {
			if errs[i].Field != want {
				t.Errorf("errs[%d].Field = %q, want %q", i, errs[i].Field, want)
			}
		}
	})

	t.Run("invalid tag pattern", func(t *testing.T) {
		cfg := &RepoConfigFile{
			Apps: []AppEntry{
				{App: "a", Environment: "dev", KustomizePath: "p", ECRRepo: "r", TagPattern: "[invalid"},
			},
		}
		errs := Validate(cfg)
		if len(errs) != 1 || errs[0].Field != "tag_pattern" {
			t.Errorf("expected 1 tag_pattern error, got %v", errs)
		}
	})

	t.Run("duplicate app+environment", func(t *testing.T) {
		cfg := &RepoConfigFile{
			Apps: []AppEntry{
				{App: "a", Environment: "dev", KustomizePath: "p", ECRRepo: "r"},
				{App: "a", Environment: "dev", KustomizePath: "q", ECRRepo: "s"},
			},
		}
		errs := Validate(cfg)
		if len(errs) != 1 || errs[0].Field != "app+environment" {
			t.Errorf("expected 1 duplicate error, got %v", errs)
		}
		if errs[0].Index != 1 {
			t.Errorf("duplicate should be on index 1, got %d", errs[0].Index)
		}
	})

	t.Run("whitespace-only fields", func(t *testing.T) {
		cfg := &RepoConfigFile{
			Apps: []AppEntry{
				{App: "  ", Environment: "dev", KustomizePath: "p", ECRRepo: "r"},
			},
		}
		errs := Validate(cfg)
		if len(errs) != 1 || errs[0].Field != "app" {
			t.Errorf("expected app required error, got %v", errs)
		}
	})
}

func TestValidEntries(t *testing.T) {
	cfg := &RepoConfigFile{
		Apps: []AppEntry{
			{App: "a", Environment: "dev", KustomizePath: "apps/a", ECRRepo: "r"},
			{},
			{App: "b", Environment: "prod", KustomizePath: "apps/b", ECRRepo: "r"},
		},
	}
	errs := Validate(cfg)
	valid := ValidEntries(cfg, errs)
	if len(valid) != 2 || valid[0] != 0 || valid[1] != 2 {
		t.Errorf("ValidEntries = %v, want [0 2]", valid)
	}
}

func TestValidationError_Error(t *testing.T) {
	e1 := &ValidationError{Index: 0, App: "myapp", Field: "environment", Msg: "required"}
	want1 := `apps[0] (myapp): environment: required`
	if got := e1.Error(); got != want1 {
		t.Errorf("Error() = %q, want %q", got, want1)
	}

	e2 := &ValidationError{Index: 1, Field: "app", Msg: "required"}
	want2 := `apps[1]: app: required`
	if got := e2.Error(); got != want2 {
		t.Errorf("Error() = %q, want %q", got, want2)
	}
}
