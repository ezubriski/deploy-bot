// Package repoconfig provides parsing and validation for repo-sourced
// .deploy-bot.json configuration files. It has no dependencies outside the
// standard library so it can be used by lightweight tooling.
package repoconfig

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	// VersionV1 is the first (and current) API version.
	VersionV1 = "deploy-bot/v1"

	// VersionPrefix is the namespace prefix for all API versions.
	VersionPrefix = "deploy-bot/"

	// CurrentVersion is the latest supported API version.
	CurrentVersion = VersionV1
)

// RepoConfigFile is the top-level envelope for a .deploy-bot.json file.
type RepoConfigFile struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Apps       []AppEntry `json:"apps"`
}

// AppEntry is a single app definition within a .deploy-bot.json file.
type AppEntry struct {
	App                     string `json:"app"`
	Environment             string `json:"environment"`
	KustomizePath           string `json:"kustomize_path"`
	ECRRepo                 string `json:"ecr_repo"`
	TagPattern              string `json:"tag_pattern"`
	AutoDeploy              bool   `json:"auto_deploy,omitempty"`
	AutoDeployApproverGroup string `json:"auto_deploy_approver_group,omitempty"`
}

// ValidationError describes a validation failure for a single app entry.
type ValidationError struct {
	Index int    `json:"index"`
	App   string `json:"app,omitempty"`
	Field string `json:"field"`
	Msg   string `json:"message"`
}

func (e *ValidationError) Error() string {
	if e.App != "" {
		return fmt.Sprintf("apps[%d] (%s): %s: %s", e.Index, e.App, e.Field, e.Msg)
	}
	return fmt.Sprintf("apps[%d]: %s: %s", e.Index, e.Field, e.Msg)
}

// Parse unmarshals a .deploy-bot.json file and normalises the API version.
// Files without an apiVersion field are treated as deploy-bot/v1.
// Unknown or malformed versions return a hard error.
func Parse(data []byte) (*RepoConfigFile, error) {
	var cfg RepoConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}

	if cfg.APIVersion == "" {
		cfg.APIVersion = VersionV1
	}

	if !strings.HasPrefix(cfg.APIVersion, VersionPrefix) {
		return nil, fmt.Errorf("unsupported apiVersion %q (expected prefix %q)", cfg.APIVersion, VersionPrefix)
	}

	switch cfg.APIVersion {
	case VersionV1:
		// ok
	default:
		return nil, fmt.Errorf("unsupported apiVersion %q (this tool supports up to %s)", cfg.APIVersion, CurrentVersion)
	}

	return &cfg, nil
}

// Validate checks all app entries and returns any validation errors.
// Valid entries can be identified by absence from the error list.
func Validate(cfg *RepoConfigFile) []ValidationError {
	var errs []ValidationError
	seen := make(map[string]int) // "app\x00env" -> first index

	for i, e := range cfg.Apps {
		app := strings.TrimSpace(e.App)
		if app == "" {
			errs = append(errs, ValidationError{Index: i, Field: "app", Msg: "required"})
			continue
		}
		env := strings.TrimSpace(e.Environment)
		if env == "" {
			errs = append(errs, ValidationError{Index: i, App: app, Field: "environment", Msg: "required"})
			continue
		}
		if strings.TrimSpace(e.KustomizePath) == "" {
			errs = append(errs, ValidationError{Index: i, App: app, Field: "kustomize_path", Msg: "required"})
			continue
		}
		if strings.TrimSpace(e.ECRRepo) == "" {
			errs = append(errs, ValidationError{Index: i, App: app, Field: "ecr_repo", Msg: "required"})
			continue
		}
		if e.TagPattern != "" {
			if _, err := regexp.Compile(e.TagPattern); err != nil {
				errs = append(errs, ValidationError{Index: i, App: app, Field: "tag_pattern", Msg: fmt.Sprintf("invalid regex: %v", err)})
				continue
			}
		}

		key := app + "\x00" + env
		if first, ok := seen[key]; ok {
			errs = append(errs, ValidationError{
				Index: i, App: app, Field: "app+environment",
				Msg: fmt.Sprintf("duplicate of apps[%d]", first),
			})
			continue
		}
		seen[key] = i
	}

	return errs
}

// ValidEntries returns the indices of entries that passed validation.
func ValidEntries(cfg *RepoConfigFile, errs []ValidationError) []int {
	invalid := make(map[int]struct{}, len(errs))
	for _, e := range errs {
		invalid[e.Index] = struct{}{}
	}
	var valid []int
	for i := range cfg.Apps {
		if _, ok := invalid[i]; !ok {
			valid = append(valid, i)
		}
	}
	return valid
}
