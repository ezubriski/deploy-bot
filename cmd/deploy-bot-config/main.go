// deploy-bot-config validates .deploy-bot.json configuration files used for
// repo-sourced app discovery with deploy-bot.
//
// Usage:
//
//	deploy-bot-config [--file PATH] [--format text|json]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ezubriski/deploy-bot/internal/repoconfig"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("deploy-bot-config", flag.ContinueOnError)
	filePath := fs.String("file", ".deploy-bot.json", "path to .deploy-bot.json")
	fs.StringVar(filePath, "f", ".deploy-bot.json", "path to .deploy-bot.json (shorthand)")
	format := fs.String("format", "text", "output format: text or json")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	data, err := os.ReadFile(*filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	cfg, err := repoconfig.Parse(data)
	if err != nil {
		if *format == "json" {
			printJSON(jsonResult{Valid: false, File: *filePath, ParseError: err.Error()})
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		return 2
	}

	verrs := repoconfig.Validate(cfg)
	validCount := len(cfg.Apps) - len(verrs)

	switch *format {
	case "json":
		printJSON(buildJSONResult(*filePath, cfg, verrs, validCount))
	default:
		printText(*filePath, cfg, verrs, validCount)
	}

	if len(verrs) > 0 {
		return 1
	}
	return 0
}

type jsonResult struct {
	Valid      bool                       `json:"valid"`
	APIVersion string                     `json:"api_version,omitempty"`
	File       string                     `json:"file"`
	AppsTotal  int                        `json:"apps_total"`
	AppsValid  int                        `json:"apps_valid"`
	Errors     []repoconfig.ValidationError `json:"errors"`
	ParseError string                     `json:"parse_error,omitempty"`
}

func buildJSONResult(file string, cfg *repoconfig.RepoConfigFile, errs []repoconfig.ValidationError, validCount int) jsonResult {
	r := jsonResult{
		Valid:      len(errs) == 0,
		APIVersion: cfg.APIVersion,
		File:       file,
		AppsTotal:  len(cfg.Apps),
		AppsValid:  validCount,
		Errors:     errs,
	}
	if r.Errors == nil {
		r.Errors = []repoconfig.ValidationError{}
	}
	return r
}

func printJSON(r jsonResult) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(r)
}

func printText(file string, cfg *repoconfig.RepoConfigFile, errs []repoconfig.ValidationError, validCount int) {
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
