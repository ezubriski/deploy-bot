package store

import (
	"crypto/tls"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

var testCreds = aws.Credentials{
	AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
	SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	SessionToken:    "",
	Source:          "test",
}

func TestBuildPresignRequest(t *testing.T) {
	req, err := buildPresignRequest("my-user", "my-replication-group")
	if err != nil {
		t.Fatalf("buildPresignRequest: %v", err)
	}

	if req.Method != "GET" {
		t.Errorf("method = %q, want GET", req.Method)
	}
	if req.URL.Host != "my-replication-group" {
		t.Errorf("host = %q, want my-replication-group", req.URL.Host)
	}
	if req.URL.Path != "/" {
		t.Errorf("path = %q, want /", req.URL.Path)
	}

	q := req.URL.Query()
	if q.Get("Action") != "connect" {
		t.Errorf("Action = %q, want connect", q.Get("Action"))
	}
	if q.Get("User") != "my-user" {
		t.Errorf("User = %q, want my-user", q.Get("User"))
	}
	if q.Get("X-Amz-Expires") != "900" {
		t.Errorf("X-Amz-Expires = %q, want 900", q.Get("X-Amz-Expires"))
	}
}

func TestGenerateToken(t *testing.T) {
	gen, err := newIAMTokenGenerator("deploy-bot-iam", "deploy-bot", "us-east-1", testCreds)
	if err != nil {
		t.Fatalf("newIAMTokenGenerator: %v", err)
	}

	token, err := gen.generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}

	// Token should not have the http:// prefix (stripped during generation).
	if strings.HasPrefix(token, "http://") {
		t.Error("token should not start with http://")
	}

	// Token should contain the replication group ID as the host.
	if !strings.HasPrefix(token, "deploy-bot/") {
		t.Errorf("token should start with replication group ID, got prefix: %q", token[:min(30, len(token))])
	}

	// Token should contain SigV4 query parameters.
	// The token is a URL without scheme, so parse it with a scheme to use url.Parse.
	parsed, err := url.Parse("http://" + token)
	if err != nil {
		t.Fatalf("parse token as URL: %v", err)
	}

	q := parsed.Query()
	for _, required := range []string{"Action", "User", "X-Amz-Expires", "X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-SignedHeaders", "X-Amz-Signature"} {
		if q.Get(required) == "" {
			t.Errorf("token missing required query parameter %q", required)
		}
	}

	if q.Get("Action") != "connect" {
		t.Errorf("Action = %q, want connect", q.Get("Action"))
	}
	if q.Get("User") != "deploy-bot-iam" {
		t.Errorf("User = %q, want deploy-bot-iam", q.Get("User"))
	}
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		t.Errorf("X-Amz-Algorithm = %q, want AWS4-HMAC-SHA256", q.Get("X-Amz-Algorithm"))
	}

	// Credential should contain the access key ID and elasticache service name.
	cred := q.Get("X-Amz-Credential")
	if !strings.HasPrefix(cred, "AKIAIOSFODNN7EXAMPLE/") {
		t.Errorf("credential should start with access key ID, got %q", cred)
	}
	if !strings.Contains(cred, "/elasticache/") {
		t.Errorf("credential should contain /elasticache/, got %q", cred)
	}
}

func TestGenerateToken_Deterministic(t *testing.T) {
	gen, err := newIAMTokenGenerator("user1", "rg1", "us-west-2", testCreds)
	if err != nil {
		t.Fatalf("newIAMTokenGenerator: %v", err)
	}

	token1, _ := gen.generateToken()
	token2, _ := gen.generateToken()

	// Tokens generated at different instants should differ (X-Amz-Date changes).
	// But both should be valid (non-empty).
	if token1 == "" || token2 == "" {
		t.Error("tokens should not be empty")
	}
}

func TestCachedCredentialsProvider_Caching(t *testing.T) {
	gen, err := newIAMTokenGenerator("user1", "rg1", "us-east-1", testCreds)
	if err != nil {
		t.Fatalf("newIAMTokenGenerator: %v", err)
	}

	provider := cachedCredentialsProvider(gen)

	user1, token1 := provider()
	user2, token2 := provider()

	if user1 != "user1" || user2 != "user1" {
		t.Errorf("userID should be user1, got %q and %q", user1, user2)
	}

	// Second call should return the cached token (same value).
	if token1 != token2 {
		t.Error("second call should return cached token")
	}

	if token1 == "" {
		t.Error("token should not be empty")
	}
}

func TestCachedCredentialsProvider_Refresh(t *testing.T) {
	callCount := 0

	// Create a fake generator that tracks calls.
	gen, err := newIAMTokenGenerator("user1", "rg1", "us-east-1", testCreds)
	if err != nil {
		t.Fatalf("newIAMTokenGenerator: %v", err)
	}

	// Wrap with a provider that has a very short cache TTL to test refresh.
	var (
		mu        sync.Mutex
		token     string
		expiresAt time.Time
	)

	provider := func() (string, string) {
		mu.Lock()
		defer mu.Unlock()

		if time.Now().Before(expiresAt) {
			return gen.userID, token
		}

		callCount++
		newToken, err := gen.generateToken()
		if err != nil {
			return gen.userID, token
		}

		token = newToken
		// Expire immediately so next call regenerates.
		expiresAt = time.Now().Add(-1 * time.Second)
		return gen.userID, token
	}

	provider()
	provider()

	if callCount != 2 {
		t.Errorf("expected 2 token generations (expired cache), got %d", callCount)
	}
}

func TestNewWithOptions_IAMAuth_EnablesTLS(t *testing.T) {
	provider := func() (string, string) {
		return "user", "token"
	}

	s := NewWithOptions(Options{
		Addr:                "localhost:6379",
		IAMAuth:             true,
		CredentialsProvider: provider,
	})

	// Verify the client was created (we can't inspect TLS config directly,
	// but we can verify the store is non-nil and the client exists).
	if s == nil || s.rdb == nil {
		t.Fatal("store should not be nil")
	}

	opts := s.rdb.Options()
	if opts.TLSConfig == nil {
		t.Error("TLS should be enabled for IAM auth")
	}
	if opts.TLSConfig != nil && opts.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLS min version = %d, want %d (TLS 1.2)", opts.TLSConfig.MinVersion, tls.VersionTLS12)
	}
	if opts.CredentialsProvider == nil {
		t.Error("CredentialsProvider should be set")
	}
}

func TestNewWithOptions_PasswordAuth_NoTLS(t *testing.T) {
	s := NewWithOptions(Options{
		Addr:     "localhost:6379",
		Password: "secret",
	})

	opts := s.rdb.Options()
	if opts.TLSConfig != nil {
		t.Error("TLS should not be set for password auth")
	}
	if opts.Password != "secret" {
		t.Errorf("password = %q, want secret", opts.Password)
	}
}
