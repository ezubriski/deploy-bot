package bot

import (
	"fmt"

	"github.com/slack-go/slack"
)

const (
	modalCallbackDeploy = "deploy_modal"
	modalCallbackReject = "reject_modal"

	blockApp       = "block_app"
	blockTag       = "block_tag"
	blockTagManual = "block_tag_manual"
	blockReason    = "block_reason"
	blockApprover  = "block_approver"
	blockRejReason = "block_rej_reason"

	actionApp       = "action_app"
	actionTag       = "action_tag"
	actionTagManual = "action_tag_manual"
	actionReason    = "action_reason"
	actionApprover  = "action_approver"
	actionRejReason = "action_rej_reason"

	actionApprove = "action_approve"
	actionReject  = "action_reject"
)

func buildDeployModal(appOptions []*slack.OptionBlockObject, tagOptions []*slack.OptionBlockObject, preSelectedApp string, staleDuration string) slack.ModalViewRequest {
	appElement := slack.NewOptionsSelectBlockElement(
		slack.OptTypeStatic,
		slack.NewTextBlockObject("plain_text", "Select app...", false, false),
		actionApp,
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
		actionTag,
		tagOptions...,
	)
	if len(tagOptions) > 0 {
		tagElement.InitialOption = tagOptions[0]
	}

	blocks := slack.Blocks{
		BlockSet: []slack.Block{
			slack.NewInputBlock(blockApp,
				slack.NewTextBlockObject("plain_text", "Application", false, false),
				nil,
				appElement,
			),
			slack.NewInputBlock(blockTag,
				slack.NewTextBlockObject("plain_text", "Image Tag", false, false),
				slack.NewTextBlockObject("plain_text", "5 most recent matching tags", false, false),
				tagElement,
			),
			slack.NewInputBlock(blockTagManual,
				slack.NewTextBlockObject("plain_text", "Manual Tag Override", false, false),
				slack.NewTextBlockObject("plain_text", "Leave blank to use selection above", false, false),
				slack.NewPlainTextInputBlockElement(
					slack.NewTextBlockObject("plain_text", "e.g. v1.2.3", false, false),
					actionTagManual,
				),
			),
			slack.NewInputBlock(blockReason,
				slack.NewTextBlockObject("plain_text", "Reason", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(
					slack.NewTextBlockObject("plain_text", "Why are you deploying?", false, false),
					actionReason,
				),
			),
			slack.NewInputBlock(blockApprover,
				slack.NewTextBlockObject("plain_text", "Approver", false, false),
				nil,
				slack.NewOptionsSelectBlockElement(
					slack.OptTypeUser,
					slack.NewTextBlockObject("plain_text", "Select approver...", false, false),
					actionApprover,
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
		CallbackID:    modalCallbackDeploy,
		Blocks:        blocks,
		NotifyOnClose: false,
	}
}

func buildRejectModal(prNumber int, app, tag string) slack.ModalViewRequest {
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject("plain_text", "Reject Deployment", false, false),
		Submit:          slack.NewTextBlockObject("plain_text", "Reject", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		CallbackID:      modalCallbackReject,
		PrivateMetadata: fmt.Sprintf("%d", prNumber),
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn",
						fmt.Sprintf("Rejecting deployment of *%s* `%s` (PR #%d)", app, tag, prNumber),
						false, false,
					),
					nil, nil,
				),
				slack.NewInputBlock(blockRejReason,
					slack.NewTextBlockObject("plain_text", "Rejection Reason", false, false),
					nil,
					slack.NewPlainTextInputBlockElement(
						slack.NewTextBlockObject("plain_text", "Why are you rejecting?", false, false),
						actionRejReason,
					),
				),
			},
		},
	}
}

func buildApproverMessage(deploy pendingInfo) []slack.MsgOption {
	text := fmt.Sprintf(
		"*Deployment Request*\n\n*App:* %s\n*Tag:* `%s`\n*Requester:* <@%s>\n*Reason:* %s\n*PR:* <%s|#%d>",
		deploy.App, deploy.Tag, deploy.RequesterID, deploy.Reason, deploy.PRURL, deploy.PRNumber,
	)
	btnApprove := slack.NewButtonBlockElement(actionApprove, fmt.Sprintf("%d", deploy.PRNumber),
		slack.NewTextBlockObject("plain_text", "Approve", false, false))
	btnApprove.Style = "primary"

	btnReject := slack.NewButtonBlockElement(actionReject, fmt.Sprintf("%d", deploy.PRNumber),
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
	Tag         string
	PRNumber    int
	PRURL       string
	RequesterID string
	Reason      string
}
