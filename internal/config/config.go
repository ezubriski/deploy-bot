package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type Config struct {
	GitHub        GitHubConfig         `json:"github"`
	Slack         SlackConfig          `json:"slack"`
	Authorization []AuthorizationEntry `json:"authorization"`
	// IdentityOverrides maps Slack user IDs to GitHub logins for users whose
	// GitHub email is private. Takes precedence over the Slack email → GitHub
	// search lookup.
	IdentityOverrides map[string]string   `json:"identity_overrides,omitempty"`
	Deployment        DeploymentConfig    `json:"deployment"`
	AWS               AWSConfig           `json:"aws"`
	ECRAutoDeploy     ECRAutoDeployConfig `json:"ecr_auto_deploy,omitempty"`
	// ArgoCDNotifications gates the optional inbound webhook endpoint that
	// receives lifecycle notifications from the argocd-notifications
	// controller (sync-succeeded, sync-failed, health-degraded). When
	// disabled, the receiver mounts no handler and consumes no Redis
	// stream, so existing deployments are unaffected.
	ArgoCDNotifications ArgoCDNotificationsConfig `json:"argocd_notifications,omitempty"`
	// Postgres configures the durable store for deploy history and
	// in-flight pending deploys. Required on 2.0+ — the bot and
	// receiver fail to start if this block is missing, invalid, or
	// Postgres is unreachable. Changes to this section do NOT take
	// effect on config hot-reload; the config watcher logs a warning
	// if a diff is detected here and operators must restart the
	// process. See docs/postgres-setup.md for deployment guidance.
	Postgres      PostgresConfig      `json:"postgres"`
	RepoDiscovery RepoDiscoveryConfig `json:"repo_discovery,omitempty"`
	Apps          []AppConfig         `json:"apps"`
	// LogLevel sets the minimum severity emitted by zap. Valid values are
	// "debug", "info", "warn", "error". Defaults to "info" when empty. The
	// LOG_LEVEL environment variable, if set on the bot/receiver process,
	// overrides this field.
	LogLevel string `json:"log_level,omitempty"`
	// LogFormat selects the zap encoder. Valid values are "json"
	// (machine-readable, default) and "console" (human-readable, useful
	// for local development). The LOG_FORMAT environment variable
	// overrides this field.
	LogFormat string `json:"log_format,omitempty"`
}

// AuthorizationEntry defines a single authorization source. A user is
// authorized if they match ANY entry (OR logic). Value is always an array
// of strings.
type AuthorizationEntry struct {
	Type  string   `json:"type"`
	Value []string `json:"value"`
}

// Authorization type constants.
const (
	AuthGitHubTeams     = "github_teams"
	AuthGitHubUsers     = "github_users"
	AuthSlackUserGroups = "slack_user_groups"
	AuthSlackEmails     = "slack_emails"
)

// ParseAuthValues returns the flattened values for each authorization source type.
func ParseAuthValues(entries []AuthorizationEntry) (gitHubTeams, gitHubUsers, slackUserGroups, slackEmails []string, err error) {
	for i, e := range entries {
		switch e.Type {
		case AuthGitHubTeams:
			gitHubTeams = append(gitHubTeams, e.Value...)
		case AuthGitHubUsers:
			gitHubUsers = append(gitHubUsers, e.Value...)
		case AuthSlackUserGroups:
			slackUserGroups = append(slackUserGroups, e.Value...)
		case AuthSlackEmails:
			slackEmails = append(slackEmails, e.Value...)
		default:
			return nil, nil, nil, nil, fmt.Errorf("authorization[%d]: unknown type %q", i, e.Type)
		}
	}
	return
}

// ParsedAuthorization holds the flattened authorization sources after parsing.
type ParsedAuthorization struct {
	GitHubTeams     []string
	GitHubUsers     []string
	SlackUserGroups []string
	SlackEmails     []string
}

// RepoDiscoveryConfig holds settings for repo-sourced app discovery.
// The feature is disabled when Enabled is false (the default).
type RepoDiscoveryConfig struct {
	Enabled               bool     `json:"enabled,omitempty"`
	EnforceRepoNaming     bool     `json:"enforce_repo_naming,omitempty"`
	KustomizePathTemplate string   `json:"kustomize_path_template,omitempty"`
	DefaultTagPattern     string   `json:"default_tag_pattern,omitempty"`
	ExemptRepos           []string `json:"exempt_repos,omitempty"`
	PollInterval          string   `json:"poll_interval,omitempty"`
	ConfigFile            string   `json:"config_file,omitempty"`
	RepoPrefix            string   `json:"repo_prefix,omitempty"`
	DiscoveredPath        string   `json:"discovered_path,omitempty"`
	ConfigMapName         string   `json:"configmap_name,omitempty"`
	RateLimitFloor        int      `json:"rate_limit_floor,omitempty"`
}

