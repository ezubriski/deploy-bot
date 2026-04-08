package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/observability"
)

// Event types.
const (
	EventRequested      = "requested"
	EventApproved       = "approved"
	EventRejected       = "rejected"
	EventExpired        = "expired"
	EventCancelled      = "cancelled"
	EventNoop           = "noop"
	EventConflictFailed = "conflict_failed"
	EventStartup        = "startup"
)

// Trigger sources.
const (
	TriggerSlashCommand = "slash_command"
	TriggerMention      = "mention"
	TriggerECRPush      = "ecr_push"
	TriggerSweeper      = "sweeper"
	TriggerStartup      = "startup"
)

type AuditEvent struct {
	EventType   string    `json:"event_type"`
	Trigger     string    `json:"trigger"`
	Timestamp   time.Time `json:"timestamp"`
	App         string    `json:"app,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Tag         string    `json:"tag,omitempty"`
	PRNumber    int       `json:"pr_number,omitempty"`
	PRURL       string    `json:"pr_url,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Rejection   string    `json:"rejection,omitempty"`
	ActorEmail  string    `json:"actor_email,omitempty"`
	ActorName   string    `json:"actor_name,omitempty"`
	AutoDeploy  bool      `json:"auto_deploy,omitempty"`
}

// Logger emits audit events. Events are always written to zap as structured
// log entries. When an S3 bucket is configured, events are additionally
// uploaded as JSON objects.
type Logger interface {
	Log(ctx context.Context, event AuditEvent) error
}

// NewLogger constructs a Logger. The zap sink is always active. The S3 sink
// is added when cfg.AWS.AuditBucket is set.
func NewLogger(ctx context.Context, cfg *config.Config, log *zap.Logger) (Logger, error) {
	l := &auditLogger{log: log}

	if cfg.AWS.AuditBucket != "" {
		baseCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("load aws config: %w", err)
		}
		clientCfg := baseCfg.Copy()
		clientCfg.Region = cfg.AWS.AuditRegion
		observability.InstrumentAWSConfig(&clientCfg)
		l.s3 = s3.NewFromConfig(clientCfg)
		l.bucket = cfg.AWS.AuditBucket
	} else {
		log.Info("audit: no S3 bucket configured, audit events go to application log only")
	}

	return l, nil
}

// auditLogger always emits to zap. If s3/bucket are set, it also uploads to S3.
type auditLogger struct {
	log    *zap.Logger
	s3     *s3.Client
	bucket string
}

func (l *auditLogger) Log(ctx context.Context, event AuditEvent) error {
	event.Timestamp = time.Now().UTC()

	// Always emit to zap.
	l.log.Info("audit event",
		zap.String("event_type", event.EventType),
		zap.String("trigger", event.Trigger),
		zap.String("app", event.App),
		zap.String("environment", event.Environment),
		zap.String("tag", event.Tag),
		zap.Int("pr_number", event.PRNumber),
		zap.String("pr_url", event.PRURL),
		zap.String("reason", event.Reason),
		zap.String("rejection", event.Rejection),
		zap.String("actor_email", event.ActorEmail),
		zap.String("actor_name", event.ActorName),
		zap.Bool("auto_deploy", event.AutoDeploy),
		zap.Time("timestamp", event.Timestamp),
	)

	// Optionally also write to S3.
	if l.s3 == nil {
		return nil
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}

	key := fmt.Sprintf("audit/deploys/%s/%s-pr%d-%d.json",
		event.Timestamp.Format("2006/01/02"),
		event.EventType,
		event.PRNumber,
		event.Timestamp.Unix(),
	)

	_, err = l.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(l.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		l.log.Error("audit: S3 upload failed", zap.String("key", key), zap.Error(err))
		return fmt.Errorf("put audit object: %w", err)
	}

	return nil
}
