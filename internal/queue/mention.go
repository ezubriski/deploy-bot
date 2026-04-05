package queue

import "github.com/slack-go/slack/socketmode"

// EventTypeAppMention is the event type for @bot mentions in channels.
const EventTypeAppMention socketmode.EventType = "app_mention"

// AppMentionEvent is the payload for an app mention event.
type AppMentionEvent struct {
	UserID   string `json:"user_id"`
	Channel  string `json:"channel"`
	Text     string `json:"text"`      // command text with mention prefix stripped
	ThreadTS string `json:"thread_ts"` // reply in thread if set
}

// NewAppMentionEvent wraps an AppMentionEvent in a socketmode.Event.
func NewAppMentionEvent(evt AppMentionEvent) socketmode.Event {
	return socketmode.Event{
		Type: EventTypeAppMention,
		Data: evt,
	}
}