// KustomizePathForRepo returns the kustomize_path for a given repo and
// environment, using the configured template. Defaults to
// "{env}/{repo}/kustomization.yaml" if no template is set.
func (r *RepoDiscoveryConfig) KustomizePathForRepo(repoName, env string) string {
	tmpl := r.KustomizePathTemplate
	if tmpl == "" {
		tmpl = "{env}/{repo}/kustomization.yaml"
	}
	result := strings.ReplaceAll(tmpl, "{env}", env)
	result = strings.ReplaceAll(result, "{repo}", repoName)
	return result
}

// IsExemptRepo returns true if the given repo (in "org/name" format) is
// exempt from enforce_repo_naming.
func (r *RepoDiscoveryConfig) IsExemptRepo(repo string) bool {
	for _, exempt := range r.ExemptRepos {
		if exempt == repo {
			return true
		}
	}
	return false
}

// PollIntervalDuration returns the parsed poll interval, defaulting to 5m.
func (r *RepoDiscoveryConfig) PollIntervalDuration() time.Duration {
	if r.PollInterval != "" {
		if d, err := time.ParseDuration(r.PollInterval); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Minute
}

// ConfigFileName returns the config file name to look for, defaulting to ".deploy-bot.json".
func (r *RepoDiscoveryConfig) ConfigFileName() string {
	if r.ConfigFile != "" {
		return r.ConfigFile
	}
	return ".deploy-bot.json"
}

// DiscoveredFilePath returns the path for discovered apps, defaulting to "/etc/deploy-bot/discovered/discovered.json".
func (r *RepoDiscoveryConfig) DiscoveredFilePath() string {
	if r.DiscoveredPath != "" {
		return r.DiscoveredPath
	}
	return "/etc/deploy-bot/discovered/discovered.json"
}

// ConfigMapTargetName returns the ConfigMap name, defaulting to "deploy-bot-discovered".
func (r *RepoDiscoveryConfig) ConfigMapTargetName() string {
	if r.ConfigMapName != "" {
		return r.ConfigMapName
	}
	return "deploy-bot-discovered"
}

// RateLimitFloorValue returns the rate limit floor, defaulting to 500.
func (r *RepoDiscoveryConfig) RateLimitFloorValue() int {
	if r.RateLimitFloor > 0 {
		return r.RateLimitFloor
	}
	return 500
}

// ECRAutoDeployConfig holds settings for ECR push-triggered deploys.
type ECRAutoDeployConfig struct {
	Enabled        bool   `json:"enabled,omitempty"`
	SQSQueueURL    string `json:"sqs_queue_url,omitempty"`
	PollInterval   string `json:"poll_interval,omitempty"`
	WebhookEnabled bool   `json:"webhook_enabled,omitempty"`
}

// ArgoCDNotificationsConfig holds settings for the optional inbound ArgoCD
// notifications webhook endpoint. When Enabled is false (the default), the
// receiver mounts nothing and consumes no Redis stream — bot/receiver
// behavior is unchanged from a standard install.
//
// The receiver expects the argocd-notifications-controller webhook service
// to POST a JSON body matching internal/argocd.WebhookPayload to
// /v1/webhooks/argocd, with the shared secret in the X-Deploybot-Secret
// header. See deploy/argocd-notifications/templates.yaml for the
// reference ConfigMap patch.
type ArgoCDNotificationsConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

// PostgresConfig holds connection and operational settings for the
// durable store backing deploy history and in-flight pending deploys.
// Postgres is required in 2.0+; there is no "enabled" switch. The
// bot and receiver fail to start if this block is missing, invalid,
// or the database is unreachable.
//
// Runtime changes to this section are not applied on config hot-
// reload. A change to any postgres field requires a process restart;
// the config watcher logs a warning if a reload detects a diff.
type PostgresConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"` // default 5432
	Database string `json:"database"`
	User     string `json:"user"`
	// SSLMode is one of "disable", "allow", "prefer", "require",
	// "verify-ca", "verify-full". Defaults to "require" — the bot
	// refuses to silently fall back to plaintext if the operator
	// forgot to set it.
	SSLMode string `json:"sslmode,omitempty"`

	// AutoMigrate, when true, causes the bot to run goose.Up() at
	// startup (gated by a Postgres advisory lock so only one replica
	// wins during a rolling restart). Default: false. Operators flip
	// this on in the upgrade window of any release shipping a new
	// migration, observe the migration run at startup, then flip it
	// back off. Release notes for such releases call this out.
	//
	// The receiver NEVER runs migrations even when AutoMigrate is
	// true: cmd/receiver/main.go ignores this field entirely. Only
	// the bot reconciles schema state.
	AutoMigrate bool `json:"auto_migrate,omitempty"`

	// RetentionHistory is the maximum age of a history row before
	// the retention ticker purges it. Accepts Go duration strings
	// (e.g. "8760h"). Defaults to 2 years. Must be >= 390 days —
	// Validate() fails at load time otherwise. The floor exists
	// because audits don't happen immediately at period end; 390
	// days is the empirically-observed "audits are done by now"
	// ceiling and dropping below it would risk deleting data an
	// auditor was about to ask for.
	//
	// pending_deploys has no retention policy: rows are deleted by
	// the bot on state transition to a terminal event (the matching
	// record goes into history). Retention is a history-only concern.
	RetentionHistory string `json:"retention_history,omitempty"`

	// Parsed duration field populated by validateStructured() on
	// success. The getter below returns this directly instead of
	// re-parsing the string on every call. Unexported so JSON
	// round-trips are unaffected.
	historyRetention time.Duration
}

