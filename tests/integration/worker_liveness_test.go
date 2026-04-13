//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// TestWorkerProcessesMultipleEvents injects two events with a time gap
// and verifies both are consumed. If only the first is consumed, the
// worker goroutine is dying after processing it.
func TestWorkerProcessesMultipleEvents(t *testing.T) {
	rdb := env.store.Redis()
	ctx := context.Background()

	// Trim all prior messages so last-delivered tracking is clean.
	// The consumer group already exists; XTRIM to 0 removes messages but
	// leaves the group intact.
	rdb.XTrimMaxLen(ctx, queue.StreamKeyUser, 0)

	makeHelpEvt := func() socketmode.Event {
		return socketmode.Event{
			Type: socketmode.EventTypeSlashCommand,
			Data: slack.SlashCommand{
				Command:   "/deploy",
				Text:      "help",
				UserID:    env.requesterID,
				ChannelID: env.deployChannel,
			},
		}
	}

	// waitConsumed polls until the consumer group reports nothing pending,
	// meaning the worker ACK'd all delivered messages.
	waitConsumed := func(label string) {
		t.Helper()
		if !poll(t, 15*time.Second, func() bool {
			groups, err := rdb.XInfoGroups(ctx, queue.StreamKeyUser).Result()
			if err != nil || len(groups) == 0 {
				return false
			}
			g := groups[0]
			return g.Pending == 0
		}) {
			t.Fatalf("%s: timed out waiting for worker to consume event", label)
		}
	}

	// Event 1
	t.Log("injecting event 1")
	if err := queue.Enqueue(ctx, rdb, makeHelpEvt()); err != nil {
		t.Fatal(err)
	}
	waitConsumed("event 1")
	t.Log("event 1 consumed")

	// Event 2 — the point of the test: if the worker died after event 1,
	// this one will never be consumed.
	t.Log("injecting event 2")
	if err := queue.Enqueue(ctx, rdb, makeHelpEvt()); err != nil {
		t.Fatal(err)
	}
	waitConsumed("event 2")
	t.Log("event 2 consumed — worker is alive after processing multiple events")
}
