package store

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

const (
	tokenValiditySeconds        = "900" // 15 minutes
	iamConnectAction            = "connect"
	hexEncodedSHA256EmptyString = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	elasticacheServiceName      = "elasticache"
	tokenRefreshInterval        = 7*time.Minute + 30*time.Second
)

// iamTokenGenerator holds the state needed to generate presigned IAM auth
// tokens. Extracted from the closure to make testing possible.
type iamTokenGenerator struct {
	userID string
	region string
	req    *http.Request
	creds  aws.Credentials
	signer *v4.Signer
}

// generateToken creates a new presigned IAM auth token.
func (g *iamTokenGenerator) generateToken() (string, error) {
	signedURL, _, err := g.signer.PresignHTTP(
		context.Background(),
		g.creds,
		g.req.Clone(context.Background()),
		hexEncodedSHA256EmptyString,
		elasticacheServiceName,
		g.region,
		time.Now().UTC(),
	)
	if err != nil {
		return "", err
	}
	return strings.Replace(signedURL, "http://", "", 1), nil
}

// buildPresignRequest constructs the HTTP request used for SigV4 presigning.
func buildPresignRequest(userID, replicationGroupID string) (*http.Request, error) {
	queryParams := url.Values{
		"Action":        {iamConnectAction},
		"User":          {userID},
		"X-Amz-Expires": {tokenValiditySeconds},
	}

	authURL := url.URL{
		Host:     replicationGroupID,
		Scheme:   "http",
		Path:     "/",
		RawQuery: queryParams.Encode(),
	}

	return http.NewRequest(http.MethodGet, authURL.String(), nil)
}

// newIAMTokenGenerator creates a token generator from AWS credentials.
func newIAMTokenGenerator(userID, replicationGroupID, region string, creds aws.Credentials) (*iamTokenGenerator, error) {
	req, err := buildPresignRequest(userID, replicationGroupID)
	if err != nil {
		return nil, fmt.Errorf("build presign request: %w", err)
	}

	return &iamTokenGenerator{
		userID: userID,
		region: region,
		req:    req,
		creds:  creds,
		signer: v4.NewSigner(),
	}, nil
}

// cachedCredentialsProvider wraps a token generator with caching and returns
// a go-redis CredentialsProvider function.
func cachedCredentialsProvider(gen *iamTokenGenerator) func() (string, string) {
	var (
		mu        sync.Mutex
		token     string
		expiresAt time.Time
	)

	return func() (string, string) {
		mu.Lock()
		defer mu.Unlock()

		if time.Now().Before(expiresAt) {
			return gen.userID, token
		}

		newToken, err := gen.generateToken()
		if err != nil {
			// Return stale token on refresh failure — the Redis client
			// will surface the auth error on the next command.
			return gen.userID, token
		}

		token = newToken
		expiresAt = time.Now().Add(tokenRefreshInterval)
		return gen.userID, token
	}
}

// IAMCredentialsProvider returns a go-redis CredentialsProvider that generates
// short-lived ElastiCache IAM auth tokens via SigV4 presigned requests. Tokens
// are cached and refreshed when more than half their lifetime has elapsed.
//
// The replicationGroupID is used as the Host in the presigned request — this
// must match the ElastiCache replication group ID (not the endpoint address).
func IAMCredentialsProvider(ctx context.Context, userID, replicationGroupID string) (func() (string, string), error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config for elasticache IAM: %w", err)
	}

	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieve aws credentials: %w", err)
	}

	gen, err := newIAMTokenGenerator(userID, replicationGroupID, cfg.Region, creds)
	if err != nil {
		return nil, err
	}

	return cachedCredentialsProvider(gen), nil
}
