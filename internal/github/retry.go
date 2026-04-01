package github

import (
	"context"
	"errors"
	"fmt"
	"time"

	gh "github.com/google/go-github/v60/github"
	"go.uber.org/zap"
)

// ErrRateLimited is returned when the GitHub primary rate limit is exceeded.
// The caller should surface this to the user rather than retrying immediately,
// as the reset window can be up to an hour.
var ErrRateLimited = errors.New("github primary rate limit exceeded")

// RetryConfig controls how the client retries GitHub secondary rate limit errors.
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts on a secondary rate
	// limit (abuse detection) response. Defaults to 3.
	MaxRetries int
	// RetryWait is the maximum time to wait between retries. If GitHub's
	// Retry-After is shorter, that value is used instead. Defaults to 2m.
	RetryWait time.Duration
}

func defaultRetryConfig() RetryConfig {
	return RetryConfig{MaxRetries: 3, RetryWait: 2 * time.Minute}
}

// retryOnRateLimit executes fn, retrying on secondary rate limit errors up to
// c.retry.MaxRetries times. Primary rate limit errors are returned immediately
// as ErrRateLimited without retrying. All other errors are returned as-is.
func (c *Client) retryOnRateLimit(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}

		var rateLimitErr *gh.RateLimitError
		var abuseErr *gh.AbuseRateLimitError

		switch {
		case errors.As(err, &rateLimitErr):
			// Primary rate limit — reset can be up to an hour away, fail fast.
			return ErrRateLimited

		case errors.As(err, &abuseErr):
			// Secondary rate limit — has a Retry-After, worth waiting.
			if attempt >= c.retry.MaxRetries {
				return fmt.Errorf("github secondary rate limit after %d retries: %w", attempt, err)
			}
			wait := c.retry.RetryWait
			if abuseErr.RetryAfter != nil && *abuseErr.RetryAfter > 0 && *abuseErr.RetryAfter < wait {
				wait = *abuseErr.RetryAfter
			}
			c.log.Warn("github secondary rate limit, retrying",
				zap.Duration("wait", wait),
				zap.Int("attempt", attempt+1),
				zap.Int("max_retries", c.retry.MaxRetries),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}

		default:
			return err
		}
	}
}
