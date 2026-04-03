package reposcanner

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ezubriski/deploy-bot/internal/config"
)

// repoConfig is the structure expected in each repository's config file.
type repoConfig struct {
	Apps []repoAppEntry `json:"apps"`
}

// repoAppEntry is a single app entry from a repo config file.
type repoAppEntry struct {
	App                     string `json:"app"`
	Environment             string `json:"environment"`
	KustomizePath           string `json:"kustomize_path"`
	ECRRepo                 string `json:"ecr_repo"`
	TagPattern              string `json:"tag_pattern"`
	AutoDeploy              bool   `json:"auto_deploy,omitempty"`
	AutoDeployApproverGroup string `json:"auto_deploy_approver_group,omitempty"`
}

// validationError describes a validation failure for a single app entry.
type validationError struct {
	Index int
	App   string
	Field string
	Msg   string
}

func (e *validationError) Error() string {
	if e.App != "" {
		return fmt.Sprintf("apps[%d] (%s): %s: %s", e.Index, e.App, e.Field, e.Msg)
	}
	return fmt.Sprintf("apps[%d]: %s: %s", e.Index, e.Field, e.Msg)
}

// parseRepoConfig parses and validates a repo config file, returning the valid
// app entries and any validation errors. Invalid entries are skipped — one bad
// entry does not invalidate the entire file.
func parseRepoConfig(data []byte, sourceRepo string) ([]config.DiscoveredAppConfig, []error) {
	var rc repoConfig
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, []error{fmt.Errorf("parse JSON: %w", err)}
	}

	var valid []config.DiscoveredAppConfig
	var errs []error

	for i, entry := range rc.Apps {
		if err := validateEntry(i, entry); err != nil {
			errs = append(errs, err)
			continue
		}
		valid = append(valid, config.DiscoveredAppConfig{
			AppConfig: config.AppConfig{
				App:                     entry.App,
				Environment:             entry.Environment,
				KustomizePath:           entry.KustomizePath,
				ECRRepo:                 entry.ECRRepo,
				TagPattern:              entry.TagPattern,
				AutoDeploy:              entry.AutoDeploy,
				AutoDeployApproverGroup: entry.AutoDeployApproverGroup,
			},
			SourceRepo: sourceRepo,
		})
	}

	return valid, errs
}

func validateEntry(index int, e repoAppEntry) error {
	e.App = strings.TrimSpace(e.App)
	if e.App == "" {
		return &validationError{Index: index, Field: "app", Msg: "required"}
	}
	if strings.TrimSpace(e.Environment) == "" {
		return &validationError{Index: index, App: e.App, Field: "environment", Msg: "required"}
	}
	if strings.TrimSpace(e.KustomizePath) == "" {
		return &validationError{Index: index, App: e.App, Field: "kustomize_path", Msg: "required"}
	}
	if strings.TrimSpace(e.ECRRepo) == "" {
		return &validationError{Index: index, App: e.App, Field: "ecr_repo", Msg: "required"}
	}
	if e.TagPattern != "" {
		if _, err := regexp.Compile(e.TagPattern); err != nil {
			return &validationError{Index: index, App: e.App, Field: "tag_pattern", Msg: fmt.Sprintf("invalid regex: %v", err)}
		}
	}
	return nil
}
