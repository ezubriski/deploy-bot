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

	// Claim the slot atomically before posting. Use a placeholder value so
	// concurrent workers see the key exists and wait for the real timestamp.
	claimed, err := b.store.SetThreadTS(ctx, env, "pending", threadTTL)
	if err != nil {
		b.log.Error("thread: claim thread slot", zap.String("env", env), zap.Error(err))
		return ""
	}
	if !claimed {
		// Another worker is creating the thread — poll briefly for the real TS.
		for i := 0; i < 5; i++ {
			time.Sleep(200 * time.Millisecond)
			ts, _ := b.store.GetThreadTS(ctx, env)
			if ts != "" && ts != "pending" {
				return ts
			}
		}
		return "" // timed out waiting — post flat
	}

	// We won the race — post the parent message.
	deployChannel := cfg.Slack.DeployChannel
	_, parentTS, err := b.slack.PostMessageContext(ctx, deployChannel,
		slack.MsgOptionText(fmt.Sprintf(
			"Processing multiple deployment requests for *%s*. Approvals and results will be threaded here.",
			env,
		), false),
	)
	if err != nil {
		b.log.Error("thread: post parent message", zap.String("env", env), zap.Error(err))
		// Release the slot so another worker can try.
		b.store.DeleteThreadTS(ctx, env)
		return ""
	}

	// Update the placeholder with the real timestamp.
	_ = b.store.UpdateThreadTS(ctx, env, parentTS, threadTTL)

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
