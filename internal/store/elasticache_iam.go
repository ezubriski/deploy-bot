package store

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

const (
	tokenValiditySeconds        = "900" // 15 minutes
	iamConnectAction            = "connect"
	hexEncodedSHA256EmptyString = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

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

	region := cfg.Region

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

	req, err := http.NewRequest(http.MethodGet, authURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build presign request: %w", err)
	}

	signer := v4.NewSigner()

	var (
		mu        sync.Mutex
		token     string
		expiresAt time.Time
	)

	return func() (string, string) {
		mu.Lock()
		defer mu.Unlock()

		if time.Now().Before(expiresAt) {
			return userID, token
		}

		signedURL, _, err := signer.PresignHTTP(
			context.Background(),
			creds,
			req.Clone(context.Background()),
			hexEncodedSHA256EmptyString,
			"elasticache",
			region,
			time.Now().UTC(),
		)
		if err != nil {
			// Return stale token on refresh failure — the Redis client
			// will surface the auth error on the next command.
			return userID, token
		}

		token = strings.Replace(signedURL, "http://", "", 1)
		// Tokens are valid for 15 minutes. Refresh at the halfway mark
		// to avoid edge-of-expiry failures.
		expiresAt = time.Now().Add(7*time.Minute + 30*time.Second)
		return userID, token
	}, nil
}
