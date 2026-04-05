package config

import (
	"fmt"
	"regexp"
	"time"
)

// ValidationError describes a single config validation failure.
type ValidationError struct {
	Section string `json:"section"`
	Field   string `json:"field"`
	Msg     string `json:"message"`
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s.%s: %s", e.Section, e.Field, e.Msg)
}

// ValidateConfig checks the config for structural errors that would cause
// runtime failures. It does not require network access.
func ValidateConfig(cfg *Config) []ValidationError {
	var errs []ValidationError

	add := func(section, field, msg string) {
		errs = append(errs, ValidationError{Section: section, Field: field, Msg: msg})
	}

	// --- github ---
	if cfg.GitHub.Org == "" {
		add("github", "org", "required")
	}
	if cfg.GitHub.Repo == "" {
		add("github", "repo", "required")
	}
	if cfg.GitHub.DeployerTeam == "" {
		add("github", "deployer_team", "required")
	}
	if cfg.GitHub.ApproverTeam == "" {
		add("github", "approver_team", "required")
	}
	if cfg.GitHub.RateLimitRetryWait != "" {
		if _, err := time.ParseDuration(cfg.GitHub.RateLimitRetryWait); err != nil {
			add("github", "rate_limit_retry_wait", fmt.Sprintf("invalid duration: %v", err))
		}
	}

	// --- slack ---
	if cfg.Slack.DeployChannel == "" {
		add("slack", "deploy_channel", "required")
	}
	if cfg.Slack.RateLimitRetryWait != "" {
		if _, err := time.ParseDuration(cfg.Slack.RateLimitRetryWait); err != nil {
			add("slack", "rate_limit_retry_wait", fmt.Sprintf("invalid duration: %v", err))
		}
	}

	// --- deployment ---
	if cfg.Deployment.StaleDuration == "" {
		add("deployment", "stale_duration", "required")
	} else if _, err := time.ParseDuration(cfg.Deployment.StaleDuration); err != nil {
		add("deployment", "stale_duration", fmt.Sprintf("invalid duration: %v", err))
	}
	if cfg.Deployment.LockTTL == "" {
		add("deployment", "lock_ttl", "required")
	} else if _, err := time.ParseDuration(cfg.Deployment.LockTTL); err != nil {
		add("deployment", "lock_ttl", fmt.Sprintf("invalid duration: %v", err))
	}
	switch cfg.Deployment.MergeMethod {
	case "", "squash", "merge", "rebase":
		// valid
	default:
		add("deployment", "merge_method", fmt.Sprintf("must be squash, merge, or rebase (got %q)", cfg.Deployment.MergeMethod))
	}
	if cfg.Deployment.ReconcileInterval != "" {
		if _, err := time.ParseDuration(cfg.Deployment.ReconcileInterval); err != nil {
			add("deployment", "reconcile_interval", fmt.Sprintf("invalid duration: %v", err))
		}
	}

	// --- aws ---
	if cfg.AWS.ECRRegion == "" {
		add("aws", "ecr_region", "required")
	}

	// --- ecr_events ---
	if cfg.ECREvents.PollInterval != "" {
		if _, err := time.ParseDuration(cfg.ECREvents.PollInterval); err != nil {
			add("ecr_events", "poll_interval", fmt.Sprintf("invalid duration: %v", err))
		}
	}

	// --- repo_discovery ---
	if cfg.RepoDiscovery.Enabled {
		if cfg.RepoDiscovery.PollInterval != "" {
			if _, err := time.ParseDuration(cfg.RepoDiscovery.PollInterval); err != nil {
				add("repo_discovery", "poll_interval", fmt.Sprintf("invalid duration: %v", err))
			}
		}
	}

	// --- apps ---
	if len(cfg.Apps) == 0 {
		add("apps", "", "at least one app is required")
	}

	seen := map[string]bool{}
	kustomizePaths := map[string]string{} // kustomize_path -> "app (env)"
	for i, app := range cfg.Apps {
		prefix := fmt.Sprintf("apps[%d]", i)
		if app.App == "" {
			add(prefix, "app", "required")
		}
		if app.Environment == "" {
			add(prefix, "environment", "required")
		}
		if app.KustomizePath == "" {
			add(prefix, "kustomize_path", "required")
		}
		if app.ECRRepo == "" {
			add(prefix, "ecr_repo", "required")
		}
		if app.TagPattern != "" {
			if _, err := regexp.Compile(app.TagPattern); err != nil {
				add(prefix, "tag_pattern", fmt.Sprintf("invalid regex: %v", err))
			}
		}

		key := app.App + "/" + app.Environment
		if app.App != "" && app.Environment != "" {
			if seen[key] {
				add(prefix, "app", fmt.Sprintf("duplicate app+environment: %s", key))
			}
			seen[key] = true
		}

		if app.KustomizePath != "" {
			if other, ok := kustomizePaths[app.KustomizePath]; ok {
				add(prefix, "kustomize_path", fmt.Sprintf("conflicts with %s — both target %s", other, app.KustomizePath))
			} else {
				label := app.App
				if app.Environment != "" {
					label += " (" + app.Environment + ")"
				}
				kustomizePaths[app.KustomizePath] = label
			}
		}
	}

	return errs
}
