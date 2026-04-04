package bot

import (
	"context"
	"fmt"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const threadTTL = 10 * time.Minute

// getThreadTS returns the Slack thread timestamp for deploy notifications in
// the given environment. If threading is disabled or the threshold hasn't been
// met, it returns "" (post flat to channel). If a thread already exists for
// this environment, its timestamp is returned. Otherwise a new parent message
// is posted and its timestamp is stored atomically (SET NX) so concurrent
// workers don't create duplicate threads.
func (b *Bot) getThreadTS(ctx context.Context, env string) string {
	cfg := b.cfg.Load()
	threshold := cfg.Slack.EffectiveThreadThreshold()

	// -1 = never thread
	if threshold < 0 {
		return ""
	}

	// Check if a thread already exists for this environment.
	ts, err := b.store.GetThreadTS(ctx, env)
	if err != nil {
		b.log.Error("thread: get thread ts", zap.String("env", env), zap.Error(err))
		return ""
	}
	if ts != "" {
		return ts
	}

	// 1 = always thread, skip count check
	if threshold > 1 {
		// Count pending deploys for this environment.
		all, err := b.store.GetAll(ctx)
		if err != nil {
			b.log.Error("thread: count pending", zap.String("env", env), zap.Error(err))
			return ""
		}
		count := 0
		for _, d := range all {
			if d.Environment == env {
				count++
			}
		}
		if count < threshold {
			return ""
		}
	}

	// Post parent message.
	deployChannel := cfg.Slack.DeployChannel
	_, parentTS, err := b.slack.PostMessageContext(ctx, deployChannel,
		slack.MsgOptionText(fmt.Sprintf(
			"Processing multiple deployment requests for *%s*. Approvals and results will be threaded here.",
			env,
		), false),
	)
	if err != nil {
		b.log.Error("thread: post parent message", zap.String("env", env), zap.Error(err))
		return ""
	}

	// Atomically store the thread TS. If another worker beat us, use theirs.
	set, err := b.store.SetThreadTS(ctx, env, parentTS, threadTTL)
	if err != nil {
		b.log.Error("thread: store thread ts", zap.String("env", env), zap.Error(err))
		return parentTS
	}
	if !set {
		// Another worker already created a thread — use theirs.
		existing, _ := b.store.GetThreadTS(ctx, env)
		if existing != "" {
			return existing
		}
	}

	return parentTS
}

// threadOption returns a MsgOption that threads the message if threadTS is
// non-empty. Returns nil options if no threading.
func threadOption(threadTS string) []slack.MsgOption {
	if threadTS == "" {
		return nil
	}
	return []slack.MsgOption{slack.MsgOptionTS(threadTS)}
}
