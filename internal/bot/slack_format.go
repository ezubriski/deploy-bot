package bot

import "fmt"

// slackMention formats a Slack user ID as a mention. If the ID is empty
// (e.g. ECR-triggered deploys with no human requester), returns a
// descriptive label instead of an empty <@> mention.
func slackMention(userID string) string {
	if userID == "" {
		return "deploy-bot (ECR)"
	}
	return fmt.Sprintf("<@%s>", userID)
}
