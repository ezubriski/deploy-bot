package config

import (
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

	t.Run("empty slack_app_token", func(t *testing.T) {
		s := valid
		s.SlackAppToken = ""
		err := s.Validate()
		if err == nil {
			t.Fatal("expected error for empty slack_app_token")
		}
		if !strings.Contains(err.Error(), "slack_app_token is empty") {
			t.Errorf("unexpected error message: %v", err)
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
		for _, want := range []string{"slack_bot_token", "slack_app_token", "github_token", "redis_addr"} {
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
