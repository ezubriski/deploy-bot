package ecrpoller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// --- test doubles ---

type stubSQS struct {
	messages []sqstypes.Message
	deleted  []string
}

func (s *stubSQS) ReceiveMessage(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	msgs := s.messages
	s.messages = nil // drain on first call
	return &sqs.ReceiveMessageOutput{Messages: msgs}, nil
}

func (s *stubSQS) DeleteMessage(_ context.Context, params *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	s.deleted = append(s.deleted, aws.ToString(params.ReceiptHandle))
	return &sqs.DeleteMessageOutput{}, nil
}

type stubBuffer struct {
	events []socketmode.Event
}

func (b *stubBuffer) Add(evt socketmode.Event) bool {
	b.events = append(b.events, evt)
	return true
}

func makeECRPushMessage(repoName, imageTag string) sqstypes.Message {
	eb := eventBridgeEvent{
		Source:     "aws.ecr",
		DetailType: "ECR Image Action",
		Detail: ecrPushDetail{
			ActionType:     "PUSH",
			Result:         "SUCCESS",
			RepositoryName: repoName,
			ImageTag:       imageTag,
		},
	}
	data, _ := json.Marshal(eb)
	return sqstypes.Message{
		Body:          aws.String(string(data)),
		ReceiptHandle: aws.String("receipt-" + repoName + "-" + imageTag),
	}
}

func newTestPoller(t *testing.T, sqsMock *stubSQS, apps []config.AppConfig) (*Poller, *redis.Client, *store.Store) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	st := store.New(mr.Addr(), "")
	cfg := &config.Config{
		Apps: apps,
	}
	holder := config.NewHolder(cfg, "")

	return &Poller{
		sqs:      sqsMock,
		queueURL: "https://sqs.example.com/test",
		rdb:      rdb,
		buf:      &stubBuffer{},
		store:    st,
		cfg:      holder,
		log:      zap.NewNop(),
	}, rdb, st
}

func TestPoller_MatchesApp(t *testing.T) {
	sqsMock := &stubSQS{
		messages: []sqstypes.Message{
			makeECRPushMessage("myapp", "v1.0.0"),
		},
	}
	apps := []config.AppConfig{{
		App:         "myapp",
		Environment: "dev",
		ECRRepo:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
		TagPattern:  `^v\d+\.\d+\.\d+$`,
	}}

	p, rdb, _ := newTestPoller(t, sqsMock, apps)
	ctx := context.Background()

	// Init the consumer group so we can read.
	w := queue.NewWorker(rdb, zap.NewNop())
	_ = w.Init(ctx)

	p.poll(ctx)

	// Verify event was enqueued.
	msgs, err := rdb.XRange(ctx, queue.StreamKeyECR, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	// Verify SQS message was deleted.
	if len(sqsMock.deleted) != 1 {
		t.Fatalf("expected 1 deleted message, got %d", len(sqsMock.deleted))
	}
}

func TestPoller_TagPatternMismatch(t *testing.T) {
	sqsMock := &stubSQS{
		messages: []sqstypes.Message{
			makeECRPushMessage("myapp", "latest"),
		},
	}
	apps := []config.AppConfig{{
		App:         "myapp",
		Environment: "dev",
		ECRRepo:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
		TagPattern:  `^v\d+\.\d+\.\d+$`,
	}}

	p, rdb, _ := newTestPoller(t, sqsMock, apps)
	ctx := context.Background()

	p.poll(ctx)

	// No event should be enqueued.
	msgs, err := rdb.XRange(ctx, queue.StreamKeyECR, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages (tag rejected), got %d", len(msgs))
	}

	// SQS message should still be deleted (discarded).
	if len(sqsMock.deleted) != 1 {
		t.Fatalf("expected 1 deleted message, got %d", len(sqsMock.deleted))
	}
}

func TestPoller_UnknownRepo(t *testing.T) {
	sqsMock := &stubSQS{
		messages: []sqstypes.Message{
			makeECRPushMessage("unknown-app", "v1.0.0"),
		},
	}
	apps := []config.AppConfig{{
		App:         "myapp",
		Environment: "dev",
		ECRRepo:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
	}}

	p, rdb, _ := newTestPoller(t, sqsMock, apps)
	ctx := context.Background()

	p.poll(ctx)

	msgs, err := rdb.XRange(ctx, queue.StreamKeyECR, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages (unknown repo), got %d", len(msgs))
	}
}

func TestPoller_LockedApp(t *testing.T) {
	sqsMock := &stubSQS{
		messages: []sqstypes.Message{
			makeECRPushMessage("myapp", "v1.0.0"),
		},
	}
	apps := []config.AppConfig{{
		App:         "myapp",
		Environment: "dev",
		ECRRepo:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
	}}

	p, rdb, st := newTestPoller(t, sqsMock, apps)
	ctx := context.Background()

	// Pre-lock the app.
	_, _ = st.AcquireLock(ctx, "dev", "myapp", "someone", 5*60_000_000_000)

	p.poll(ctx)

	// No event should be enqueued when locked.
	msgs, err := rdb.XRange(ctx, queue.StreamKeyECR, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages (locked), got %d", len(msgs))
	}

	// SQS message should be deleted (discarded).
	if len(sqsMock.deleted) != 1 {
		t.Fatalf("expected 1 deleted message, got %d", len(sqsMock.deleted))
	}
}

func TestPoller_NonPushEvent(t *testing.T) {
	eb := eventBridgeEvent{
		Source:     "aws.ecr",
		DetailType: "ECR Image Action",
		Detail: ecrPushDetail{
			ActionType:     "DELETE",
			Result:         "SUCCESS",
			RepositoryName: "myapp",
			ImageTag:       "v1.0.0",
		},
	}
	data, _ := json.Marshal(eb)
	sqsMock := &stubSQS{
		messages: []sqstypes.Message{{
			Body:          aws.String(string(data)),
			ReceiptHandle: aws.String("receipt-delete"),
		}},
	}
	apps := []config.AppConfig{{
		App:         "myapp",
		Environment: "dev",
		ECRRepo:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
	}}

	p, rdb, _ := newTestPoller(t, sqsMock, apps)
	ctx := context.Background()

	p.poll(ctx)

	msgs, err := rdb.XRange(ctx, queue.StreamKeyECR, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages (non-push), got %d", len(msgs))
	}
}
