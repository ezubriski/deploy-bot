package config

import (
	"context"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"golang.org/x/oauth2"
)

// GitHubHTTPClient returns an *http.Client configured for GitHub API access.
// If GitHub App credentials are set, the client uses automatic installation
// token generation and refresh. Otherwise, it uses the static PAT.
func (s *Secrets) GitHubHTTPClient() (*http.Client, error) {
	if s.UseGitHubApp() {
		itr, err := ghinstallation.New(
			http.DefaultTransport,
			s.GitHubAppID,
			s.GitHubAppInstallationID,
			[]byte(s.GitHubAppPrivateKey),
		)
		if err != nil {
			return nil, err
		}
		return &http.Client{Transport: itr}, nil
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: s.GitHubToken})
	return oauth2.NewClient(context.Background(), ts), nil
}

// ScannerHTTPClient returns an *http.Client for repo scanning. If a dedicated
// scanner token is configured, it is used (as a PAT). Otherwise, the primary
// GitHub auth (App or PAT) is used.
func (s *Secrets) ScannerHTTPClient() (*http.Client, error) {
	if s.GitHubScannerToken != "" {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: s.GitHubScannerToken})
		return oauth2.NewClient(context.Background(), ts), nil
	}
	return s.GitHubHTTPClient()
}
