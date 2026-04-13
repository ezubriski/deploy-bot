package dynatrace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/ezubriski/deploy-bot/internal/observability"
)

// Client queries Dynatrace using the Grail DQL API with OAuth2 client
// credentials authentication.
type Client struct {
	httpClient     *http.Client
	environmentURL string
	log            *zap.Logger
}

// NewClient creates a Dynatrace API client that authenticates using
// OAuth2 client credentials. The returned client automatically refreshes
// tokens as needed.
func NewClient(environmentURL, tokenURL, clientID, clientSecret string, scopes []string, log *zap.Logger) *Client {
	ccCfg := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		Scopes:       scopes,
	}

	// Use a base transport with observability instrumentation.
	base := observability.WrapTransport(http.DefaultTransport)
	baseClient := &http.Client{Transport: base}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, baseClient)

	httpClient := ccCfg.Client(ctx)

	return &Client{
		httpClient:     httpClient,
		environmentURL: strings.TrimRight(environmentURL, "/"),
		log:            log,
	}
}

const (
	dqlQueryPath    = "/platform/storage/query/v1/query:execute"
	pollPath        = "/platform/storage/query/v1/query:poll"
	maxPollAttempts = 20
	pollBackoff     = 2 * time.Second
)

// QueryDQL executes a DQL query and returns the result. It handles the
// async polling pattern: if the initial response state is "RUNNING", it
// polls until completion or context cancellation.
func (c *Client) QueryDQL(ctx context.Context, query string) (*QueryResult, error) {
	reqBody := QueryRequest{
		Query:                 query,
		DefaultTimeframeStart: "now()-5m",
		DefaultTimeframeEnd:   "now()",
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal query request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.environmentURL+dqlQueryPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			c.log.Debug("close response body", zap.Error(cerr))
		}
	}()

	qr, err := c.decodeResponse(resp)
	if err != nil {
		return nil, err
	}

	if qr.State == "SUCCEEDED" {
		return qr.Result, nil
	}

	if qr.State != "RUNNING" {
		return nil, fmt.Errorf("unexpected query state: %s", qr.State)
	}

	return c.pollForResult(ctx, qr.RequestToken)
}

func (c *Client) pollForResult(ctx context.Context, requestToken string) (*QueryResult, error) {
	timer := time.NewTimer(pollBackoff)
	defer timer.Stop()

	for i := 0; i < maxPollAttempts; i++ {
		if i > 0 {
			timer.Reset(pollBackoff)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}

		qr, err := c.doPollRequest(ctx, requestToken)
		if err != nil {
			return nil, err
		}

		switch qr.State {
		case "SUCCEEDED":
			return qr.Result, nil
		case "RUNNING":
			c.log.Debug("dynatrace: query still running", zap.Int("attempt", i+1), zap.Int("progress", qr.Progress))
			continue
		default:
			return nil, fmt.Errorf("query failed with state: %s", qr.State)
		}
	}
	return nil, fmt.Errorf("query did not complete after %d poll attempts", maxPollAttempts)
}

func (c *Client) doPollRequest(ctx context.Context, requestToken string) (*QueryResponse, error) {
	reqBody, err := json.Marshal(map[string]string{"requestToken": requestToken})
	if err != nil {
		return nil, fmt.Errorf("marshal poll request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.environmentURL+pollPath, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create poll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll query: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			c.log.Debug("close poll response body", zap.Error(cerr))
		}
	}()

	return c.decodeResponse(resp)
}

func (c *Client) decodeResponse(resp *http.Response) (*QueryResponse, error) {
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if readErr != nil {
			return nil, fmt.Errorf("dynatrace API error (status %d), failed to read body: %w", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("dynatrace API error (status %d): %s", resp.StatusCode, string(body))
	}
	var qr QueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &qr, nil
}
