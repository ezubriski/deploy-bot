// Package sanitize provides input sanitization for user-provided text that
// flows into Slack messages, GitHub comments/PRs, YAML files, and git refs.
package sanitize

import (
	"regexp"
	"strings"
)

// tagSafe matches characters safe for use in tags across all contexts:
// git branch names, YAML values, Slack messages, and GitHub markdown.
// Allows alphanumeric, dots, hyphens, underscores, and plus signs.
var tagSafe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*$`)

// TagIsSafe returns true if the tag contains only characters that are safe
// for use in git refs, YAML values, and display contexts. This is applied
// in addition to the per-app tag_pattern regex.
func TagIsSafe(tag string) bool {
	if tag == "" || len(tag) > 256 {
		return false
	}
	return tagSafe.MatchString(tag)
}

// BranchName makes a tag safe for use as part of a git branch name.
// Replaces characters not allowed in git refs, collapses runs of hyphens,
// and trims leading/trailing hyphens.
func BranchName(tag string) string {
	// Replace known problematic characters with hyphens.
	r := strings.NewReplacer(
		"/", "-",
		":", "-",
		"+", "-",
		" ", "-",
		"~", "-",
		"^", "-",
		"*", "-",
		"?", "-",
		"[", "-",
		"]", "-",
		"\\", "-",
		"..", "-",
	)
	s := r.Replace(tag)
	// Collapse consecutive hyphens.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-.")
	if s == "" {
		s = "invalid"
	}
	return s
}

// slackMrkdwnReplacer escapes characters that have special meaning in Slack
// mrkdwn formatting. Applied to user-provided text before interpolation into
// mrkdwn blocks.
var slackMrkdwnReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)

// SlackText escapes user-provided text for safe inclusion in Slack mrkdwn
// messages. Prevents injection of @mentions, #channels, links, and
// formatting. Truncates to maxLen if provided (0 = no limit).
func SlackText(text string, maxLen int) string {
	s := slackMrkdwnReplacer.Replace(text)
	if maxLen > 0 && len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}

// GitHubMarkdown escapes user-provided text for safe inclusion in GitHub
// markdown (PR bodies, comments). Prevents injection of formatting,
// mentions, and links.
func GitHubMarkdown(text string) string {
	// Escape characters with special meaning in GitHub markdown.
	r := strings.NewReplacer(
		"@", "\\@",
		"#", "\\#",
		"[", "\\[",
		"]", "\\]",
		"<", "&lt;",
		">", "&gt;",
	)
	s := r.Replace(text)
	// Truncate very long inputs.
	if len(s) > 1000 {
		s = s[:1000] + "..."
	}
	return s
}
