package bot

import (
	"context"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// warnIfErr logs err at Warn level with op as the message and the supplied
// fields. It is a no-op when err is nil. Use this to surface failures from
// fire-and-forget cleanup work (label removal, lock release, history push,
// etc.) without silently dropping the error or having to write a three-line
// if-err block at every call site.
func (b *Bot) warnIfErr(op string, err error, fields ...zap.Field) {
	if err == nil {
		return
	}
	b.log.Warn(op, append(fields, zap.Error(err))...)
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
