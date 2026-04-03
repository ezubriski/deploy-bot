package reposcanner

import (
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/repoconfig"
)

// parseRepoConfig parses and validates a repo config file, returning the valid
// app entries and any validation errors. Invalid entries are skipped — one bad
// entry does not invalidate the entire file.
func parseRepoConfig(data []byte, sourceRepo string) ([]config.DiscoveredAppConfig, []error) {
	cfg, err := repoconfig.Parse(data)
	if err != nil {
		return nil, []error{err}
	}

	verrs := repoconfig.Validate(cfg)

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
