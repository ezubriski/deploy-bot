package github

import (
	"testing"
)

func TestParsePRMeta(t *testing.T) {
	t.Run("valid metadata", func(t *testing.T) {
		body := "Deploy PR body\n<!-- deploy-bot-meta: {\"requester_id\":\"U123ABC\",\"app\":\"myapp\",\"tag\":\"v1.2.3\"} -->"
		meta, ok := ParsePRMeta(body)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if meta.RequesterSlackID != "U123ABC" {
			t.Errorf("RequesterSlackID = %q, want %q", meta.RequesterSlackID, "U123ABC")
		}
		if meta.App != "myapp" {
			t.Errorf("App = %q, want %q", meta.App, "myapp")
		}
		if meta.Tag != "v1.2.3" {
			t.Errorf("Tag = %q, want %q", meta.Tag, "v1.2.3")
		}
	})

	t.Run("no metadata comment", func(t *testing.T) {
		_, ok := ParsePRMeta("Just a plain PR body with no metadata.")
		if ok {
			t.Fatal("expected ok=false for body with no metadata comment")
		}
	})

	t.Run("malformed JSON in comment", func(t *testing.T) {
		_, ok := ParsePRMeta("<!-- deploy-bot-meta: {not valid json} -->")
		if ok {
			t.Fatal("expected ok=false for malformed JSON")
		}
	})

	t.Run("empty body", func(t *testing.T) {
		_, ok := ParsePRMeta("")
		if ok {
			t.Fatal("expected ok=false for empty body")
		}
	})
}
