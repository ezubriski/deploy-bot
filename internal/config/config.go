package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type Config struct {
	GitHub        GitHubConfig        `json:"github"`
	Slack         SlackConfig         `json:"slack"`
	Deployment    DeploymentConfig    `json:"deployment"`
	AWS           AWSConfig           `json:"aws"`
	ECREvents     ECREventsConfig     `json:"ecr_events,omitempty"`
	RepoDiscovery RepoDiscoveryConfig `json:"repo_discovery,omitempty"`
	Apps          []AppConfig         `json:"apps"`
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
	ConfigMapNamespace    string   `json:"configmap_namespace,omitempty"`
	RateLimitFloor        int      `json:"rate_limit_floor,omitempty"`
	WarnChannel           string   `json:"warn_channel,omitempty"`
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

// ECREventsConfig holds settings for ECR push-triggered deploys.
// The feature is disabled when SQSQueueURL is empty.
type ECREventsConfig struct {
	SQSQueueURL  string `json:"sqs_queue_url,omitempty"`
	PollInterval string `json:"poll_interval,omitempty"`
}

// PollIntervalDuration returns the parsed poll interval, defaulting to 30s.
func (e *ECREventsConfig) PollIntervalDuration() time.Duration {
	if e.PollInterval != "" {
		if d, err := time.ParseDuration(e.PollInterval); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

type GitHubConfig struct {
	Org          string `json:"org"`
	Repo         string `json:"repo"`
	DeployerTeam string `json:"deployer_team"`
	ApproverTeam string `json:"approver_team"`
	// Users maps Slack user IDs to GitHub logins for users whose GitHub email
	// is private. Takes precedence over the Slack email → GitHub search lookup.
	Users map[string]string `json:"users,omitempty"`
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
	// ApproverGroup is the default Slack user group (S...) or channel (C...) to
	// mention when requesting approval. Per-app auto_deploy_approver_group
	// overrides this.
	ApproverGroup string `json:"approver_group,omitempty"`
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
	ECRRoleARN   string `json:"ecr_role_arn"`
	ECRRegion    string `json:"ecr_region"`
	AuditRoleARN string `json:"audit_role_arn"`
	AuditBucket  string `json:"audit_bucket"`
	AuditRegion  string `json:"audit_region"`
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
	// AutoDeployApproverGroup is the Slack ID to notify for ECR-triggered deploys
	// and no-op notifications. Use a channel ID (C…) to post there directly, or
	// a user group ID (S…) to @mention the group in the deploy channel.
	AutoDeployApproverGroup string `json:"auto_deploy_approver_group,omitempty"`

	// SourceRepo is set only for repo-discovered apps (e.g. "org/myapp").
	// Empty for operator-managed apps. Not serialized to the primary config.
	SourceRepo string `json:"-"`

	compiledPattern *regexp.Regexp
}

// IsProd returns true if the app's environment is "prod" or "production".
func (a *AppConfig) IsProd() bool {
	env := strings.ToLower(a.Environment)
	return env == "prod" || env == "production"
}

// EffectiveApproverGroup returns the Slack group to mention for approval
// requests. Per-app auto_deploy_approver_group takes precedence over the
// global slack.approver_group default.
func (a *AppConfig) EffectiveApproverGroup(globalDefault string) string {
	if a.AutoDeployApproverGroup != "" {
		return a.AutoDeployApproverGroup
	}
	return globalDefault
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
	GitHubToken             string `json:"github_token"`
	GitHubScannerToken      string `json:"github_scanner_token,omitempty"`
	RedisAddr               string `json:"redis_addr"`
	RedisToken              string `json:"redis_token,omitempty"`
	RedisIAMAuth            bool   `json:"redis_iam_auth,omitempty"`
	RedisUserID             string `json:"redis_user_id,omitempty"`
	RedisReplicationGroupID string `json:"redis_replication_group_id,omitempty"`
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
	if s.GitHubToken == "" {
		errs = append(errs, errors.New("github_token is empty"))
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
		operatorApps[a.App+"\x00"+a.Environment] = struct{}{}
	}
	var conflicts []Conflict
	for _, d := range discovered.Apps {
		key := d.App + "\x00" + d.Environment
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
		existing[a.App+"\x00"+a.Environment] = struct{}{}
	}
	result := make([]AppConfig, len(primary))
	copy(result, primary)
	for _, d := range discovered {
		key := d.App + "\x00" + d.Environment
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

func (c *Config) StaleDuration() (time.Duration, error) {
	if c.Deployment.StaleDuration == "" {
		return 2 * time.Hour, nil
	}
	return time.ParseDuration(c.Deployment.StaleDuration)
}

func (c *Config) LockTTL() (time.Duration, error) {
	if c.Deployment.LockTTL == "" {
		return 5 * time.Minute, nil
	}
	return time.ParseDuration(c.Deployment.LockTTL)
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
		if c.Apps[i].App == name {
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
