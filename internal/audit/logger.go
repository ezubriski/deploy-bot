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

const (
	EventRequested = "requested"
	EventApproved  = "approved"
	EventRejected  = "rejected"
	EventExpired   = "expired"
	EventCancelled = "cancelled"
)

type AuditEvent struct {
	EventType   string    `json:"event_type"`
	Timestamp   time.Time `json:"timestamp"`
	App         string    `json:"app"`
	Environment string    `json:"environment"`
	Tag         string    `json:"tag"`
	PRNumber  int       `json:"pr_number"`
	PRURL     string    `json:"pr_url"`
	Requester string    `json:"requester"`
	Approver  string    `json:"approver"`
	Reason    string    `json:"reason"`
	Rejection string    `json:"rejection"`
	SlackTeam string    `json:"slack_team"`
}

type Logger struct {
	s3     *s3.Client
	bucket string
	log    *zap.Logger
}

func NewLogger(ctx context.Context, cfg *config.Config, log *zap.Logger) (*Logger, error) {
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

	return &Logger{
		s3:     s3Client,
		bucket: cfg.AWS.AuditBucket,
		log:    log,
	}, nil
}

func (l *Logger) Log(ctx context.Context, event AuditEvent) error {
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
