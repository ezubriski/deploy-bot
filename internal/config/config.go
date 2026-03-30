package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type Config struct {
	GitHub     GitHubConfig     `json:"github"`
	Slack      SlackConfig      `json:"slack"`
	Deployment DeploymentConfig `json:"deployment"`
	AWS        AWSConfig        `json:"aws"`
	Apps       []AppConfig      `json:"apps"`
}

type GitHubConfig struct {
	Org          string `json:"org"`
	Repo         string `json:"repo"`
	DeployerTeam string `json:"deployer_team"`
	ApproverTeam string `json:"approver_team"`
}

type SlackConfig struct {
	DeployChannel string `json:"deploy_channel"`
}

type DeploymentConfig struct {
	StaleDuration string `json:"stale_duration"`
	MergeMethod   string `json:"merge_method"`
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
}

type Secrets struct {
	SlackBotToken string `json:"slack_bot_token"`
	SlackAppToken string `json:"slack_app_token"`
	GitHubToken   string `json:"github_token"`
	RedisAddr     string `json:"redis_addr"`
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

func (c *Config) AppByName(name string) (*AppConfig, bool) {
	for i := range c.Apps {
		if c.Apps[i].App == name {
			return &c.Apps[i], true
		}
	}
	return nil, false
}
