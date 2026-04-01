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
	GitHub     GitHubConfig     `json:"github"`
	Slack      SlackConfig      `json:"slack"`
	Deployment DeploymentConfig `json:"deployment"`
	AWS        AWSConfig        `json:"aws"`
	Apps       []AppConfig      `json:"apps"`
}

type GitHubConfig struct {
	Org          string            `json:"org"`
	Repo         string            `json:"repo"`
	DeployerTeam string            `json:"deployer_team"`
	ApproverTeam string            `json:"approver_team"`
	// Users maps Slack user IDs to GitHub logins for users whose GitHub email
	// is private. Takes precedence over the Slack email → GitHub search lookup.
	Users               map[string]string `json:"users,omitempty"`
	// RateLimitMaxRetries is the maximum number of retries on a GitHub secondary
	// rate limit (abuse detection). Defaults to 3.
	RateLimitMaxRetries int    `json:"rate_limit_max_retries,omitempty"`
	// RateLimitRetryWait is the maximum duration to wait between retries.
	// Accepts Go duration strings (e.g. "2m"). Defaults to "2m".
	RateLimitRetryWait  string `json:"rate_limit_retry_wait,omitempty"`
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
	// RateLimitMaxRetries is the maximum number of retries on a Slack 429
	// rate-limit response. Defaults to 3.
	RateLimitMaxRetries int    `json:"rate_limit_max_retries,omitempty"`
	// RateLimitRetryWait is the maximum duration to wait between retries.
	// Accepts Go duration strings (e.g. "30s"). Defaults to "30s".
	RateLimitRetryWait  string `json:"rate_limit_retry_wait,omitempty"`
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
	StaleDuration     string `json:"stale_duration"`
	MergeMethod       string `json:"merge_method"`
	LockTTL           string `json:"lock_ttl"`
	Label             string `json:"label,omitempty"`
	ReconcileInterval string `json:"reconcile_interval,omitempty"`
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
	ECRRoleARN    string `json:"ecr_role_arn"`
	ECRRegion     string `json:"ecr_region"`
	AuditRoleARN  string `json:"audit_role_arn"`
	AuditBucket   string `json:"audit_bucket"`
	AuditRegion   string `json:"audit_region"`
}

type AppConfig struct {
	App           string `json:"app"`
	KustomizePath string `json:"kustomize_path"`
	ECRRepo       string `json:"ecr_repo"`
	TagPattern    string `json:"tag_pattern"`

	compiledPattern *regexp.Regexp
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
	SlackBotToken string `json:"slack_bot_token"`
	SlackAppToken string `json:"slack_app_token"`
	GitHubToken   string `json:"github_token"`
	RedisAddr     string `json:"redis_addr"`
	RedisToken    string `json:"redis_token,omitempty"`
}

// Validate checks that all required secrets are present and have the expected format.
func (s *Secrets) Validate() error {
	var errs []error
	if s.SlackBotToken == "" {
		errs = append(errs, errors.New("slack_bot_token is empty"))
	} else if !strings.HasPrefix(s.SlackBotToken, "xoxb-") {
		errs = append(errs, fmt.Errorf("slack_bot_token has unexpected prefix (want xoxb-, got %q)", tokenPrefix(s.SlackBotToken)))
	}
	if s.SlackAppToken == "" {
		errs = append(errs, errors.New("slack_app_token is empty"))
	} else if !strings.HasPrefix(s.SlackAppToken, "xapp-") {
		errs = append(errs, fmt.Errorf("slack_app_token has unexpected prefix (want xapp-, got %q)", tokenPrefix(s.SlackAppToken)))
	}
	if s.GitHubToken == "" {
		errs = append(errs, errors.New("github_token is empty"))
	}
	if s.RedisAddr == "" {
		errs = append(errs, errors.New("redis_addr is empty"))
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
	return &cfg, nil
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
