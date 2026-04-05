package reposcanner

import (
	"strings"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/repoconfig"
)

// parseRepoConfig parses and validates a repo config file, returning the valid
// app entries and any validation errors. Invalid entries are skipped — one bad
// entry does not invalidate the entire file.
//
// When enforceRepoNaming is true, app and kustomize_path are derived from
// sourceRepo for v2 configs and validated against the convention.
func parseRepoConfig(data []byte, sourceRepo string, enforceRepoNaming bool) ([]config.DiscoveredAppConfig, []error) {
	cfg, err := repoconfig.Parse(data)
	if err != nil {
		return nil, []error{err}
	}

	// Extract the short repo name (e.g. "my-service" from "org/my-service").
	repoName := sourceRepo
	if idx := strings.LastIndex(sourceRepo, "/"); idx >= 0 {
		repoName = sourceRepo[idx+1:]
	}

	opts := repoconfig.ValidateOpts{
		RepoNaming: enforceRepoNaming,
		RepoName:   repoName,
	}
	verrs := repoconfig.ValidateWithOpts(cfg, opts)

	// Apply derived values to valid entries before converting.
	if enforceRepoNaming && cfg.APIVersion == repoconfig.VersionV2 {
		repoconfig.ApplyRepoNaming(cfg, repoName)
	}

	validIndices := repoconfig.ValidEntries(cfg, verrs)
	var valid []config.DiscoveredAppConfig
	for _, i := range validIndices {
		e := cfg.Apps[i]
		valid = append(valid, config.DiscoveredAppConfig{
			AppConfig: config.AppConfig{
				App:                     e.App,
				Environment:             e.Environment,
				KustomizePath:           e.KustomizePath,
				ECRRepo:                 e.ECRRepo,
				TagPattern:              e.TagPattern,
				AutoDeploy:              e.AutoDeploy,
				AutoDeployApproverGroup: e.AutoDeployApproverGroup,
			},
			SourceRepo: sourceRepo,
		})
	}

	var errs []error
	for i := range verrs {
		errs = append(errs, &verrs[i])
	}

	return valid, errs
}
