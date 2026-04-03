package ecrpoller

// eventBridgeEnvelope is the outer wrapper for an EventBridge event delivered
// via SQS. SQS wraps the EventBridge JSON in a "Message" field inside its
// own JSON body.
type sqsBody struct {
	Message string `json:"Message"`
}

// eventBridgeEvent represents the EventBridge "ECR Image Action" event.
type eventBridgeEvent struct {
	Source     string         `json:"source"`
	DetailType string        `json:"detail-type"`
	Detail     ecrPushDetail `json:"detail"`
}

// ecrPushDetail contains the fields we care about from the ECR push event.
type ecrPushDetail struct {
	ActionType     string `json:"action-type"`
	Result         string `json:"result"`
	RepositoryName string `json:"repository-name"`
	ImageTag       string `json:"image-tag"`
	RegistryID     string `json:"registry-id"`
}
