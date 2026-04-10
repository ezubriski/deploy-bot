package bot

import (
	"encoding/json"
	"fmt"

	"github.com/slack-go/slack"

	"github.com/ezubriski/deploy-bot/internal/sanitize"
)

const (
	ModalCallbackDeploy = "deploy_modal"
	ModalCallbackReject = "reject_modal"

	BlockAppName   = "block_app_name"
	BlockEnv       = "block_env"
	BlockTag       = "block_tag"
	BlockTagHint   = "block_tag_hint"
	BlockTagManual = "block_tag_manual"
	BlockReason    = "block_reason"
	BlockApprover  = "block_approver"
	BlockRejReason = "block_rej_reason"

	ActionAppName   = "action_app_name"
	ActionEnv       = "action_env"
	ActionTag       = "action_tag"
	ActionTagManual = "action_tag_manual"
	ActionReason    = "action_reason"
	ActionApprover  = "action_approver"
	ActionRejReason = "action_rej_reason"

	ActionApprove = "action_approve"
	ActionReject  = "action_reject"

	// Deprecated: kept for backward compatibility during rollout.
	BlockApp  = "block_app"
	ActionApp = "action_app"
)

// DeployModalState is stored in the modal's PrivateMetadata as JSON.
// It tracks the current filter selections so they survive view updates.
type DeployModalState struct {
	SelectedApp string `json:"app,omitempty"`
	SelectedEnv string `json:"env,omitempty"`
}

func (s DeployModalState) Marshal() string {
	data, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func ParseDeployModalState(raw string) DeployModalState {
	var s DeployModalState
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return DeployModalState{}
		}
	}
	return s
}

// DeployModalParams holds all inputs for building the deploy modal.
type DeployModalParams struct {
	AppNameOptions []*slack.OptionBlockObject
	EnvOptions     []*slack.OptionBlockObject
	TagOptions     []*slack.OptionBlockObject
	SelectedApp    string
	SelectedEnv    string
	SelectedTag    string
	ManualTag      string
	Reason         string
	Approver       string
	StaleDuration  string
	CommandName    string
}

// ModalValues provides safe access to Slack modal view state values,
// returning zero values instead of panicking on missing blocks or actions.
type ModalValues map[string]map[string]slack.BlockAction

// SelectedOption returns the selected option value for a static select block.
func (m ModalValues) SelectedOption(block, action string) string {
	if m == nil {
		return ""
	}
	actions, ok := m[block]
	if !ok {
		return ""
	}
	a, ok := actions[action]
	if !ok {
		return ""
	}
	return a.SelectedOption.Value
}

// Text returns the plain text value for an input block.
func (m ModalValues) Text(block, action string) string {
	if m == nil {
		return ""
	}
	actions, ok := m[block]
	if !ok {
		return ""
	}
	a, ok := actions[action]
	if !ok {
		return ""
	}
	return a.Value
}

// SelectedUser returns the selected user ID for a user select block.
func (m ModalValues) SelectedUser(block, action string) string {
	if m == nil {
		return ""
	}
	actions, ok := m[block]
	if !ok {
		return ""
	}
	a, ok := actions[action]
	if !ok {
		return ""
	}
	return a.SelectedUser
}

