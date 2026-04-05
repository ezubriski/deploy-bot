package config

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubHTTPClient_PAT(t *testing.T) {
	s := &Secrets{GitHubToken: "ghp_testtoken123"}

	client, err := s.GitHubHTTPClient()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the client sends an Authorization header with the token.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer ghp_testtoken123" {
			t.Errorf("expected Bearer token, got: %q", auth)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
}

func TestGitHubHTTPClient_AppCredentials(t *testing.T) {
	s := &Secrets{
		GitHubAppID:             12345,
		GitHubAppInstallationID: 67890,
		GitHubAppPrivateKey:     "not-a-real-key",
	}

	// With an invalid key, ghinstallation should return an error.
	_, err := s.GitHubHTTPClient()
	if err == nil {
		t.Fatal("expected error with invalid private key")
	}
}

func TestScannerHTTPClient_DedicatedToken(t *testing.T) {
	s := &Secrets{
		GitHubToken:        "primary-token",
		GitHubScannerToken: "scanner-token",
	}

	client, err := s.ScannerHTTPClient()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer scanner-token" {
			t.Errorf("expected scanner token, got: %q", auth)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
}

func TestScannerHTTPClient_FallsThroughToPrimary(t *testing.T) {
	s := &Secrets{GitHubToken: "primary-token"}

	client, err := s.ScannerHTTPClient()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer primary-token" {
			t.Errorf("expected primary token, got: %q", auth)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
}