// Postgres retention and default constants. These are deliberately
// top-level constants (not struct fields) so the audit-compliance
// floor is unambiguous and grep-able across the codebase.
const (
	// minPostgresRetentionHistory is the hard floor on history row
	// retention. 390 days — audits don't happen immediately at
	// period end; this covers the audit lag without risking early
	// deletion of compliance-relevant data. Enforced at config load;
	// attempting to run the bot with a shorter value is a fatal
	// startup error.
	minPostgresRetentionHistory = 390 * 24 * time.Hour

	// defaultPostgresRetentionHistory is what operators get when
	// they leave retention_history blank. 2 years — comfortably
	// above the audit floor and cheap to store at deploy-bot scale.
	defaultPostgresRetentionHistory = 2 * 365 * 24 * time.Hour

	// defaultPostgresPort is the standard Postgres listen port.
	defaultPostgresPort = 5432

	// defaultPostgresSSLMode is the default if the operator does
	// not set sslmode explicitly. "require" is the narrowest
	// reasonable default — actual CA verification requires
	// "verify-ca" or "verify-full" which need a trust bundle.
	defaultPostgresSSLMode = "require"
)

// validateStructured returns postgres validation failures as
// []ValidationError and, on success, caches the parsed retention
// durations on the receiver so HistoryRetentionDuration() and
// PendingTerminalRetentionDuration() don't have to re-parse.
//
// This is the single source of truth for postgres validation; both
// Validate() (called from Load for fail-fast startup) and
// ValidateConfig() (the CLI validator) delegate here so the two
// entrypoints can't drift.
func (p *PostgresConfig) validateStructured() []ValidationError {
	var errs []ValidationError
	add := func(field, msg string) {
		errs = append(errs, ValidationError{Section: "postgres", Field: field, Msg: msg})
	}

	if strings.TrimSpace(p.Host) == "" {
		add("host", "required")
	}
	if strings.TrimSpace(p.Database) == "" {
		add("database", "required")
	}
	if strings.TrimSpace(p.User) == "" {
		add("user", "required")
	}
	if p.Port < 0 {
		add("port", fmt.Sprintf("must be >= 0 (0 means default), got %d", p.Port))
	}

	// Seed with defaults so a validated-but-blank config still yields
	// usable durations from the getters below.
	p.historyRetention = defaultPostgresRetentionHistory
	if p.RetentionHistory != "" {
		d, err := time.ParseDuration(p.RetentionHistory)
		switch {
		case err != nil:
			add("retention_history", fmt.Sprintf("invalid duration %q: %v", p.RetentionHistory, err))
		case d < minPostgresRetentionHistory:
			add("retention_history", fmt.Sprintf(
				"must be >= %s (%d days) for audit compliance — audits don't happen "+
					"immediately at period end and anything shorter risks deleting data an "+
					"auditor is about to ask for; got %s",
				minPostgresRetentionHistory, int(minPostgresRetentionHistory.Hours()/24), d,
			))
		default:
			p.historyRetention = d
		}
	}

	return errs
}

// Validate checks that the postgres config is structurally valid and
// that retention values meet the floors. Called from Load() after
// JSON unmarshaling; callers should surface any error and refuse to
// start. Successful validation also caches parsed retention durations
// on the receiver.
func (p *PostgresConfig) Validate() error {
	ve := p.validateStructured()
	if len(ve) == 0 {
		return nil
	}
	wrapped := make([]error, len(ve))
	for i, e := range ve {
		wrapped[i] = e
	}
	return errors.Join(wrapped...)
}

