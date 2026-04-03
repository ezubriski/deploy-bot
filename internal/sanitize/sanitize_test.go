package sanitize

import (
	"strings"
	"testing"
)

func TestTagIsSafe(t *testing.T) {
	safe := []string{
		"v1.0.0",
		"v1.2.3-rc.1",
		"sha-abc1234",
		"20240101.1",
		"v1.0.0+build.123",
		"release_v2",
		"a",
	}
	for _, tag := range safe {
		if !TagIsSafe(tag) {
			t.Errorf("TagIsSafe(%q) = false, want true", tag)
		}
	}

	unsafe := []string{
		"",
		".starts-with-dot",
		"-starts-with-hyphen",
		"has spaces",
		"has\nnewline",
		"has\ttab",
		"yaml: injection",
		`yaml" injection`,
		"has#comment",
		"<script>",
		"<!channel>",
		"@mention",
		"has/slash",
		strings.Repeat("a", 257),
	}
	for _, tag := range unsafe {
		if TagIsSafe(tag) {
			t.Errorf("TagIsSafe(%q) = true, want false", tag)
		}
	}
}

func TestBranchName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"v1.0.0", "v1.0.0"},
		{"feature/branch", "feature-branch"},
		{"v1:latest", "v1-latest"},
		{"tag with spaces", "tag-with-spaces"},
		{"a~~b", "a-b"},
		{"a..b", "a-b"},
		{"--leading--", "leading"},
		{"", "invalid"},
	}
	for _, tt := range tests {
		got := BranchName(tt.input)
		if got != tt.want {
			t.Errorf("BranchName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSlackText(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"plain text", "plain text"},
		{"<@U12345>", "&lt;@U12345&gt;"},
		{"<!channel>", "&lt;!channel&gt;"},
		{"<http://evil.com|Click>", "&lt;http://evil.com|Click&gt;"},
		{"A & B", "A &amp; B"},
	}
	for _, tt := range tests {
		got := SlackText(tt.input, 0)
		if got != tt.want {
			t.Errorf("SlackText(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}

	t.Run("truncation", func(t *testing.T) {
		got := SlackText("abcdefghij", 5)
		if got != "abcde..." {
			t.Errorf("SlackText truncation = %q, want %q", got, "abcde...")
		}
	})
}

func TestGitHubMarkdown(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"plain text", "plain text"},
		{"@user", "\\@user"},
		{"#123", "\\#123"},
		{"[link](url)", "\\[link\\](url)"},
		{"<script>", "&lt;script&gt;"},
	}
	for _, tt := range tests {
		got := GitHubMarkdown(tt.input)
		if got != tt.want {
			t.Errorf("GitHubMarkdown(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
