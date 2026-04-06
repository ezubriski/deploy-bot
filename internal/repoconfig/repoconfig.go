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
	// VersionV1 is the first API version. All fields are required.
	VersionV1 = "deploy-bot/v1"

	// VersionV2 allows omitting app and kustomize_path when
	// enforce_repo_naming is enabled. The scanner derives them
	// from the repository name.
	VersionV2 = "deploy-bot/v2"

	// VersionPrefix is the namespace prefix for all API versions.
	VersionPrefix = "deploy-bot/"

	// CurrentVersion is the latest supported API version.
	CurrentVersion = VersionV2
)

// RepoConfigFile is the top-level envelope for a .deploy-bot.json file.
type RepoConfigFile struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Apps       []AppEntry `json:"apps"`
}

// AppEntry is a single app definition within a .deploy-bot.json file.
type AppEntry struct {
	App           string `json:"app"`
	Environment   string `json:"environment"`
	KustomizePath string `json:"kustomize_path"`
	ECRRepo       string `json:"ecr_repo"`
	TagPattern    string `json:"tag_pattern"`
	AutoDeploy    bool   `json:"auto_deploy,omitempty"`
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
	case VersionV1, VersionV2:
		// ok
	default:
		return nil, fmt.Errorf("unsupported apiVersion %q (this tool supports up to %s)", cfg.APIVersion, CurrentVersion)
	}

	return &cfg, nil
}

// ValidateOpts controls validation behavior.
type ValidateOpts struct {
	// RepoNaming indicates enforce_repo_naming is active. When true and the
	// config is v2, app and kustomize_path may be omitted (derived from
	// RepoName). If specified explicitly, they must match the derived values.
	RepoNaming bool
	// RepoName is the repository name used to derive app and kustomize_path
	// when RepoNaming is true (e.g. "my-service" from "org/my-service").
	RepoName string
	// Exempt indicates this repo is exempt from enforce_repo_naming.
	// v1 configs are accepted and no derivation is applied.
	Exempt bool
	// KustomizePathFn derives the kustomize_path from repo name and
	// environment. If nil, defaults to "<env>/<repo>/kustomization.yaml".
	KustomizePathFn func(repoName, env string) string
	// DefaultTagPattern is applied when an app entry omits tag_pattern.
	DefaultTagPattern string
}

// DerivedApp returns the app name derived from the repo name.
func (o ValidateOpts) DerivedApp(repoName string) string {
	return repoName
}

// DerivedKustomizePath returns the kustomize_path derived from the repo name
// and environment using KustomizePathFn, or the default convention.
func (o ValidateOpts) DerivedKustomizePath(repoName, env string) string {
	if o.KustomizePathFn != nil {
		return o.KustomizePathFn(repoName, env)
	}
	return env + "/" + repoName + "/kustomization.yaml"
}

// Validate checks all app entries and returns any validation errors.
// Valid entries can be identified by absence from the error list.
func Validate(cfg *RepoConfigFile) []ValidationError {
	return ValidateWithOpts(cfg, ValidateOpts{})
}