// PortValue returns the configured port or defaultPostgresPort.
func (p *PostgresConfig) PortValue() int {
	if p.Port > 0 {
		return p.Port
	}
	return defaultPostgresPort
}

// SSLModeValue returns the configured sslmode or defaultPostgresSSLMode.
func (p *PostgresConfig) SSLModeValue() string {
	if p.SSLMode != "" {
		return p.SSLMode
	}
	return defaultPostgresSSLMode
}

// HistoryRetentionDuration returns the parsed history retention.
// Populated by validateStructured(); for a validated config this
// never returns a value below the audit-compliance floor. Falls back
// to the default for callers that reach it without having run
// validation (e.g. unit tests using a zero-value PostgresConfig).
func (p *PostgresConfig) HistoryRetentionDuration() time.Duration {
	if p.historyRetention > 0 {
		return p.historyRetention
	}
	return defaultPostgresRetentionHistory
}

// PollIntervalDuration returns the parsed poll interval, defaulting to 30s.
func (e *ECRAutoDeployConfig) PollIntervalDuration() time.Duration {
	if e.PollInterval != "" {
		if d, err := time.ParseDuration(e.PollInterval); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

type GitHubConfig struct {
	Org  string `json:"org"`
	Repo string `json:"repo"`
	// RateLimitMaxRetries is the maximum number of retries on a GitHub secondary
	// rate limit (abuse detection). Defaults to 3.
	RateLimitMaxRetries int `json:"rate_limit_max_retries,omitempty"`
	// RateLimitRetryWait is the maximum duration to wait between retries.
	// Accepts Go duration strings (e.g. "2m"). Defaults to "2m".
	RateLimitRetryWait string `json:"rate_limit_retry_wait,omitempty"`
}

// RateLimitConfig returns the parsed rate limit retry settings with defaults applied.
func (g *GitHubConfig) RateLimitConfig() (maxRetries int, retryWait time.Duration) {
	maxRetries = g.RateLimitMaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	retryWait = 2 * time.Minute
	if g.RateLimitRetryWait != "" {
		if d, err := time.ParseDuration(g.RateLimitRetryWait); err == nil && d > 0 {
			retryWait = d
		}
	}
	return
}

type SlackConfig struct {
	DeployChannel   string   `json:"deploy_channel"`
	AllowedChannels []string `json:"allowed_channels,omitempty"`
	BufferSize      int      `json:"buffer_size,omitempty"`
	// ThreadThreshold controls when deploy notifications are threaded by
	// environment to avoid channel flooding:
	//   0 or omitted: default to 4
	//  -1: never thread (always flat)
	//   1: always thread
	//   N: thread when N+ deploys are pending in the same environment
	ThreadThreshold *int `json:"thread_threshold,omitempty"`
	// RateLimitMaxRetries is the maximum number of retries on a Slack 429
	// rate-limit response. Defaults to 3.
	RateLimitMaxRetries int `json:"rate_limit_max_retries,omitempty"`
	// RateLimitRetryWait is the maximum duration to wait between retries.
	// Accepts Go duration strings (e.g. "30s"). Defaults to "30s".
	RateLimitRetryWait string `json:"rate_limit_retry_wait,omitempty"`
}

// EffectiveThreadThreshold returns the resolved thread threshold.
// 0/omitted defaults to 4.
func (s *SlackConfig) EffectiveThreadThreshold() int {
	if s.ThreadThreshold == nil {
		return 4
	}
	return *s.ThreadThreshold
}

// RateLimitConfig returns the parsed Slack rate limit retry settings with
// defaults applied.
func (s *SlackConfig) RateLimitConfig() (maxRetries int, retryWait time.Duration) {
	maxRetries = s.RateLimitMaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	retryWait = 30 * time.Second
	if s.RateLimitRetryWait != "" {
		if d, err := time.ParseDuration(s.RateLimitRetryWait); err == nil && d > 0 {
			retryWait = d
		}
	}
	return
}

// IsChannelAllowed returns true if the channel is permitted to use the bot.
// If AllowedChannels is empty, all channels are allowed.
func (s *SlackConfig) IsChannelAllowed(channelID string) bool {
	if len(s.AllowedChannels) == 0 {
		return true
	}
	for _, id := range s.AllowedChannels {
		if id == channelID {
			return true
		}
	}
	return false
}

type DeploymentConfig struct {
	StaleDuration       string `json:"stale_duration"`
	MergeMethod         string `json:"merge_method"`
	LockTTL             string `json:"lock_ttl"`
	Label               string `json:"label,omitempty"`
	ReconcileInterval   string `json:"reconcile_interval,omitempty"`
	AllowProdAutoDeploy bool   `json:"allow_prod_auto_deploy,omitempty"`
}

// DeployLabel returns the configured GitHub label name, defaulting to "deploy-bot".
func (c *Config) DeployLabel() string {
	if c.Deployment.Label == "" {
		return "deploy-bot"
	}
	return c.Deployment.Label
}

// PendingLabel returns the label applied to open deploy PRs and removed on any
// closure. Derived from DeployLabel so no separate config is needed.
func (c *Config) PendingLabel() string {
	return c.DeployLabel() + "/pending"
}

type AWSConfig struct {
	ECRRegion   string `json:"ecr_region"`
	AuditBucket string `json:"audit_bucket"`
	AuditRegion string `json:"audit_region"`
}

type AppConfig struct {
	App           string `json:"app"`
	Environment   string `json:"environment"`
	KustomizePath string `json:"kustomize_path"`
	ECRRepo       string `json:"ecr_repo"`
	TagPattern    string `json:"tag_pattern"`
	// AutoDeploy, when true, causes ECR push-triggered deploys to merge
	// automatically without human approval. Subject to the global
	// AllowProdAutoDeploy guard for production environments.
	AutoDeploy bool `json:"auto_deploy,omitempty"`

	// SourceRepo is set only for repo-discovered apps (e.g. "org/myapp").
	// Empty for operator-managed apps. Not serialized to the primary config.
	SourceRepo string `json:"-"`

	compiledPattern *regexp.Regexp
}

// FullName returns the composite app name including the environment suffix
// (e.g. "nginx-dev"). This is the name used in slash commands, store keys,
// branch names, and all user-facing messages.
func (a *AppConfig) FullName() string {
	return a.App + "-" + a.Environment
}

// IsProd returns true if the app's environment is "prod" or "production".
func (a *AppConfig) IsProd() bool {
	env := strings.ToLower(a.Environment)
	return env == "prod" || env == "production"
}

// EffectiveAutoDeploy returns whether this app should auto-deploy, taking the
// global production guard into account.
func (a *AppConfig) EffectiveAutoDeploy(allowProdAutoDeploy bool) bool {
	if !a.AutoDeploy {
		return false
	}
	if a.IsProd() && !allowProdAutoDeploy {
		return false
	}
	return true
}

// CompiledTagPattern returns a compiled version of TagPattern, compiling it
// on first call. Panics if TagPattern is not a valid regular expression.
func (a *AppConfig) CompiledTagPattern() *regexp.Regexp {
	if a.compiledPattern == nil {
		a.compiledPattern = regexp.MustCompile(a.TagPattern)
	}
	return a.compiledPattern
}

type Secrets struct {
	SlackBotToken           string `json:"slack_bot_token"`
	SlackAppToken           string `json:"slack_app_token"`
	GitHubToken             string `json:"github_token,omitempty"`
	GitHubScannerToken      string `json:"github_scanner_token,omitempty"`
	GitHubAppID             int64  `json:"github_app_id,omitempty"`
	GitHubAppInstallationID int64  `json:"github_app_installation_id,omitempty"`
	GitHubAppPrivateKey     string `json:"github_app_private_key,omitempty"`
	RedisAddr               string `json:"redis_addr"`
	RedisToken              string `json:"redis_token,omitempty"`
	RedisIAMAuth            bool   `json:"redis_iam_auth,omitempty"`
	RedisUserID             string `json:"redis_user_id,omitempty"`
	RedisReplicationGroupID string `json:"redis_replication_group_id,omitempty"`
	ECRWebhookAPIKey        string `json:"ecr_webhook_api_key,omitempty"`
	// ArgoCDWebhookAPIKey is the shared secret expected in the
	// X-Deploybot-Secret header on inbound ArgoCD notification webhooks.
	// Required (>= 32 chars) when ArgoCDNotifications.Enabled is true.
	ArgoCDWebhookAPIKey string `json:"argocd_webhook_api_key,omitempty"`

	// PostgresPassword is used to authenticate to the durable store
	// when PostgresIAMAuth is false (the default). Required in that
	// case; ignored when PostgresIAMAuth is true.
	PostgresPassword string `json:"postgres_password,omitempty"`

	// PostgresIAMAuth, when true, causes the bot to authenticate to
	// AWS RDS using IAM-generated tokens instead of a static
	// password. The token is short-lived (~15m) and refreshed
	// transparently by the pool; requires PostgresRDSRegion to be
	// set so the SigV4 signer knows which region to target.
	//
	// This mirrors the existing RedisIAMAuth pattern. The code path
	// that generates tokens lives in internal/store/postgres/.
	PostgresIAMAuth bool `json:"postgres_iam_auth,omitempty"`

	// PostgresRDSRegion is the AWS region hosting the RDS instance,
	// needed for SigV4 presigning when PostgresIAMAuth is true.
	// Ignored when password auth is in use.
	PostgresRDSRegion string `json:"postgres_rds_region,omitempty"`
}

// UseGitHubApp returns true if GitHub App credentials are configured.
func (s *Secrets) UseGitHubApp() bool {
	return s.GitHubAppID != 0
}

// ScannerToken returns the token to use for repo scanning. If a dedicated
// scanner token is configured, it is returned; otherwise the primary
// GitHubToken is used.
func (s *Secrets) ScannerToken() string {
	if s.GitHubScannerToken != "" {
		return s.GitHubScannerToken
	}
	return s.GitHubToken
}

// Validate checks that all required secrets are present and have the expected format.
func (s *Secrets) Validate() error {
	var errs []error
	if s.SlackBotToken == "" {
		errs = append(errs, errors.New("slack_bot_token is empty"))
	} else if !strings.HasPrefix(s.SlackBotToken, "xoxb-") {
		errs = append(errs, fmt.Errorf("slack_bot_token has unexpected prefix (want xoxb-, got %q)", tokenPrefix(s.SlackBotToken)))
	}
	if s.SlackAppToken != "" && !strings.HasPrefix(s.SlackAppToken, "xapp-") {
		errs = append(errs, fmt.Errorf("slack_app_token has unexpected prefix (want xapp-, got %q)", tokenPrefix(s.SlackAppToken)))
	}
	appFields := 0
	if s.GitHubAppID != 0 {
		appFields++
	}
	if s.GitHubAppInstallationID != 0 {
		appFields++
	}
	if s.GitHubAppPrivateKey != "" {
		appFields++
	}
	if appFields > 0 && appFields < 3 {
		errs = append(errs, errors.New("github_app_id, github_app_installation_id, and github_app_private_key must all be set together"))
	}
	if appFields == 3 {
		if !strings.Contains(s.GitHubAppPrivateKey, "BEGIN") {
			errs = append(errs, errors.New("github_app_private_key does not appear to be a PEM-encoded key"))
		}
	}
	if s.GitHubToken == "" && appFields == 0 {
		errs = append(errs, errors.New("github_token or github app credentials (github_app_id, github_app_installation_id, github_app_private_key) required"))
	}
	if s.RedisAddr == "" {
		errs = append(errs, errors.New("redis_addr is empty"))
	}
	if s.RedisIAMAuth {
		if s.RedisUserID == "" {
			errs = append(errs, errors.New("redis_user_id is required when redis_iam_auth is true"))
		}
		if s.RedisReplicationGroupID == "" {
			errs = append(errs, errors.New("redis_replication_group_id is required when redis_iam_auth is true"))
		}
		if s.RedisToken != "" {
			errs = append(errs, errors.New("redis_token and redis_iam_auth are mutually exclusive"))
		}
	}
	// Postgres auth: IAM or password, exactly one.
	if s.PostgresIAMAuth {
		if s.PostgresRDSRegion == "" {
			errs = append(errs, errors.New("postgres_rds_region is required when postgres_iam_auth is true"))
		}
		if s.PostgresPassword != "" {
			errs = append(errs, errors.New("postgres_password and postgres_iam_auth are mutually exclusive"))
		}
	} else {
		if s.PostgresPassword == "" {
			errs = append(errs, errors.New("postgres_password is required (set postgres_iam_auth: true to use RDS IAM auth instead)"))
		}
	}
	return errors.Join(errs...)
}

// tokenPrefix returns the portion of a token before the first hyphen, or the
// full token if there is no hyphen, for use in error messages.
func tokenPrefix(token string) string {
	if idx := strings.Index(token, "-"); idx >= 0 {
		return token[:idx] + "-"
	}
	return token
}

// DiscoveredApps is the format of the discovered apps file written by the
// repo scanner. Each entry includes a SourceRepo field for audit/debugging.
type DiscoveredApps struct {
	Apps []DiscoveredAppConfig `json:"apps"`
}

// DiscoveredAppConfig extends AppConfig with the source repository.
type DiscoveredAppConfig struct {
	AppConfig
	SourceRepo string `json:"_source_repo,omitempty"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Deployment.MergeMethod == "" {
		cfg.Deployment.MergeMethod = "squash"
	}
	if cfg.Deployment.StaleDuration != "" {
		if _, err := time.ParseDuration(cfg.Deployment.StaleDuration); err != nil {
			return nil, fmt.Errorf("invalid deployment.stale_duration %q: %w", cfg.Deployment.StaleDuration, err)
		}
	}
	if cfg.Deployment.LockTTL != "" {
		if _, err := time.ParseDuration(cfg.Deployment.LockTTL); err != nil {
			return nil, fmt.Errorf("invalid deployment.lock_ttl %q: %w", cfg.Deployment.LockTTL, err)
		}
	}
	if err := cfg.Postgres.Validate(); err != nil {
		return nil, fmt.Errorf("postgres config: %w", err)
	}
	if cfg.LogLevel != "" {
		if _, err := ParseLogLevel(cfg.LogLevel); err != nil {
			return nil, fmt.Errorf("invalid log_level %q: %w", cfg.LogLevel, err)
		}
	}
	if cfg.LogFormat != "" {
		if _, err := ParseLogFormat(cfg.LogFormat); err != nil {
			return nil, fmt.Errorf("invalid log_format %q: %w", cfg.LogFormat, err)
		}
	}
	kpaths := map[string]string{} // kustomize_path -> app name
	for _, app := range cfg.Apps {
		if app.Environment == "" {
			return nil, fmt.Errorf("app %q is missing required field \"environment\"", app.App)
		}
		if app.TagPattern != "" {
			if _, err := regexp.Compile(app.TagPattern); err != nil {
				return nil, fmt.Errorf("app %q has invalid tag_pattern: %w", app.App, err)
			}
		}
		if app.KustomizePath != "" {
			if other, ok := kpaths[app.KustomizePath]; ok {
				return nil, fmt.Errorf("app %q and %q both target kustomize_path %q", app.App, other, app.KustomizePath)
			}
			kpaths[app.KustomizePath] = app.App
		}
	}
	return &cfg, nil
}

// LoadWithDiscovered loads the primary config and merges in discovered apps
// from discoveredPath. Operator-defined apps take precedence: any discovered
// app whose (app, environment) pair already exists in the primary config is
// silently skipped. If discoveredPath is empty or the file doesn't exist, only
// the primary config is returned.
func LoadWithDiscovered(path, discoveredPath string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if discoveredPath == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(discoveredPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read discovered apps: %w", err)
	}
	if len(data) == 0 {
		return cfg, nil
	}
	var discovered DiscoveredApps
	if err := json.Unmarshal(data, &discovered); err != nil {
		return nil, fmt.Errorf("parse discovered apps: %w", err)
	}
	cfg.Apps = MergeApps(cfg.Apps, discovered.Apps)
	return cfg, nil
}

// Conflict describes a repo-sourced app blocked by an operator-managed entry.
type Conflict struct {
	App        string
	Env        string
	SourceRepo string
}

// LoadConflicts reads the discovered file and returns any entries whose
// (app, environment) pair collides with the primary config.
func LoadConflicts(primaryPath, discoveredPath string) ([]Conflict, error) {
	if discoveredPath == "" {
		return nil, nil
	}
	primary, err := Load(primaryPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(discoveredPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var discovered DiscoveredApps
	if err := json.Unmarshal(data, &discovered); err != nil {
		return nil, err
	}
	operatorApps := make(map[string]struct{}, len(primary.Apps))
	for _, a := range primary.Apps {
		operatorApps[a.FullName()] = struct{}{}
	}
	var conflicts []Conflict
	for _, d := range discovered.Apps {
		key := d.FullName()
		if _, ok := operatorApps[key]; ok {
			conflicts = append(conflicts, Conflict{
				App:        d.App,
				Env:        d.Environment,
				SourceRepo: d.SourceRepo,
			})
		}
	}
	return conflicts, nil
}

// MergeApps appends discovered apps to the primary list, skipping any whose
// (app, environment) pair already exists in primary.
func MergeApps(primary []AppConfig, discovered []DiscoveredAppConfig) []AppConfig {
	existing := make(map[string]struct{}, len(primary))
	for _, a := range primary {
		existing[a.FullName()] = struct{}{}
	}
	result := make([]AppConfig, len(primary))
	copy(result, primary)
	for _, d := range discovered {
		key := d.FullName()
		if _, ok := existing[key]; ok {
			continue
		}
		existing[key] = struct{}{}
		app := d.AppConfig
		app.SourceRepo = d.SourceRepo
		result = append(result, app)
	}
	return result
}

// StaleDuration returns the parsed stale duration. The string form is
// validated at Load time, so this accessor is infallible — it returns the
// default if Deployment.StaleDuration is empty and the parsed value
// otherwise. A parse error here is a programming bug, not a runtime
// condition, so we panic.
func (c *Config) StaleDuration() time.Duration {
	if c.Deployment.StaleDuration == "" {
		return 2 * time.Hour
	}
	d, err := time.ParseDuration(c.Deployment.StaleDuration)
	if err != nil {
		panic(fmt.Sprintf("config: stale_duration %q invalid post-Load: %v", c.Deployment.StaleDuration, err))
	}
	return d
}

// LockTTL is the parsed deploy lock TTL. See StaleDuration for the
// validate-at-load contract.
func (c *Config) LockTTL() time.Duration {
	if c.Deployment.LockTTL == "" {
		return 5 * time.Minute
	}
	d, err := time.ParseDuration(c.Deployment.LockTTL)
	if err != nil {
		panic(fmt.Sprintf("config: lock_ttl %q invalid post-Load: %v", c.Deployment.LockTTL, err))
	}
	return d
}

// LoadSecretsFromFile reads and parses bot secrets from a JSON file on disk.
// This supports Kubernetes Secret volume mounts as an alternative to AWS
// Secrets Manager.
func LoadSecretsFromFile(path string) (*Secrets, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	var secrets Secrets
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("parse secrets file: %w", err)
	}
	return &secrets, nil
}

// LoadSecrets fetches and parses the bot secrets from AWS Secrets Manager.
func LoadSecrets(ctx context.Context, secretName string) (*Secrets, error) {
	if secretName == "" {
		return nil, fmt.Errorf("secret name is empty")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := secretsmanager.NewFromConfig(cfg)
	out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretName),
	})
	if err != nil {
		return nil, fmt.Errorf("get secret: %w", err)
	}
	var secrets Secrets
	if err := json.Unmarshal([]byte(aws.ToString(out.SecretString)), &secrets); err != nil {
		return nil, fmt.Errorf("parse secrets: %w", err)
	}
	return &secrets, nil
}

func (c *Config) AppByName(name string) (*AppConfig, bool) {
	for i := range c.Apps {
		if c.Apps[i].FullName() == name {
			return &c.Apps[i], true
		}
	}
	return nil, false
}

// AppByECRRepo returns the first app whose ECR repo contains the given
// repository name as a suffix. This matches the short repo name from
// EventBridge events against the full URI in config.
func (c *Config) AppByECRRepo(repoName string) (*AppConfig, bool) {
	for i := range c.Apps {
		if strings.HasSuffix(c.Apps[i].ECRRepo, "/"+repoName) || c.Apps[i].ECRRepo == repoName {
			return &c.Apps[i], true
		}
	}
	return nil, false
}

// AppsByECRRepo returns all apps whose ECR repo matches the given repository
// name. Multiple apps may share the same ECR repo (e.g. the same image
// deployed across environments).
func (c *Config) AppsByECRRepo(repoName string) []*AppConfig {
	var matches []*AppConfig
	for i := range c.Apps {
		if strings.HasSuffix(c.Apps[i].ECRRepo, "/"+repoName) || c.Apps[i].ECRRepo == repoName {
			matches = append(matches, &c.Apps[i])
		}
	}
	return matches
}

// UniqueAppNames returns deduplicated, sorted base app names across all
// configured apps.
func (c *Config) UniqueAppNames() []string {
	seen := map[string]bool{}
	for _, app := range c.Apps {
		seen[app.App] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// UniqueEnvironments returns deduplicated, sorted environment names across all
// configured apps.
func (c *Config) UniqueEnvironments() []string {
	seen := map[string]bool{}
	for _, app := range c.Apps {
		seen[app.Environment] = true
	}
	envs := make([]string, 0, len(seen))
	for env := range seen {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	return envs
}

// EnvironmentsForApp returns the sorted environments where the given base app
// name is configured.
func (c *Config) EnvironmentsForApp(appName string) []string {
	var envs []string
	for _, app := range c.Apps {
		if app.App == appName {
			envs = append(envs, app.Environment)
		}
	}
	sort.Strings(envs)
	return envs
}

// AppsForEnvironment returns the sorted base app names available in the given
// environment.
func (c *Config) AppsForEnvironment(env string) []string {
	var names []string
	for _, app := range c.Apps {
		if app.Environment == env {
			names = append(names, app.App)
		}
	}
	sort.Strings(names)
	return names
}

// AppByComponents looks up an AppConfig by base name and environment.
func (c *Config) AppByComponents(appName, env string) (*AppConfig, bool) {
	for i := range c.Apps {
		if c.Apps[i].App == appName && c.Apps[i].Environment == env {
			return &c.Apps[i], true
		}
	}
	return nil, false
}
