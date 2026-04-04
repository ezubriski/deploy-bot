package ecrpoller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// bufferAdder is the subset of buffer.Buffer used by the poller.
type bufferAdder interface {
	Add(evt socketmode.Event) bool
}

// Poller long-polls an SQS queue for ECR push events from EventBridge and
// enqueues matching events to Redis via the shared buffer.
type Poller struct {
	sqs          sqsClient
	queueURL     string
	pollInterval time.Duration
	rdb          *redis.Client
	buf          bufferAdder
	store        *store.Store
	cfg          *config.Holder
	log          *zap.Logger
}

// sqsClient is the subset of the SQS API used by the poller.
type sqsClient interface {
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// New creates a Poller. Returns an error if the AWS SQS client cannot be
// initialised. The poller is inactive until Run is called.
func New(ctx context.Context, rdb *redis.Client, buf bufferAdder, st *store.Store, cfg *config.Holder, log *zap.Logger) (*Poller, error) {
	c := cfg.Load()
	if c.ECREvents.SQSQueueURL == "" {
		return nil, fmt.Errorf("ecr_events.sqs_queue_url is not configured")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	sqsClient := sqs.NewFromConfig(awsCfg)

	return &Poller{
		sqs:          sqsClient,
		queueURL:     c.ECREvents.SQSQueueURL,
		pollInterval: c.ECREvents.PollIntervalDuration(),
		rdb:          rdb,
		buf:          buf,
		store:        st,
		cfg:          cfg,
		log:          log,
	}, nil
}

// Run polls SQS in a loop until ctx is cancelled. It should be started as a
// goroutine under the leader context.
func (p *Poller) Run(ctx context.Context) {
	p.log.Info("ecrpoller: starting", zap.String("queue", p.queueURL))
	for {
		select {
		case <-ctx.Done():
			p.log.Info("ecrpoller: stopped")
			return
		default:
		}
		p.poll(ctx)
	}
}

func (p *Poller) poll(ctx context.Context) {
	out, err := p.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(p.queueURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     20,
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		p.log.Error("ecrpoller: receive", zap.Error(err))
		select {
		case <-ctx.Done():
		case <-time.After(p.pollInterval):
		}
		return
	}

	for _, msg := range out.Messages {
		p.handleMessage(ctx, msg)
	}
}

func (p *Poller) handleMessage(ctx context.Context, msg sqstypes.Message) {
	body := aws.ToString(msg.Body)

	// SQS messages from EventBridge via SNS have the event in a "Message"
	// field. Direct EventBridge→SQS puts the event at the top level.
	var eb eventBridgeEvent
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		p.log.Error("ecrpoller: unmarshal event", zap.Error(err))
		p.deleteMessage(ctx, msg)
		return
	}

	// If "source" is empty, try unwrapping SNS envelope.
	if eb.Source == "" {
		var wrapper sqsBody
		if err := json.Unmarshal([]byte(body), &wrapper); err == nil && wrapper.Message != "" {
			if err := json.Unmarshal([]byte(wrapper.Message), &eb); err != nil {
				p.log.Error("ecrpoller: unmarshal SNS message", zap.Error(err))
				p.deleteMessage(ctx, msg)
				return
			}
		}
	}

	if eb.Detail.ActionType != "PUSH" || eb.Detail.Result != "SUCCESS" {
		p.log.Debug("ecrpoller: skipping non-push event",
			zap.String("action", eb.Detail.ActionType),
			zap.String("result", eb.Detail.Result),
		)
		p.deleteMessage(ctx, msg)
		return
	}

	repoName := eb.Detail.RepositoryName
	imageTag := eb.Detail.ImageTag

	cfg := p.cfg.Load()
	matchingApps := cfg.AppsByECRRepo(repoName)
	if len(matchingApps) == 0 {
		p.log.Debug("ecrpoller: no app matches repo", zap.String("repo", repoName))
		p.deleteMessage(ctx, msg)
		return
	}

	enqueued := 0
	for _, appCfg := range matchingApps {
		// Validate tag against pattern.
		if appCfg.TagPattern != "" && !appCfg.CompiledTagPattern().MatchString(imageTag) {
			p.log.Debug("ecrpoller: tag does not match pattern",
				zap.String("app", appCfg.App),
				zap.String("tag", imageTag),
				zap.String("pattern", appCfg.TagPattern),
			)
			continue
		}

		// Check deploy lock — skip if locked.
		locked, err := p.store.IsLocked(ctx, appCfg.Environment, appCfg.App)
		if err != nil {
			p.log.Error("ecrpoller: check lock", zap.String("app", appCfg.App), zap.Error(err))
			continue
		}
		if locked {
			p.log.Info("ecrpoller: app locked, skipping",
				zap.String("app", appCfg.App),
				zap.String("tag", imageTag),
			)
			continue
		}

		// Enqueue to Redis stream (via buffer on failure).
		evt := queue.NewECRPushEvent(queue.ECRPushEvent{
			App:        appCfg.App,
			Tag:        imageTag,
			Repository: appCfg.ECRRepo,
		})

		if err := queue.Enqueue(ctx, p.rdb, evt); err != nil {
			p.log.Warn("ecrpoller: enqueue failed, buffering", zap.String("app", appCfg.App), zap.Error(err))
			p.buf.Add(evt)
		}
		enqueued++
		p.log.Info("ecrpoller: event enqueued",
			zap.String("app", appCfg.App),
			zap.String("tag", imageTag),
		)
	}

	// Delete SQS message after processing all matching apps.
	p.deleteMessage(ctx, msg)
	p.log.Info("ecrpoller: processed push event",
		zap.String("repo", repoName),
		zap.String("tag", imageTag),
		zap.Int("matched", len(matchingApps)),
		zap.Int("enqueued", enqueued),
	)
}

func (p *Poller) deleteMessage(ctx context.Context, msg sqstypes.Message) {
	_, err := p.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(p.queueURL),
		ReceiptHandle: msg.ReceiptHandle,
	})
	if err != nil {
		p.log.Error("ecrpoller: delete message", zap.Error(err))
	}
}
