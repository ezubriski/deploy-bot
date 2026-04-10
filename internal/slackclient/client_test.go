package slackclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// fastClient returns a Client with a near-zero retry wait so tests don't block.
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

// rateLimitThenOK returns a test server that responds with a Slack 429 on the
// first request and okBody on all subsequent requests, along with a call counter.
// Retry-After is set to "0" so slack-go parses a RateLimitedError with a zero
// wait duration (our wrapper then uses retryWait instead).
func rateLimitThenOK(t *testing.T, okBody any) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(okBody)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newClient(t *testing.T, srvURL string, maxRetries int) *Client {
	t.Helper()
	raw := slack.New("test-token", slack.OptionAPIURL(srvURL+"/"))
	return New(raw, maxRetries, time.Millisecond, zap.NewNop())
}

// --- retryOnRateLimit unit tests ---

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

// --- wrapped method tests ---

func TestPostMessageContext_RetriesOnRateLimit(t *testing.T) {
	srv, calls := rateLimitThenOK(t, map[string]any{
		"ok": true, "channel": "C123", "ts": "111.222",
		"message": map[string]any{"text": "hi"},
	})
	c := newClient(t, srv.URL, 1)

	ch, ts, err := c.PostMessageContext(context.Background(), "C123", slack.MsgOptionText("hi", false))

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if ch != "C123" {
		t.Errorf("channel = %q, want %q", ch, "C123")
	}
	if ts != "111.222" {
		t.Errorf("timestamp = %q, want %q", ts, "111.222")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (1 rate-limited + 1 success)", n)
	}
}

func TestPostEphemeralContext_RetriesOnRateLimit(t *testing.T) {
	srv, calls := rateLimitThenOK(t, map[string]any{
		"ok": true, "message_ts": "222.333",
	})
	c := newClient(t, srv.URL, 1)

	ts, err := c.PostEphemeralContext(context.Background(), "C123", "U456", slack.MsgOptionText("hi", false))

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if ts != "222.333" {
		t.Errorf("timestamp = %q, want %q", ts, "222.333")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (1 rate-limited + 1 success)", n)
	}
}

func TestUpdateMessageContext_RetriesOnRateLimit(t *testing.T) {
	srv, calls := rateLimitThenOK(t, map[string]any{
		"ok": true, "channel": "C123", "ts": "333.444", "text": "updated",
	})
	c := newClient(t, srv.URL, 1)

	ch, ts, text, err := c.UpdateMessageContext(context.Background(), "C123", "333.444", slack.MsgOptionText("updated", false))

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if ch != "C123" {
		t.Errorf("channel = %q, want %q", ch, "C123")
	}
	if ts != "333.444" {
		t.Errorf("timestamp = %q, want %q", ts, "333.444")
	}
	if text != "updated" {
		t.Errorf("text = %q, want %q", text, "updated")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (1 rate-limited + 1 success)", n)
	}
}

func TestOpenViewContext_RetriesOnRateLimit(t *testing.T) {
	srv, calls := rateLimitThenOK(t, map[string]any{
		"ok":   true,
		"view": map[string]any{"id": "V123", "type": "modal"},
	})
	c := newClient(t, srv.URL, 1)

	resp, err := c.OpenViewContext(context.Background(), "trigger-id", slack.ModalViewRequest{
		Type:  slack.VTModal,
		Title: slack.NewTextBlockObject("plain_text", "Title", false, false),
	})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil ViewResponse")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (1 rate-limited + 1 success)", n)
	}
}

func TestUpdateViewContext_RetriesOnRateLimit(t *testing.T) {
	srv, calls := rateLimitThenOK(t, map[string]any{
		"ok":   true,
		"view": map[string]any{"id": "V123", "type": "modal"},
	})
	c := newClient(t, srv.URL, 1)

	resp, err := c.UpdateViewContext(context.Background(), slack.ModalViewRequest{
		Type:  slack.VTModal,
		Title: slack.NewTextBlockObject("plain_text", "Title", false, false),
	}, "", "hash123", "V123")

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil ViewResponse")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (1 rate-limited + 1 success)", n)
	}
}

func TestPostMessageContext_ContextCancelledReturnsCtxErr(t *testing.T) {
	// Server always 429s; context is already cancelled so retry wait fires ctx.Done.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := &Client{
		Client:     slack.New("test-token", slack.OptionAPIURL(srv.URL+"/")),
		log:        zap.NewNop(),
		maxRetries: 3,
		retryWait:  time.Hour, // long wait so ctx fires first
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := c.PostMessageContext(ctx, "C123", slack.MsgOptionText("hi", false))

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
