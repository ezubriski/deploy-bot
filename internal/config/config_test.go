package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestIsChannelAllowed(t *testing.T) {
	t.Run("empty allowlist permits all channels", func(t *testing.T) {
		cfg := &SlackConfig{}
		if !cfg.IsChannelAllowed("C12345") {
			t.Error("expected all channels allowed when allowlist is empty")
		}
	})

	t.Run("listed channel is allowed", func(t *testing.T) {
		cfg := &SlackConfig{AllowedChannels: []string{"C11111", "C22222"}}
		if !cfg.IsChannelAllowed("C11111") {
			t.Error("expected C11111 to be allowed")
		}
	})

	t.Run("unlisted channel is rejected", func(t *testing.T) {
		cfg := &SlackConfig{AllowedChannels: []string{"C11111"}}
		if cfg.IsChannelAllowed("C99999") {
			t.Error("expected C99999 to be rejected")
		}
	})
}

func TestDeployLabel(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		cfg := &Config{}
		if got := cfg.DeployLabel(); got != "deploy-bot" {
			t.Errorf("DeployLabel() = %q, want %q", got, "deploy-bot")
		}
	})

	t.Run("uses configured value", func(t *testing.T) {
		cfg := &Config{Deployment: DeploymentConfig{Label: "custom-label"}}
		if got := cfg.DeployLabel(); got != "custom-label" {
			t.Errorf("DeployLabel() = %q, want %q", got, "custom-label")
		}
	})
}

func TestPendingLabel(t *testing.T) {
	cfg := &Config{Deployment: DeploymentConfig{Label: "deploy-bot"}}
	if got := cfg.PendingLabel(); got != "deploy-bot/pending" {
		t.Errorf("PendingLabel() = %q, want %q", got, "deploy-bot/pending")
	}
}

