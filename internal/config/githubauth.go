package config

import (
	"context"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	ghv84 "github.com/google/go-github/v84/github"
	"golang.org/x/oauth2"
)

// newAppTransport creates a ghinstallation transport with optional token scoping.
// If opts is nil, the token inherits the full installation permissions.
func (s *Secrets) newAppTransport(opts *ghv84.InstallationTokenOptions) (*ghinstallation.Transport, error) {
	itr, err := ghinstallation.New(
		http.DefaultTransport,
		s.GitHubAppID,
		s.GitHubAppInstallationID,
		[]byte(s.GitHubAppPrivateKey),
	)
	if err != nil {
		return nil, err
	}
	if opts != nil {
		itr.InstallationTokenOptions = opts
	}
	return itr, nil
}

func (s *Secrets) patClient(token string) *http.Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	return oauth2.NewClient(context.Background(), ts)
}

func ptr(s string) *string { return &s }

// GitHubHTTPClient returns an *http.Client for the worker (bot + sweeper).
// Scoped to: contents (rw), pull requests (rw), issues (rw), members (read),
// restricted to the gitops repository.
func (s *Secrets) GitHubHTTPClient(gitopsRepo string) (*http.Client, error) {
	if s.UseGitHubApp() {
		itr, err := s.newAppTransport(&ghv84.InstallationTokenOptions{
			Repositories: []string{gitopsRepo},
			Permissions: &ghv84.InstallationPermissions{
				Contents:     ptr("write"),
				PullRequests: ptr("write"),
				Issues:       ptr("write"),
				Members:      ptr("read"),
			},
		})
		if err != nil {
			return nil, err
		}
		return &http.Client{Transport: itr}, nil
	}
	return s.patClient(s.GitHubToken), nil
}

// ValidatorHTTPClient returns an *http.Client for identity resolution.
// Scoped to: members (read) only.
func (s *Secrets) ValidatorHTTPClient() (*http.Client, error) {
	if s.UseGitHubApp() {
		itr, err := s.newAppTransport(&ghv84.InstallationTokenOptions{
			Permissions: &ghv84.InstallationPermissions{
				Members: ptr("read"),
			},
		})
		if err != nil {
			return nil, err
		}
		return &http.Client{Transport: itr}, nil
	}
	return s.patClient(s.GitHubToken), nil
}

// ApproverHTTPClient returns an *http.Client for the approver cache.
// Scoped to: members (read) only.
func (s *Secrets) ApproverHTTPClient() (*http.Client, error) {
	return s.ValidatorHTTPClient()
}

// ScannerHTTPClient returns an *http.Client for repo scanning. If a dedicated
// scanner token is configured, it is used (as a PAT). Otherwise, a scoped
// App token is used: contents (read), commit statuses (rw).
func (s *Secrets) ScannerHTTPClient() (*http.Client, error) {
	if s.GitHubScannerToken != "" {
		return s.patClient(s.GitHubScannerToken), nil
	}
	if s.UseGitHubApp() {
		itr, err := s.newAppTransport(&ghv84.InstallationTokenOptions{
			Permissions: &ghv84.InstallationPermissions{
				Contents: ptr("read"),
				Statuses: ptr("write"),
			},
		})
		if err != nil {
			return nil, err
		}
		return &http.Client{Transport: itr}, nil
	}
	return s.patClient(s.GitHubToken), nil
}
