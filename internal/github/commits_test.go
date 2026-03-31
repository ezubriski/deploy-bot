package github

import (
	"testing"
)

func TestDeployCommitRe(t *testing.T) {
	cases := []struct {
		msg       string
		wantApp   string
		wantTag   string
		wantMatch bool
	}{
		{"deploy(myapp): update image tag to v1.2.3", "myapp", "v1.2.3", true},
		{"deploy(my-app): update image tag to v1.2.3", "my-app", "v1.2.3", true},
		{"deploy(my-app): update image tag to sha-abc123", "my-app", "sha-abc123", true},
		{"deploy(ns/app): update image tag to v1.0.0", "ns/app", "v1.0.0", true},
		// not a deploy commit
		{"chore: bump version", "", "", false},
		{"deploy(myapp): wrong suffix", "", "", false},
		// anchored — leading content must not match
		{"prefix deploy(myapp): update image tag to v1.2.3", "", "", false},
	}
	for _, tc := range cases {
		m := deployCommitRe.FindStringSubmatch(tc.msg)
		matched := m != nil
		if matched != tc.wantMatch {
			t.Errorf("msg=%q: matched=%v, want %v", tc.msg, matched, tc.wantMatch)
			continue
		}
		if tc.wantMatch {
			if m[1] != tc.wantApp {
				t.Errorf("msg=%q: app=%q, want %q", tc.msg, m[1], tc.wantApp)
			}
			if m[2] != tc.wantTag {
				t.Errorf("msg=%q: tag=%q, want %q", tc.msg, m[2], tc.wantTag)
			}
		}
	}
}
