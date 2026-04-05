package reposcanner

import (
	"strings"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/repoconfig"
)

// parseRepoConfig parses and validates a repo config file, returning the valid
// app entries and any validation errors. Invalid entries are skipped — one bad
// entry does not invalidate the entire file.
func parseRepoConfig(data []byte, sourceRepo string, rdCfg config.RepoDiscoveryConfig) ([]config.DiscoveredAppConfig, []error) {
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
		RepoNaming:        rdCfg.EnforceRepoNaming,
		RepoName:          repoName,
		Exempt:            rdCfg.IsExemptRepo(sourceRepo),
		DefaultTagPattern: rdCfg.DefaultTagPattern,
	}
	if rdCfg.EnforceRepoNaming {
		opts.KustomizePathFn = func(repo, env string) string {
			return rdCfg.KustomizePathForRepo(repo, env)
		}
	}

	verrs := repoconfig.ValidateWithOpts(cfg, opts)

	// Apply derived/default values to valid entries before converting.
	if rdCfg.EnforceRepoNaming && !opts.Exempt && cfg.APIVersion == repoconfig.VersionV2 {
		repoconfig.ApplyDefaults(cfg, opts)
	} else if rdCfg.DefaultTagPattern != "" {
		// Even non-enforced configs get the default tag pattern.
		repoconfig.ApplyDefaults(cfg, repoconfig.ValidateOpts{
			DefaultTagPattern: rdCfg.DefaultTagPattern,
		})
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