func buildDeployModal(p DeployModalParams) slack.ModalViewRequest {
	state := DeployModalState{
		SelectedApp: p.SelectedApp,
		SelectedEnv: p.SelectedEnv,
	}

	// App Name select with dispatch_action
	appElement := slack.NewOptionsSelectBlockElement(
		slack.OptTypeStatic,
		slack.NewTextBlockObject("plain_text", "Select app...", false, false),
		ActionAppName,
		p.AppNameOptions...,
	)
	if p.SelectedApp != "" {
		for _, opt := range p.AppNameOptions {
			if opt.Value == p.SelectedApp {
				appElement.InitialOption = opt
				break
			}
		}
	}
	appBlock := slack.NewInputBlock(BlockAppName,
		slack.NewTextBlockObject("plain_text", "Application", false, false),
		nil,
		appElement,
	).WithDispatchAction(true)

	// Environment select with dispatch_action
	envElement := slack.NewOptionsSelectBlockElement(
		slack.OptTypeStatic,
		slack.NewTextBlockObject("plain_text", "Select environment...", false, false),
		ActionEnv,
		p.EnvOptions...,
	)
	if p.SelectedEnv != "" {
		for _, opt := range p.EnvOptions {
			if opt.Value == p.SelectedEnv {
				envElement.InitialOption = opt
				break
			}
		}
	}
	envBlock := slack.NewInputBlock(BlockEnv,
		slack.NewTextBlockObject("plain_text", "Environment", false, false),
		nil,
		envElement,
	).WithDispatchAction(true)

	blocks := []slack.Block{appBlock, envBlock}

	// Tag select — only shown when both app and env are selected
	if len(p.TagOptions) > 0 {
		tagElement := slack.NewOptionsSelectBlockElement(
			slack.OptTypeStatic,
			slack.NewTextBlockObject("plain_text", "Select tag...", false, false),
			ActionTag,
			p.TagOptions...,
		)
		if p.SelectedTag != "" {
			for _, opt := range p.TagOptions {
				if opt.Value == p.SelectedTag {
					tagElement.InitialOption = opt
					break
				}
			}
		} else if len(p.TagOptions) > 0 {
			tagElement.InitialOption = p.TagOptions[0]
		}
		blocks = append(blocks, slack.NewInputBlock(BlockTag,
			slack.NewTextBlockObject("plain_text", "Image Tag", false, false),
			slack.NewTextBlockObject("plain_text", "5 most recent matching tags", false, false),
			tagElement,
		))
	} else {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn",
				"_Select both an application and environment to see available tags._",
				false, false,
			),
			nil, nil,
			slack.SectionBlockOptionBlockID(BlockTagHint),
		))
	}

	// Manual tag override
	manualTagEl := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject("plain_text", "e.g. v1.2.3", false, false),
		ActionTagManual,
	)
	if p.ManualTag != "" {
		manualTagEl.InitialValue = p.ManualTag
	}
	commandHint := p.CommandName
	if commandHint == "" {
		commandHint = "/deploy"
	}
	blocks = append(blocks, slack.NewInputBlock(BlockTagManual,
		slack.NewTextBlockObject("plain_text", "Manual Tag Override", false, false),
		slack.NewTextBlockObject("plain_text",
			fmt.Sprintf("Leave blank to use selection above. If the tag is not found a message will be posted in the deploy channel — use %s tags <app> to list valid tags.", commandHint),
			false, false),
		manualTagEl,
	).WithOptional(true))

	// Reason
	reasonEl := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject("plain_text", "Why are you deploying?", false, false),
		ActionReason,
	)
	if p.Reason != "" {
		reasonEl.InitialValue = p.Reason
	}
	blocks = append(blocks, slack.NewInputBlock(BlockReason,
		slack.NewTextBlockObject("plain_text", "Reason", false, false),
		nil,
		reasonEl,
	))

	// Approver
	approverEl := slack.NewOptionsSelectBlockElement(
		slack.OptTypeUser,
		slack.NewTextBlockObject("plain_text", "Select approver...", false, false),
		ActionApprover,
	)
	if p.Approver != "" {
		approverEl.InitialUser = p.Approver
	}
	blocks = append(blocks, slack.NewInputBlock(BlockApprover,
		slack.NewTextBlockObject("plain_text", "Approver", false, false),
		nil,
		approverEl,
	))

	// Warning
	staleDur := p.StaleDuration
	if staleDur == "" {
		staleDur = "2h"
	}
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn",
			fmt.Sprintf(":warning: Deployments expire after *%s* if not approved.", staleDur),
			false, false,
		),
		nil, nil,
	))

	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject("plain_text", "Request Deployment", false, false),
		Submit:          slack.NewTextBlockObject("plain_text", "Submit", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		CallbackID:      ModalCallbackDeploy,
		PrivateMetadata: state.Marshal(),
		Blocks:          slack.Blocks{BlockSet: blocks},
		NotifyOnClose:   false,
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
		"*Deployment Request* — %s your approval is needed\n\n*App:* %s\n*Environment:* %s\n*Tag:* `%s`\n*Requester:* %s\n*Reason:* %s\n*PR:* <%s|#%d>",
		slackMention(deploy.ApproverID), deploy.App, deploy.Environment, deploy.Tag, slackMention(deploy.RequesterID), sanitize.SlackText(deploy.Reason, 500), deploy.PRURL, deploy.PRNumber,
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
