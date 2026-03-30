package config

import (
	"strings"
	"testing"
)

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
