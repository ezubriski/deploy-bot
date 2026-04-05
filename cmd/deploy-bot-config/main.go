// deploy-bot-config validates deploy-bot configuration files.
//
// Usage:
//
//	# Validate the main config.json
//	deploy-bot-config validate --config config.json
//
//	# Validate a repo-sourced .deploy-bot.json
//	deploy-bot-config validate --file .deploy-bot.json
//
//	# JSON output
//	deploy-bot-config validate --config config.json --format json
//
// Legacy usage (no subcommand) is equivalent to validate --file:
//
//	deploy-bot-config --file .deploy-bot.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/repoconfig"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// Subcommand dispatch. If the first arg is "validate", parse its flags.
	// Otherwise fall back to legacy mode (validate --file).
	if len(args) > 0 && args[0] == "validate" {
		return runValidate(args[1:])
	}
	return runValidate(args)
}

func runValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	filePath := fs.String("file", "", "path to .deploy-bot.json (repo-sourced app config)")
	fs.StringVar(filePath, "f", "", "path to .deploy-bot.json (shorthand)")
	configPath := fs.String("config", "", "path to config.json (main bot config)")
	fs.StringVar(configPath, "c", "", "path to config.json (shorthand)")
	repoNaming := fs.Bool("repo-naming", false, "simulate enforce_repo_naming (derives app and kustomize_path from --repo)")
	repoName := fs.String("repo", "", "repository name for --repo-naming (e.g. my-service)")
	format := fs.String("format", "text", "output format: text or json")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *repoNaming && *repoName == "" {
		fmt.Fprintln(os.Stderr, "error: --repo-naming requires --repo <name>")
		return 2
	}

	opts := repoconfig.ValidateOpts{
		RepoNaming: *repoNaming,
		RepoName:   *repoName,
	}

	// Determine mode: --config for main config, --file for repo config.
	// Default to --file .deploy-bot.json if neither is specified.
	switch {
	case *configPath != "":
		return validateMainConfig(*configPath, *format)
	case *filePath != "":
		return validateRepoConfig(*filePath, *format, opts)
	default:
		return validateRepoConfig(".deploy-bot.json", *format, opts)
	}
}

// --- main config.json validation ---

type configResult struct {
	Valid  bool                     `json:"valid"`
	File   string                   `json:"file"`
	Errors []config.ValidationError `json:"errors"`
}

func validateMainConfig(path, format string) int {
	cfg, err := config.Load(path)
	if err != nil {
		if format == "json" {
			printConfigJSON(configResult{Valid: false, File: path, Errors: []config.ValidationError{
				{Section: "parse", Field: "", Msg: err.Error()},
			}})
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		return 2
	}

	errs := config.ValidateConfig(cfg)

	switch format {
	case "json":
		if errs == nil {
			errs = []config.ValidationError{}
		}
		printConfigJSON(configResult{Valid: len(errs) == 0, File: path, Errors: errs})
	default:
		printConfigText(path, cfg, errs)
	}

	if len(errs) > 0 {
		return 1
	}
	return 0
}

func printConfigJSON(r configResult) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}

