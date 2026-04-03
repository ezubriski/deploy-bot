package bot

import (
	"fmt"

	"github.com/slack-go/slack"

	"github.com/ezubriski/deploy-bot/internal/sanitize"
)

const (
	ModalCallbackDeploy = "deploy_modal"
	ModalCallbackReject = "reject_modal"

	BlockApp       = "block_app"
	BlockTag       = "block_tag"
	BlockTagManual = "block_tag_manual"
	BlockReason    = "block_reason"
	BlockApprover  = "block_approver"
	BlockRejReason = "block_rej_reason"

	ActionApp       = "action_app"
	ActionTag       = "action_tag"
	ActionTagManual = "action_tag_manual"
	ActionReason    = "action_reason"
	ActionApprover  = "action_approver"
	ActionRejReason = "action_rej_reason"

	ActionApprove = "action_approve"
	ActionReject  = "action_reject"
)

func buildDeployModal(appOptions []*slack.OptionBlockObject, tagOptions []*slack.OptionBlockObject, preSelectedApp, preSelectedTag, staleDuration, commandName string) slack.ModalViewRequest {
	appElement := slack.NewOptionsSelectBlockElement(
		slack.OptTypeStatic,
		slack.NewTextBlockObject("plain_text", "Select app...", false, false),
		ActionApp,
		appOptions...,
	)
	if preSelectedApp != "" {
		for _, opt := range appOptions {
			if opt.Value == preSelectedApp {
				appElement.InitialOption = opt
				break
			}
		}
	}

	tagElement := slack.NewOptionsSelectBlockElement(
		slack.OptTypeStatic,
		slack.NewTextBlockObject("plain_text", "Select tag...", false, false),
		ActionTag,
		tagOptions...,
	)
	if len(tagOptions) > 0 {
		tagElement.InitialOption = tagOptions[0]
	}

	blocks := slack.Blocks{
		BlockSet: []slack.Block{
			slack.NewInputBlock(BlockApp,
				slack.NewTextBlockObject("plain_text", "Application", false, false),
				nil,
				appElement,
			),
			slack.NewInputBlock(BlockTag,
				slack.NewTextBlockObject("plain_text", "Image Tag", false, false),
				slack.NewTextBlockObject("plain_text", "5 most recent matching tags", false, false),
				tagElement,
			),
			slack.NewInputBlock(BlockTagManual,
				slack.NewTextBlockObject("plain_text", "Manual Tag Override", false, false),
				slack.NewTextBlockObject("plain_text", fmt.Sprintf("Leave blank to use selection above. If the tag is not found a message will be posted in the deploy channel — use %s tags <app> to list valid tags.", commandName), false, false),
				func() *slack.PlainTextInputBlockElement {
					el := slack.NewPlainTextInputBlockElement(
						slack.NewTextBlockObject("plain_text", "e.g. v1.2.3", false, false),
						ActionTagManual,
					)
					if preSelectedTag != "" {
						el.InitialValue = preSelectedTag
					}
					return el
				}(),
			),
			slack.NewInputBlock(BlockReason,
				slack.NewTextBlockObject("plain_text", "Reason", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(
					slack.NewTextBlockObject("plain_text", "Why are you deploying?", false, false),
					ActionReason,
				),
			),
			slack.NewInputBlock(BlockApprover,
				slack.NewTextBlockObject("plain_text", "Approver", false, false),
				nil,
				slack.NewOptionsSelectBlockElement(
					slack.OptTypeUser,
					slack.NewTextBlockObject("plain_text", "Select approver...", false, false),
					ActionApprover,
				),
			),
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf(":warning: Deployments expire after *%s* if not approved.", staleDuration),
					false, false,
				),
				nil, nil,
			),
		},
	}

	return slack.ModalViewRequest{
		Type:          slack.VTModal,
		Title:         slack.NewTextBlockObject("plain_text", "Request Deployment", false, false),
		Submit:        slack.NewTextBlockObject("plain_text", "Submit", false, false),
		Close:         slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		CallbackID:    ModalCallbackDeploy,
		Blocks:        blocks,
		NotifyOnClose: false,
	}
}

func buildRejectModal(prNumber int, app, env, tag string) slack.ModalViewRequest {
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject("plain_text", "Reject Deployment", false, false),
		Submit:          slack.NewTextBlockObject("plain_text", "Reject", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		CallbackID:      ModalCallbackReject,
		PrivateMetadata: fmt.Sprintf("%d", prNumber),
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn",
						fmt.Sprintf("Rejecting deployment of *%s* (%s) `%s` (PR #%d)", app, env, tag, prNumber),
						false, false,
					),
					nil, nil,
				),
				slack.NewInputBlock(BlockRejReason,
					slack.NewTextBlockObject("plain_text", "Rejection Reason", false, false),
					nil,
					slack.NewPlainTextInputBlockElement(
						slack.NewTextBlockObject("plain_text", "Why are you rejecting?", false, false),
						ActionRejReason,
					),
				),
			},
		},
	}
}

func buildApproverMessage(deploy pendingInfo) []slack.MsgOption {
	text := fmt.Sprintf(
		"*Deployment Request* — <@%s> your approval is needed\n\n*App:* %s\n*Environment:* %s\n*Tag:* `%s`\n*Requester:* <@%s>\n*Reason:* %s\n*PR:* <%s|#%d>",
		deploy.ApproverID, deploy.App, deploy.Environment, deploy.Tag, deploy.RequesterID, sanitize.SlackText(deploy.Reason, 500), deploy.PRURL, deploy.PRNumber,
	)
	btnApprove := slack.NewButtonBlockElement(ActionApprove, fmt.Sprintf("%d", deploy.PRNumber),
		slack.NewTextBlockObject("plain_text", "Approve", false, false))
	btnApprove.Style = "primary"

	btnReject := slack.NewButtonBlockElement(ActionReject, fmt.Sprintf("%d", deploy.PRNumber),
		slack.NewTextBlockObject("plain_text", "Reject", false, false))
	btnReject.Style = "danger"

	return []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil,
			),
			slack.NewActionBlock("", btnApprove, btnReject),
		),
	}
}

type pendingInfo struct {
	App         string
	Environment string
	Tag         string
	PRNumber    int
	PRURL       string
	RequesterID string
	ApproverID  string
	Reason      string
}
