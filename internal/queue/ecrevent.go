package queue

import "github.com/slack-go/slack/socketmode"

// EventTypeECRPush is the event type for ECR image push notifications.
// Defined as a socketmode.EventType so it can be carried by the same
// envelope, buffer, and worker infrastructure as Slack events.
const EventTypeECRPush socketmode.EventType = "ecr_push"

// ECRPushEvent is the payload for an ECR push-triggered deploy event.
type ECRPushEvent struct {
	App        string `json:"app"`        // matched app name from config
	Tag        string `json:"tag"`        // pushed image tag
	Repository string `json:"repository"` // full ECR repository URI
}

// NewECRPushEvent wraps an ECRPushEvent in a socketmode.Event so it can be
// enqueued and buffered using the same path as Slack events.
func NewECRPushEvent(evt ECRPushEvent) socketmode.Event {
	return socketmode.Event{
		Type: EventTypeECRPush,
		Data: evt,
	}
}