func printConfigText(path string, cfg *config.Config, errs []config.ValidationError) {
	fmt.Printf("%s\n\n", path)

	sections := []struct {
		name   string
		checks []string
	}{
		{"github", []string{"org", "repo", "deployer_team", "approver_team"}},
		{"slack", []string{"deploy_channel"}},
		{"deployment", []string{"stale_duration", "lock_ttl", "merge_method"}},
		{"aws", []string{"ecr_region"}},
	}

	errMap := map[string]string{}
	for _, e := range errs {
		errMap[e.Section+"."+e.Field] = e.Msg
	}

	for _, s := range sections {
		for _, field := range s.checks {
			key := s.name + "." + field
			if msg, ok := errMap[key]; ok {
				fmt.Printf("  \u2717 %s: %s\n", key, msg)
			} else {
				fmt.Printf("  \u2713 %s\n", key)
			}
		}
	}

	// Apps
	fmt.Println()
	appErrs := map[string]string{}
	for _, e := range errs {
		if len(e.Section) > 4 && e.Section[:4] == "apps" {
			appErrs[e.Section+"."+e.Field] = e.Msg
		}
	}

	if len(cfg.Apps) == 0 {
		fmt.Println("  \u2717 apps: at least one app is required")
	} else {
		validCount := 0
		for i, app := range cfg.Apps {
			prefix := fmt.Sprintf("apps[%d]", i)
			name := app.App
			if name == "" {
				name = "?"
			}
			name += " (" + app.Environment + ")"

			var appFieldErrs []string
			for _, field := range []string{"app", "environment", "kustomize_path", "ecr_repo", "tag_pattern"} {
				key := prefix + "." + field
				if msg, ok := appErrs[key]; ok {
					appFieldErrs = append(appFieldErrs, field+": "+msg)
				}
			}
			if len(appFieldErrs) > 0 {
				for _, e := range appFieldErrs {
					fmt.Printf("  \u2717 %s %s: %s\n", prefix, name, e)
				}
			} else {
				fmt.Printf("  \u2713 %s %s\n", prefix, name)
				validCount++
			}
		}
		fmt.Println()
		fmt.Printf("%d/%d apps valid.\n", validCount, len(cfg.Apps))
	}

	// Non-app, non-section errors (e.g. duration parsing)
	for _, e := range errs {
		if len(e.Section) > 4 && e.Section[:4] == "apps" {
			continue
		}
		found := false
		for _, s := range sections {
			for _, field := range s.checks {
				if e.Section == s.name && e.Field == field {
					found = true
				}
			}
		}
		if !found {
			fmt.Printf("  \u2717 %s.%s: %s\n", e.Section, e.Field, e.Msg)
		}
	}

	fmt.Println()
	if len(errs) == 0 {
		fmt.Println("Config is valid.")
	} else {
		noun := "error"
		if len(errs) > 1 {
			noun = "errors"
		}
		fmt.Printf("%d %s found.\n", len(errs), noun)
	}
}

// --- repo-sourced .deploy-bot.json validation ---

type repoResult struct {
	Valid      bool                         `json:"valid"`
	APIVersion string                       `json:"api_version,omitempty"`
	File       string                       `json:"file"`
	AppsTotal  int                          `json:"apps_total"`
	AppsValid  int                          `json:"apps_valid"`
	Errors     []repoconfig.ValidationError `json:"errors"`
	ParseError string                       `json:"parse_error,omitempty"`
}

func validateRepoConfig(path, format string, opts repoconfig.ValidateOpts) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	cfg, err := repoconfig.Parse(data)
	if err != nil {
		if format == "json" {
			printRepoJSON(repoResult{Valid: false, File: path, ParseError: err.Error()})
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		return 2
	}

	verrs := repoconfig.ValidateWithOpts(cfg, opts)

	// Apply derived values so the output shows what the scanner will produce.
	if opts.RepoNaming && cfg.APIVersion == repoconfig.VersionV2 {
		repoconfig.ApplyRepoNaming(cfg, opts.RepoName)
	}
	validCount := len(cfg.Apps) - len(verrs)

	switch format {
	case "json":
		r := repoResult{
			Valid:      len(verrs) == 0,
			APIVersion: cfg.APIVersion,
			File:       path,
			AppsTotal:  len(cfg.Apps),
			AppsValid:  validCount,
			Errors:     verrs,
		}
		if r.Errors == nil {
			r.Errors = []repoconfig.ValidationError{}
		}
		printRepoJSON(r)
	default:
		printRepoText(path, cfg, verrs, validCount)
	}

	if len(verrs) > 0 {
		return 1
	}
	return 0
}

func printRepoJSON(r repoResult) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}

func printRepoText(file string, cfg *repoconfig.RepoConfigFile, errs []repoconfig.ValidationError, validCount int) {
	fmt.Printf("%s (%s)\n\n", file, cfg.APIVersion)

	invalid := make(map[int]*repoconfig.ValidationError, len(errs))
	for i := range errs {
		invalid[errs[i].Index] = &errs[i]
	}

	for i, app := range cfg.Apps {
		name := app.App
		if name == "" {
			name = "?"
		}
		if e, ok := invalid[i]; ok {
			fmt.Printf("  \u2717 apps[%d] (%s): %s: %s\n", i, name, e.Field, e.Msg)
		} else {
			fmt.Printf("  \u2713 apps[%d] (%s): ok\n", i, name)
		}
	}

	fmt.Println()
	if len(errs) == 0 {
		fmt.Printf("%d/%d apps valid.\n", validCount, len(cfg.Apps))
	} else {
		noun := "error"
		if len(errs) > 1 {
			noun = "errors"
		}
		fmt.Printf("%d/%d apps valid. %d %s found.\n", validCount, len(cfg.Apps), len(errs), noun)
	}
}