func TestSecretsValidate(t *testing.T) {
	valid := Secrets{
		SlackBotToken: "xoxb-valid-token",
		SlackAppToken: "xapp-valid-token",
		GitHubToken:   "github_pat_abc123",
		RedisAddr:     "redis:6379",
	}

	t.Run("valid secrets pass", func(t *testing.T) {
		if err := valid.Validate(); err != nil {
			t.Errorf("expected no error, got: %v", err)
		}
	})

	t.Run("empty slack_bot_token", func(t *testing.T) {
		s := valid
		s.SlackBotToken = ""
		err := s.Validate()
		if err == nil {
			t.Fatal("expected error for empty slack_bot_token")
		}
		if !strings.Contains(err.Error(), "slack_bot_token is empty") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("wrong slack_bot_token prefix", func(t *testing.T) {
		s := valid
		s.SlackBotToken = "xoxp-wrong-type"
		err := s.Validate()
		if err == nil {
			t.Fatal("expected error for wrong bot token prefix")
		}
		if !strings.Contains(err.Error(), "xoxb-") {
			t.Errorf("error should mention expected prefix xoxb-, got: %v", err)
		}
	})

	t.Run("empty slack_app_token is valid", func(t *testing.T) {
		s := valid
		s.SlackAppToken = ""
		if err := s.Validate(); err != nil {
			t.Errorf("slack_app_token is optional, got error: %v", err)
		}
	})

	t.Run("wrong slack_app_token prefix", func(t *testing.T) {
		s := valid
		s.SlackAppToken = "xoxb-wrong-type"
		err := s.Validate()
		if err == nil {
			t.Fatal("expected error for wrong app token prefix")
		}
		if !strings.Contains(err.Error(), "xapp-") {
			t.Errorf("error should mention expected prefix xapp-, got: %v", err)
		}
	})

	t.Run("empty github_token", func(t *testing.T) {
		s := valid
		s.GitHubToken = ""
		err := s.Validate()
		if err == nil {
			t.Fatal("expected error for empty github_token")
		}
		if !strings.Contains(err.Error(), "github_token is empty") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("empty redis_addr", func(t *testing.T) {
		s := valid
		s.RedisAddr = ""
		err := s.Validate()
		if err == nil {
			t.Fatal("expected error for empty redis_addr")
		}
		if !strings.Contains(err.Error(), "redis_addr is empty") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("multiple errors reported together", func(t *testing.T) {
		s := Secrets{} // all empty
		err := s.Validate()
		if err == nil {
			t.Fatal("expected errors for all-empty secrets")
		}
		for _, want := range []string{"slack_bot_token", "github_token", "redis_addr"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("expected error to mention %q, got: %v", want, err)
			}
		}
	})
}

func TestCompiledTagPattern(t *testing.T) {
	t.Run("matches valid semver tag", func(t *testing.T) {
		a := &AppConfig{TagPattern: `^v[0-9]+\.[0-9]+\.[0-9]+$`}
		re := a.CompiledTagPattern()
		if re == nil {
			t.Fatal("expected non-nil regexp")
		}
		if !re.MatchString("v1.2.3") {
			t.Error("expected v1.2.3 to match")
		}
		if re.MatchString("v1.2") {
			t.Error("expected v1.2 not to match")
		}
	})

	t.Run("cached on repeated calls", func(t *testing.T) {
		a := &AppConfig{TagPattern: `^v\d+$`}
		r1 := a.CompiledTagPattern()
		r2 := a.CompiledTagPattern()
		if r1 != r2 {
			t.Error("expected same *regexp.Regexp pointer on second call")
		}
	})

	t.Run("panics on invalid pattern", func(t *testing.T) {
		a := &AppConfig{TagPattern: `[invalid`}
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for invalid regex pattern")
			}
		}()
		a.CompiledTagPattern()
	})
}

func TestRateLimitConfig(t *testing.T) {
	t.Run("defaults when zero", func(t *testing.T) {
		cfg := &GitHubConfig{}
		maxRetries, retryWait := cfg.RateLimitConfig()
		if maxRetries != 3 {
			t.Errorf("maxRetries = %d, want 3", maxRetries)
		}
		if retryWait != 2*time.Minute {
			t.Errorf("retryWait = %v, want 2m", retryWait)
		}
	})

	t.Run("custom values parsed", func(t *testing.T) {
		cfg := &GitHubConfig{RateLimitMaxRetries: 5, RateLimitRetryWait: "90s"}
		maxRetries, retryWait := cfg.RateLimitConfig()
		if maxRetries != 5 {
			t.Errorf("maxRetries = %d, want 5", maxRetries)
		}
		if retryWait != 90*time.Second {
			t.Errorf("retryWait = %v, want 90s", retryWait)
		}
	})

	t.Run("invalid duration falls back to default", func(t *testing.T) {
		cfg := &GitHubConfig{RateLimitMaxRetries: 2, RateLimitRetryWait: "not-a-duration"}
		_, retryWait := cfg.RateLimitConfig()
		if retryWait != 2*time.Minute {
			t.Errorf("retryWait = %v, want 2m default on bad input", retryWait)
		}
	})

	t.Run("zero duration falls back to default", func(t *testing.T) {
		cfg := &GitHubConfig{RateLimitRetryWait: "0s"}
		_, retryWait := cfg.RateLimitConfig()
		if retryWait != 2*time.Minute {
			t.Errorf("retryWait = %v, want 2m default for zero duration", retryWait)
		}
	})
}

func TestSlackRateLimitConfig(t *testing.T) {
	t.Run("defaults when zero", func(t *testing.T) {
		cfg := &SlackConfig{}
		maxRetries, retryWait := cfg.RateLimitConfig()
		if maxRetries != 3 {
			t.Errorf("maxRetries = %d, want 3", maxRetries)
		}
		if retryWait != 30*time.Second {
			t.Errorf("retryWait = %v, want 30s", retryWait)
		}
	})

	t.Run("custom values parsed", func(t *testing.T) {
		cfg := &SlackConfig{RateLimitMaxRetries: 5, RateLimitRetryWait: "60s"}
		maxRetries, retryWait := cfg.RateLimitConfig()
		if maxRetries != 5 {
			t.Errorf("maxRetries = %d, want 5", maxRetries)
		}
		if retryWait != 60*time.Second {
			t.Errorf("retryWait = %v, want 60s", retryWait)
		}
	})

	t.Run("invalid duration falls back to default", func(t *testing.T) {
		cfg := &SlackConfig{RateLimitRetryWait: "not-a-duration"}
		_, retryWait := cfg.RateLimitConfig()
		if retryWait != 30*time.Second {
			t.Errorf("retryWait = %v, want 30s default on bad input", retryWait)
		}
	})
}

func TestTokenPrefix(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"xoxb-abc-123", "xoxb-"},
		{"xapp-abc", "xapp-"},
		{"nohyphen", "nohyphen"},
		{"", ""},
	}
	for _, tc := range cases {
		got := tokenPrefix(tc.input)
		if got != tc.want {
			t.Errorf("tokenPrefix(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestMergeApps_NoDuplicates(t *testing.T) {
	primary := []AppConfig{
		{App: "a", Environment: "dev"},
		{App: "b", Environment: "prod"},
	}
	discovered := []DiscoveredAppConfig{
		{AppConfig: AppConfig{App: "c", Environment: "dev"}, SourceRepo: "org/c"},
		{AppConfig: AppConfig{App: "d", Environment: "prod"}, SourceRepo: "org/d"},
	}

	merged := MergeApps(primary, discovered)
	if len(merged) != 4 {
		t.Fatalf("expected 4 apps, got %d", len(merged))
	}
}

func TestMergeApps_OperatorWins(t *testing.T) {
	primary := []AppConfig{
		{App: "myapp", Environment: "prod", KustomizePath: "operator-path"},
	}
	discovered := []DiscoveredAppConfig{
		{AppConfig: AppConfig{App: "myapp", Environment: "prod", KustomizePath: "repo-path"}, SourceRepo: "org/myapp"},
		{AppConfig: AppConfig{App: "myapp", Environment: "dev", KustomizePath: "repo-path-dev"}, SourceRepo: "org/myapp"},
	}

	merged := MergeApps(primary, discovered)
	if len(merged) != 2 {
		t.Fatalf("expected 2 apps, got %d", len(merged))
	}
	// Operator's entry should be kept for prod.
	if merged[0].KustomizePath != "operator-path" {
		t.Errorf("expected operator path for prod, got %q", merged[0].KustomizePath)
	}
	// Dev should be added from discovered.
	if merged[1].KustomizePath != "repo-path-dev" {
		t.Errorf("expected repo path for dev, got %q", merged[1].KustomizePath)
	}
}

func TestMergeApps_DeduplicatesDiscovered(t *testing.T) {
	primary := []AppConfig{}
	discovered := []DiscoveredAppConfig{
		{AppConfig: AppConfig{App: "myapp", Environment: "dev"}, SourceRepo: "org/a"},
		{AppConfig: AppConfig{App: "myapp", Environment: "dev"}, SourceRepo: "org/b"},
	}

	merged := MergeApps(primary, discovered)
	if len(merged) != 1 {
		t.Fatalf("expected 1 app (first wins among discovered), got %d", len(merged))
	}
}

func TestLoad_InvalidTagPattern(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	writeFile(t, path, `{"apps":[{"app":"a","environment":"dev","tag_pattern":"[invalid"}]}`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid tag_pattern")
	}
	if !strings.Contains(err.Error(), "invalid tag_pattern") {
		t.Errorf("error = %v, want mention of invalid tag_pattern", err)
	}
}

func TestLoad_ValidTagPattern(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	writeFile(t, path, `{"apps":[{"app":"a","environment":"dev","tag_pattern":"^v\\d+$"}]}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(cfg.Apps))
	}
}

func TestLoadWithDiscovered_NoFile(t *testing.T) {
	dir := t.TempDir()
	primaryPath := dir + "/config.json"
	writeFile(t, primaryPath, `{"deployment":{"merge_method":"squash"},"apps":[{"app":"a","environment":"dev"}]}`)

	cfg, err := LoadWithDiscovered(primaryPath, dir+"/nonexistent.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(cfg.Apps))
	}
}

func TestLoadWithDiscovered_MergesApps(t *testing.T) {
	dir := t.TempDir()
	primaryPath := dir + "/config.json"
	discoveredPath := dir + "/discovered.json"

	writeFile(t, primaryPath, `{"deployment":{"merge_method":"squash"},"apps":[{"app":"a","environment":"dev"}]}`)
	writeFile(t, discoveredPath, `{"apps":[{"app":"b","environment":"prod","kustomize_path":"p","ecr_repo":"r","_source_repo":"org/b"}]}`)

	cfg, err := LoadWithDiscovered(primaryPath, discoveredPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Apps) != 2 {
		t.Fatalf("expected 2 apps, got %d", len(cfg.Apps))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSecretsFromFile(t *testing.T) {
	t.Run("valid file", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/secrets.json"
		writeFile(t, path, `{
			"slack_bot_token": "xoxb-test",
			"slack_app_token": "xapp-test",
			"github_token": "ghp_test",
			"redis_addr": "localhost:6379"
		}`)

		s, err := LoadSecretsFromFile(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s.SlackBotToken != "xoxb-test" {
			t.Errorf("SlackBotToken = %q", s.SlackBotToken)
		}
		if s.RedisAddr != "localhost:6379" {
			t.Errorf("RedisAddr = %q", s.RedisAddr)
		}
	})

	t.Run("file not found", func(t *testing.T) {
		_, err := LoadSecretsFromFile("/nonexistent/secrets.json")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/bad.json"
		writeFile(t, path, `not json`)
		_, err := LoadSecretsFromFile(path)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestRepoDiscoveryDefaults(t *testing.T) {
	rd := &RepoDiscoveryConfig{}
	if rd.PollIntervalDuration() != 5*time.Minute {
		t.Errorf("PollIntervalDuration = %v, want 5m", rd.PollIntervalDuration())
	}
	if rd.ConfigFileName() != ".deploy-bot.json" {
		t.Errorf("ConfigFileName = %q, want .deploy-bot.json", rd.ConfigFileName())
	}
	if rd.DiscoveredFilePath() != "/etc/deploy-bot/discovered.json" {
		t.Errorf("DiscoveredFilePath = %q", rd.DiscoveredFilePath())
	}
	if rd.ConfigMapTargetName() != "deploy-bot-discovered" {
		t.Errorf("ConfigMapTargetName = %q", rd.ConfigMapTargetName())
	}
	if rd.RateLimitFloorValue() != 500 {
		t.Errorf("RateLimitFloorValue = %d, want 500", rd.RateLimitFloorValue())
	}
}

func TestAppByECRRepo(t *testing.T) {
	cfg := &Config{
		Apps: []AppConfig{
			{App: "myapp", Environment: "dev", ECRRepo: "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp"},
		},
	}

	t.Run("suffix match", func(t *testing.T) {
		app, ok := cfg.AppByECRRepo("myapp")
		if !ok || app.App != "myapp" {
			t.Errorf("expected match, got ok=%v app=%v", ok, app)
		}
	})

	t.Run("exact match", func(t *testing.T) {
		app, ok := cfg.AppByECRRepo("123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp")
		if !ok || app.App != "myapp" {
			t.Errorf("expected match, got ok=%v app=%v", ok, app)
		}
	})

	t.Run("no match", func(t *testing.T) {
		_, ok := cfg.AppByECRRepo("other-app")
		if ok {
			t.Error("expected no match")
		}
	})
}

func TestIsProd(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"prod", true},
		{"production", true},
		{"PROD", true},
		{"Production", true},
		{"dev", false},
		{"staging", false},
	}
	for _, tt := range tests {
		a := &AppConfig{Environment: tt.env}
		if got := a.IsProd(); got != tt.want {
			t.Errorf("IsProd(%q) = %v, want %v", tt.env, got, tt.want)
		}
	}
}

func TestEffectiveAutoDeploy(t *testing.T) {
	t.Run("auto_deploy false", func(t *testing.T) {
		a := &AppConfig{AutoDeploy: false}
		if a.EffectiveAutoDeploy(true) {
			t.Error("expected false when auto_deploy is false")
		}
	})

	t.Run("prod blocked by guard", func(t *testing.T) {
		a := &AppConfig{AutoDeploy: true, Environment: "prod"}
		if a.EffectiveAutoDeploy(false) {
			t.Error("expected false for prod when guard is off")
		}
	})

	t.Run("prod allowed by guard", func(t *testing.T) {
		a := &AppConfig{AutoDeploy: true, Environment: "prod"}
		if !a.EffectiveAutoDeploy(true) {
			t.Error("expected true for prod when guard is on")
		}
	})

	t.Run("non-prod always allowed", func(t *testing.T) {
		a := &AppConfig{AutoDeploy: true, Environment: "dev"}
		if !a.EffectiveAutoDeploy(false) {
			t.Error("expected true for non-prod even when guard is off")
		}
	})
}
