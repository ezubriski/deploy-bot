package github

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	gh "github.com/google/go-github/v60/github"
	"go.uber.org/zap"
)

// fastClient returns a Client with a near-zero retry wait so tests don't block.
func fastClient(maxRetries int) *Client {
	return &Client{
		log:   zap.NewNop(),
		retry: RetryConfig{MaxRetries: maxRetries, RetryWait: time.Millisecond},
	}
}

func rateLimitErr() *gh.RateLimitError {
	return &gh.RateLimitError{
		Response: &http.Response{StatusCode: 403},
		Message:  "API rate limit exceeded",
	}
}

func abuseErr(retryAfter *time.Duration) *gh.AbuseRateLimitError {
	return &gh.AbuseRateLimitError{
		Response:   &http.Response{StatusCode: 403},
		Message:    "secondary rate limit",
		RetryAfter: retryAfter,
	}
}

func TestRetryOnRateLimit_SuccessFirstAttempt(t *testing.T) {
	c := fastClient(3)
	calls := 0
	err := c.retryOnRateLimit(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestRetryOnRateLimit_NonRateLimitError(t *testing.T) {
	c := fastClient(3)
	sentinel := errors.New("some other error")
	calls := 0
	err := c.retryOnRateLimit(context.Background(), func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry), got %d", calls)
	}
}

func TestRetryOnRateLimit_PrimaryRateLimitFailsFast(t *testing.T) {
	c := fastClient(3)
	calls := 0
	err := c.retryOnRateLimit(context.Background(), func() error {
		calls++
		return rateLimitErr()
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on primary limit), got %d", calls)
	}
}

func TestRetryOnRateLimit_SecondaryRetryThenSucceed(t *testing.T) {
	c := fastClient(3)
	calls := 0
	err := c.retryOnRateLimit(context.Background(), func() error {
		calls++
		if calls < 3 {
			return abuseErr(nil)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil after retries, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestRetryOnRateLimit_SecondaryExhaustsMaxRetries(t *testing.T) {
	c := fastClient(2)
	calls := 0
	err := c.retryOnRateLimit(context.Background(), func() error {
		calls++
		return abuseErr(nil)
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if errors.Is(err, ErrRateLimited) {
		t.Error("exhausted secondary limit should not return ErrRateLimited")
	}
	// 1 initial attempt + 2 retries = 3 total
	if calls != 3 {
		t.Errorf("expected 3 calls (1 + maxRetries), got %d", calls)
	}
}

func TestRetryOnRateLimit_RespectsRetryAfterShorterThanWait(t *testing.T) {
	// RetryAfter (1ms) < RetryWait (1h) — should use RetryAfter and not block.
	retryAfter := time.Millisecond
	c := &Client{
		log:   zap.NewNop(),
		retry: RetryConfig{MaxRetries: 1, RetryWait: time.Hour},
	}
	calls := 0
	start := time.Now()
	err := c.retryOnRateLimit(context.Background(), func() error {
		calls++
		if calls == 1 {
			return abuseErr(&retryAfter)
		}
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("expected fast retry (RetryAfter=1ms), took %v", elapsed)
	}
}

func TestRetryOnRateLimit_ContextCancelledDuringWait(t *testing.T) {
	c := &Client{
		log:   zap.NewNop(),
		retry: RetryConfig{MaxRetries: 3, RetryWait: time.Hour}, // long wait
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the wait select fires ctx.Done()

	calls := 0
	err := c.retryOnRateLimit(ctx, func() error {
		calls++
		return abuseErr(nil)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call before ctx cancel, got %d", calls)
	}
}
