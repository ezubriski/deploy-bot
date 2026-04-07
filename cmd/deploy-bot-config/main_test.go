package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".deploy-bot.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun_Valid(t *testing.T) {
	path := writeTestFile(t, `{
		"apiVersion": "deploy-bot/v2",
		"apps": [
			{"app": "a", "environment": "dev", "kustomize_path": "p", "ecr_repo": "r"}
		]
	}`)
	code := run([]string{"--file", path})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRun_MissingVersionRejected(t *testing.T) {
	path := writeTestFile(t, `{
		"apps": [
			{"app": "a", "environment": "dev", "kustomize_path": "p", "ecr_repo": "r"}
		]
	}`)
	code := run([]string{"--file", path})
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (parse error)", code)
	}
}

func TestRun_ValidationErrors(t *testing.T) {
	path := writeTestFile(t, `{
		"apiVersion": "deploy-bot/v2",
		"apps": [
			{"app": "", "environment": "dev", "kustomize_path": "p", "ecr_repo": "r"}
		]
	}`)
	code := run([]string{"--file", path})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRun_UnknownVersion(t *testing.T) {
	path := writeTestFile(t, `{"apiVersion": "deploy-bot/v99", "apps": []}`)
	code := run([]string{"--file", path})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_FileNotFound(t *testing.T) {
	code := run([]string{"--file", "/nonexistent/path"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_InvalidJSON(t *testing.T) {
	path := writeTestFile(t, `not json`)
	code := run([]string{"--file", path})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_JSONFormat(t *testing.T) {
	path := writeTestFile(t, `{
		"apiVersion": "deploy-bot/v2",
		"apps": [
			{"app": "a", "environment": "dev", "kustomize_path": "p", "ecr_repo": "r"}
		]
	}`)
	code := run([]string{"--file", path, "--format", "json"})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRun_JSONFormatWithErrors(t *testing.T) {
	path := writeTestFile(t, `{
		"apiVersion": "deploy-bot/v2",
		"apps": [
			{"app": "", "environment": "dev", "kustomize_path": "p", "ecr_repo": "r"}
		]
	}`)
	code := run([]string{"--file", path, "--format", "json"})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRun_JSONFormatParseError(t *testing.T) {
	path := writeTestFile(t, `{"apiVersion": "deploy-bot/v99", "apps": []}`)
	code := run([]string{"--file", path, "--format", "json"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_ShortFlag(t *testing.T) {
	path := writeTestFile(t, `{"apiVersion":"deploy-bot/v2","apps":[{"app":"a","environment":"dev","kustomize_path":"p","ecr_repo":"r"}]}`)
	code := run([]string{"-f", path})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}
