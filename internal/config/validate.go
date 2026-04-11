package config

import (
	"fmt"
	"regexp"
	"strings"
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
	if cfg.GitHub.RateLimitRetryWait != "" {
		if _, err := time.ParseDuration(cfg.GitHub.RateLimitRetryWait); err != nil {
			add("github", "rate_limit_retry_wait", fmt.Sprintf("invalid duration: %v", err))
		}
	}

	// --- authorization ---
	if len(cfg.Authorization) == 0 {
		add("authorization", "", "at least one entry required")
	}
	ghTeams, ghUsers, _, slackEmails, parseErr := ParseAuthValues(cfg.Authorization)
	if parseErr != nil {
		add("authorization", "", parseErr.Error())
	}
	if (len(ghTeams) > 0 || len(ghUsers) > 0) && cfg.GitHub.Org == "" {
		add("authorization", "", "github_team/github_user entries require github.org to be set")
	}
	for i, email := range slackEmails {
		if !strings.Contains(email, "@") {
			add("authorization", fmt.Sprintf("slack_emails[%d]", i), fmt.Sprintf("expected email address, got %q", email))
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

	// --- ecr_auto_deploy ---
	if cfg.ECRAutoDeploy.PollInterval != "" {
		if _, err := time.ParseDuration(cfg.ECRAutoDeploy.PollInterval); err != nil {
			add("ecr_auto_deploy", "poll_interval", fmt.Sprintf("invalid duration: %v", err))
		}
	}

	// --- postgres ---
	// Required in 2.0+, no enabled flag. See PostgresConfig doc comment
	// for rationale and docs/postgres-setup.md for operator guidance.
	if strings.TrimSpace(cfg.Postgres.Host) == "" {
		add("postgres", "host", "required")
	}
	if strings.TrimSpace(cfg.Postgres.Database) == "" {
		add("postgres", "database", "required")
	}
	if strings.TrimSpace(cfg.Postgres.User) == "" {
		add("postgres", "user", "required")
	}
	if cfg.Postgres.Port < 0 {
		add("postgres", "port", fmt.Sprintf("must be >= 0, got %d", cfg.Postgres.Port))
	}
	if cfg.Postgres.RetentionHistory != "" {
		d, err := time.ParseDuration(cfg.Postgres.RetentionHistory)
		switch {
		case err != nil:
			add("postgres", "retention_history", fmt.Sprintf("invalid duration: %v", err))
		case d < minPostgresRetentionHistory:
			add("postgres", "retention_history", fmt.Sprintf(
				"must be >= %s (%d days) for audit compliance — audits don't happen immediately at period end and anything shorter risks deleting data an auditor is about to ask for",
				minPostgresRetentionHistory, int(minPostgresRetentionHistory.Hours()/24),
			))
		}
	}
	if cfg.Postgres.RetentionPendingTerminal != "" {
		d, err := time.ParseDuration(cfg.Postgres.RetentionPendingTerminal)
		switch {
		case err != nil:
			add("postgres", "retention_pending_terminal", fmt.Sprintf("invalid duration: %v", err))
		case d < minPostgresRetentionPendingTerminal:
			add("postgres", "retention_pending_terminal", fmt.Sprintf("must be >= %s, got %s", minPostgresRetentionPendingTerminal, d))
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

		if app.App != "" && app.Environment != "" {
			key := app.FullName()
			if seen[key] {
				add(prefix, "app", fmt.Sprintf("duplicate app+environment: %s", key))
			}
			seen[key] = true
		}

		if app.KustomizePath != "" {
			if other, ok := kustomizePaths[app.KustomizePath]; ok {
				add(prefix, "kustomize_path", fmt.Sprintf("conflicts with %s — both target %s", other, app.KustomizePath))
			} else {
				label := app.FullName()
				kustomizePaths[app.KustomizePath] = label
			}
		}
	}

	return errs
}