// ValidateWithOpts checks all app entries with the given options.
func ValidateWithOpts(cfg *RepoConfigFile, opts ValidateOpts) []ValidationError {
	// When enforcement is on and the repo is not exempt, v1 is rejected.
	if opts.RepoNaming && !opts.Exempt && cfg.APIVersion == VersionV1 {
		return []ValidationError{{
			Index: -1, Field: "apiVersion",
			Msg: "enforce_repo_naming requires apiVersion deploy-bot/v2. " +
				"Update your config or contact the operator to add this repo to exempt_repos.",
		}}
	}

	var errs []ValidationError
	seen := make(map[string]int) // "app\x00env" -> first index

	type kpathEntry struct {
		index int
		app   string
	}
	kpaths := make(map[string]kpathEntry) // kustomize_path -> first occurrence

	// Derive fields when enforcement is on, the config is v2, and the repo is not exempt.
	allowDerived := opts.RepoNaming && !opts.Exempt && cfg.APIVersion == VersionV2

	for i, e := range cfg.Apps {
		app := strings.TrimSpace(e.App)
		kpath := strings.TrimSpace(e.KustomizePath)
		env := strings.TrimSpace(e.Environment)

		// When repo naming is enforced and v2, derive or validate app and kustomize_path.
		if allowDerived {
			// Environment is needed to derive paths, check it first.
			if env == "" {
				errs = append(errs, ValidationError{Index: i, App: app, Field: "environment", Msg: "required"})
				continue
			}

			derivedApp := opts.DerivedApp(opts.RepoName)
			derivedPath := opts.DerivedKustomizePath(opts.RepoName, env)

			if app == "" {
				app = derivedApp
			} else if app != derivedApp {
				errs = append(errs, ValidationError{
					Index: i, App: app, Field: "app",
					Msg: fmt.Sprintf("must be %q when enforce_repo_naming is enabled (or omit to derive automatically)", derivedApp),
				})
				continue
			}
			if kpath == "" {
				kpath = derivedPath
			} else if kpath != derivedPath {
				errs = append(errs, ValidationError{
					Index: i, App: app, Field: "kustomize_path",
					Msg: fmt.Sprintf("must be %q when enforce_repo_naming is enabled (or omit to derive automatically)", derivedPath),
				})
				continue
			}
		} else {
			if app == "" {
				errs = append(errs, ValidationError{Index: i, Field: "app", Msg: "required"})
				continue
			}
			if env == "" {
				errs = append(errs, ValidationError{Index: i, App: app, Field: "environment", Msg: "required"})
				continue
			}
			if kpath == "" {
				errs = append(errs, ValidationError{Index: i, App: app, Field: "kustomize_path", Msg: "required"})
				continue
			}
		}

		if strings.TrimSpace(e.ECRRepo) == "" {
			errs = append(errs, ValidationError{Index: i, App: app, Field: "ecr_repo", Msg: "required"})
			continue
		}

		// Apply default tag pattern if omitted.
		tagPattern := e.TagPattern
		if tagPattern == "" && opts.DefaultTagPattern != "" {
			tagPattern = opts.DefaultTagPattern
		}
		if tagPattern != "" {
			if _, err := regexp.Compile(tagPattern); err != nil {
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

		if first, ok := kpaths[kpath]; ok {
			errs = append(errs, ValidationError{
				Index: i, App: app, Field: "kustomize_path",
				Msg: fmt.Sprintf("conflicts with apps[%d] (%s) — both target %s", first.index, first.app, kpath),
			})
		} else {
			kpaths[kpath] = kpathEntry{index: i, app: app}
		}
	}

	return errs
}

// ApplyDefaults fills in derived app, kustomize_path, and tag_pattern fields
// on entries where they are empty. Call this after validation to populate the
// config before converting to DiscoveredAppConfig.
func ApplyDefaults(cfg *RepoConfigFile, opts ValidateOpts) {
	for i := range cfg.Apps {
		e := &cfg.Apps[i]
		env := strings.TrimSpace(e.Environment)
		if strings.TrimSpace(e.App) == "" {
			e.App = opts.DerivedApp(opts.RepoName)
		}
		if strings.TrimSpace(e.KustomizePath) == "" && env != "" {
			e.KustomizePath = opts.DerivedKustomizePath(opts.RepoName, env)
		}
		if e.TagPattern == "" && opts.DefaultTagPattern != "" {
			e.TagPattern = opts.DefaultTagPattern
		}
	}
}

// ValidEntries returns the indices of entries that passed validation.
// If any error has Index < 0 (file-level error), no entries are valid.
func ValidEntries(cfg *RepoConfigFile, errs []ValidationError) []int {
	invalid := make(map[int]struct{}, len(errs))
	for _, e := range errs {
		if e.Index < 0 {
			return nil // file-level error invalidates all entries
		}
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
