package slackclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// Poster is the subset of *slack.Client methods used by the bot and sweeper.
// Both *slack.Client and *Client implement this interface.
type Poster interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	PostEphemeralContext(ctx context.Context, channelID, userID string, options ...slack.MsgOption) (string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
	OpenViewContext(ctx context.Context, triggerID string, view slack.ModalViewRequest) (*slack.ViewResponse, error)
}

// Client wraps *slack.Client and retries on Slack 429 rate-limit responses.
type Client struct {
	*slack.Client
	maxRetries int
	retryWait  time.Duration
	log        *zap.Logger
}

// New wraps c with rate-limit retry. maxRetries is the number of additional
// attempts after the first; retryWait is the ceiling on the wait between
// retries (the Retry-After header is preferred when shorter).
func New(c *slack.Client, maxRetries int, retryWait time.Duration, log *zap.Logger) *Client {
	return &Client{
		Client:     c,
		maxRetries: maxRetries,
		retryWait:  retryWait,
		log:        log,
	}
}

func (c *Client) retryOnRateLimit(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		var rateLimited *slack.RateLimitedError
		if !errors.As(err, &rateLimited) {
			return err
		}
		if attempt >= c.maxRetries {
			return fmt.Errorf("slack rate limit after %d retries: %w", attempt, err)
		}
		wait := c.retryWait
		if rateLimited.RetryAfter > 0 && rateLimited.RetryAfter < wait {
			wait = rateLimited.RetryAfter
		}
		c.log.Warn("slack rate limit, retrying",
			zap.Duration("wait", wait),
			zap.Int("attempt", attempt+1),
			zap.Int("max_retries", c.maxRetries),
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (c *Client) PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (channel, timestamp string, err error) {
	if retryErr := c.retryOnRateLimit(ctx, func() error {
		channel, timestamp, err = c.Client.PostMessageContext(ctx, channelID, options...)
		return err
	}); retryErr != nil {
		err = retryErr
	}
	return
}

func (c *Client) PostEphemeralContext(ctx context.Context, channelID, userID string, options ...slack.MsgOption) (timestamp string, err error) {
	if retryErr := c.retryOnRateLimit(ctx, func() error {
		timestamp, err = c.Client.PostEphemeralContext(ctx, channelID, userID, options...)
		return err
	}); retryErr != nil {
		err = retryErr
	}
	return
}

func (c *Client) UpdateMessageContext(ctx context.Context, channelID, msgTimestamp string, options ...slack.MsgOption) (ch, ts, text string, err error) {
	if retryErr := c.retryOnRateLimit(ctx, func() error {
		ch, ts, text, err = c.Client.UpdateMessageContext(ctx, channelID, msgTimestamp, options...)
		return err
	}); retryErr != nil {
		err = retryErr
	}
	return
}

func (c *Client) OpenViewContext(ctx context.Context, triggerID string, view slack.ModalViewRequest) (resp *slack.ViewResponse, err error) {
	if retryErr := c.retryOnRateLimit(ctx, func() error {
		resp, err = c.Client.OpenViewContext(ctx, triggerID, view)
		return err
	}); retryErr != nil {
		err = retryErr
	}
	return
}
