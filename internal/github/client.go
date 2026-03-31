package github

import (
	"net/http"

	gh "github.com/google/go-github/v60/github"
)

// NewClientWithHTTP creates a Client using the provided HTTP client and base URL.
// baseURL must end with a slash (e.g., "http://localhost:1234/").
// Intended for testing with a custom transport or base URL.
func NewClientWithHTTP(httpClient *http.Client, baseURL, org, repo string) (*Client, error) {
	ghc, err := gh.NewClient(httpClient).WithEnterpriseURLs(baseURL, baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{gh: ghc, org: org, repo: repo}, nil
}
