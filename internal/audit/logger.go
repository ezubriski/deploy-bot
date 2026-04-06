package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
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
	EventType    string    `json:"event_type"`
	Trigger      string    `json:"trigger"`
	Timestamp    time.Time `json:"timestamp"`
	App          string    `json:"app,omitempty"`
	Environment  string    `json:"environment,omitempty"`
	Tag          string    `json:"tag,omitempty"`
	PRNumber     int       `json:"pr_number,omitempty"`
	PRURL        string    `json:"pr_url,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Rejection    string    `json:"rejection,omitempty"`
	ActorEmail   string    `json:"actor_email,omitempty"`
	ActorName    string    `json:"actor_name,omitempty"`
	ActorSlackID string    `json:"actor_slack_id,omitempty"`
	AutoDeploy   bool      `json:"auto_deploy,omitempty"`
	MergeMethod  string    `json:"merge_method,omitempty"`
}

// Logger is the interface satisfied by both the S3-backed logger and the
// zap fallback used when no audit bucket is configured.
type Logger interface {
	Log(ctx context.Context, event AuditEvent) error
}

// NewLogger returns an S3-backed Logger when cfg.AWS.AuditBucket is set, and
// a zap-based Logger otherwise. The zap fallback writes structured audit
// entries at INFO level, which is useful in dev/staging environments that
// don't have an S3 bucket configured.
func NewLogger(ctx context.Context, cfg *config.Config, log *zap.Logger) (Logger, error) {
	if cfg.AWS.AuditBucket == "" {
		log.Info("audit: no bucket configured, writing audit events to application log")
		return &zapLogger{log: log}, nil
	}

	baseCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	clientCfg := baseCfg.Copy()
	clientCfg.Region = cfg.AWS.AuditRegion
	if cfg.AWS.AuditRoleARN != "" {
		stsClient := sts.NewFromConfig(baseCfg)
		clientCfg.Credentials = stscreds.NewAssumeRoleProvider(stsClient, cfg.AWS.AuditRoleARN)
	}

	s3Client := s3.NewFromConfig(clientCfg)

	return &s3Logger{
		s3:     s3Client,
		bucket: cfg.AWS.AuditBucket,
		log:    log,
	}, nil
}

// s3Logger writes audit events as JSON objects to S3.
type s3Logger struct {
	s3     *s3.Client
	bucket string
	log    *zap.Logger
}

func (l *s3Logger) Log(ctx context.Context, event AuditEvent) error {
	event.Timestamp = time.Now().UTC()

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
		l.log.Error("failed to write audit log", zap.String("key", key), zap.Error(err))
		return fmt.Errorf("put audit object: %w", err)
	}

	l.log.Info("audit event logged", zap.String("event", event.EventType), zap.String("key", key))
	return nil
}

// zapLogger writes audit events as structured zap log entries. Used when no
// S3 bucket is configured.
type zapLogger struct {
	log *zap.Logger
}

func (l *zapLogger) Log(_ context.Context, event AuditEvent) error {
	event.Timestamp = time.Now().UTC()
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
		zap.String("actor_slack_id", event.ActorSlackID),
		zap.Bool("auto_deploy", event.AutoDeploy),
		zap.String("merge_method", event.MergeMethod),
		zap.Time("timestamp", event.Timestamp),
	)
	return nil
}
