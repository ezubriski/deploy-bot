package slackclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

func fastClient(maxRetries int) *Client {
	return &Client{
		log:        zap.NewNop(),
		maxRetries: maxRetries,
		retryWait:  time.Millisecond,
	}
}

func rateLimitedErr(retryAfter time.Duration) *slack.RateLimitedError {
	return &slack.RateLimitedError{RetryAfter: retryAfter}
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

func TestRetryOnRateLimit_RetryThenSucceed(t *testing.T) {
	c := fastClient(3)
	calls := 0
	err := c.retryOnRateLimit(context.Background(), func() error {
		calls++
		if calls < 3 {
			return rateLimitedErr(time.Millisecond)
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

func TestRetryOnRateLimit_ExhaustsMaxRetries(t *testing.T) {
	c := fastClient(2)
	calls := 0
	err := c.retryOnRateLimit(context.Background(), func() error {
		calls++
		return rateLimitedErr(time.Millisecond)
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// 1 initial attempt + 2 retries = 3 total
	if calls != 3 {
		t.Errorf("expected 3 calls (1 + maxRetries), got %d", calls)
	}
}

func TestRetryOnRateLimit_RespectsRetryAfterShorterThanWait(t *testing.T) {
	c := &Client{
		log:        zap.NewNop(),
		maxRetries: 1,
		retryWait:  time.Hour, // long ceiling
	}
	calls := 0
	start := time.Now()
	err := c.retryOnRateLimit(context.Background(), func() error {
		calls++
		if calls == 1 {
			return rateLimitedErr(time.Millisecond) // short RetryAfter
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
		log:        zap.NewNop(),
		maxRetries: 3,
		retryWait:  time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	err := c.retryOnRateLimit(ctx, func() error {
		calls++
		return rateLimitedErr(time.Hour)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call before ctx cancel, got %d", calls)
	}
}
