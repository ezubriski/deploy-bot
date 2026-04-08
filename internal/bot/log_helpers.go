package bot

import (
	"context"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// warnIfErr logs err at Warn level with op as the message and the supplied
// fields. It is a no-op when err is nil. Use this for failures that the
// system can recover from on its own (a stale label that the sweeper will
// remove next pass, a missed cosmetic comment, an undelivered status post)
// — informational, not actionable.
func (b *Bot) warnIfErr(op string, err error, fields ...zap.Field) {
	if err == nil {
		return
	}
	b.log.Warn(op, append(fields, zap.Error(err))...)
}

// errIfErr is the Error-level counterpart to warnIfErr. Reserve this for
// failures that leave persistent orphan state, lose audit records, or
// risk double-processing — anything an operator should investigate.
// Examples: store.ReleaseLock (orphan lock blocks future deploys),
// store.Delete on a pending entry (phantom in /deploy list), audit.Log
// (compliance gap), Slack ack (Slack will redeliver and the worker will
// double-process the event).
func (b *Bot) errIfErr(op string, err error, fields ...zap.Field) {
	if err == nil {
		return
	}
	b.log.Error(op, append(fields, zap.Error(err))...)
}

// postSlack wraps slack.PostMessageContext, dropping the channel and
// timestamp returns and logging any error. Use this when the caller does
// not care about the resulting message handle and just wants to send a
// notification, which is the common case for status updates and error
// notices in the bot.
func (b *Bot) postSlack(ctx context.Context, channelID, op string, options ...slack.MsgOption) {
	if _, _, err := b.slack.PostMessageContext(ctx, channelID, options...); err != nil {
		b.log.Warn("slack post: "+op, zap.String("channel", channelID), zap.Error(err))
	}
}
