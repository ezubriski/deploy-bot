package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/slack-go/slack"
)

// findRollbackTag tests

func TestFindRollbackTag_TwoApproved(t *testing.T) {
	entries := []store.HistoryEntry{
		{App: "myapp", Tag: "v1.3", EventType: "approved", CompletedAt: time.Now()},
		{App: "myapp", Tag: "v1.2", EventType: "approved", CompletedAt: time.Now().Add(-time.Hour)},
	}
	current, previous, ok := findRollbackTag(entries, "myapp")
	if !ok {
		t.Fatal("expected ok=true with two approved entries")
	}
	if current != "v1.3" {
		t.Errorf("current = %q, want %q", current, "v1.3")
	}
	if previous != "v1.2" {
		t.Errorf("previous = %q, want %q", previous, "v1.2")
	}
}

func TestFindRollbackTag_NoApproved(t *testing.T) {
	entries := []store.HistoryEntry{
		{App: "myapp", Tag: "v1.3", EventType: "rejected"},
		{App: "myapp", Tag: "v1.2", EventType: "cancelled"},
	}
	_, _, ok := findRollbackTag(entries, "myapp")
	if ok {
		t.Fatal("expected ok=false with no approved entries")
	}
}

func TestFindRollbackTag_OneApproved(t *testing.T) {
	entries := []store.HistoryEntry{
		{App: "myapp", Tag: "v1.3", EventType: "approved"},
		{App: "myapp", Tag: "v1.2", EventType: "rejected"},
	}
	_, _, ok := findRollbackTag(entries, "myapp")
	if ok {
		t.Fatal("expected ok=false with only one approved entry")
	}
}

func TestFindRollbackTag_FiltersOtherApps(t *testing.T) {
	entries := []store.HistoryEntry{
		{App: "app-b", Tag: "v9.0", EventType: "approved"},
		{App: "myapp", Tag: "v1.3", EventType: "approved"},
		{App: "app-b", Tag: "v8.0", EventType: "approved"},
		{App: "myapp", Tag: "v1.2", EventType: "approved"},
	}
	current, previous, ok := findRollbackTag(entries, "myapp")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if current != "v1.3" || previous != "v1.2" {
		t.Errorf("got (%q, %q), want (\"v1.3\", \"v1.2\")", current, previous)
	}
}

func TestFindRollbackTag_EmptyHistory(t *testing.T) {
	_, _, ok := findRollbackTag(nil, "myapp")
	if ok {
		t.Fatal("expected ok=false on empty history")
	}
}

// buildDeployModal pre-filled tag tests

func TestBuildDeployModal_PreSelectedTag(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{
		SelectedApp:   "myapp",
		ManualTag:     "v1.2.3",
		StaleDuration: "2h",
		CommandName:   "/deploy",
	})

	for _, blk := range modal.Blocks.BlockSet {
		ib, ok := blk.(*slack.InputBlock)
		if !ok || ib.BlockID != BlockTagManual {
			continue
		}
		el, ok := ib.Element.(*slack.PlainTextInputBlockElement)
		if !ok {
			t.Fatal("manual tag element is not a PlainTextInputBlockElement")
		}
		if el.InitialValue != "v1.2.3" {
			t.Errorf("InitialValue = %q, want %q", el.InitialValue, "v1.2.3")
		}
		return
	}
	t.Fatal("BlockTagManual input block not found in modal")
}

func TestBuildDeployModal_NoPreSelectedTag(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{
		SelectedApp:   "myapp",
		StaleDuration: "2h",
		CommandName:   "/deploy",
	})

	for _, blk := range modal.Blocks.BlockSet {
		ib, ok := blk.(*slack.InputBlock)
		if !ok || ib.BlockID != BlockTagManual {
			continue
		}
		el, ok := ib.Element.(*slack.PlainTextInputBlockElement)
		if !ok {
			t.Fatal("manual tag element is not a PlainTextInputBlockElement")
		}
		if el.InitialValue != "" {
			t.Errorf("InitialValue = %q, want empty string when no tag pre-selected", el.InitialValue)
		}
		return
	}
	t.Fatal("BlockTagManual input block not found in modal")
}

func TestBuildDeployModal_DispatchAction(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{StaleDuration: "2h"})

	for _, blk := range modal.Blocks.BlockSet {
		ib, ok := blk.(*slack.InputBlock)
		if !ok {
			continue
		}
		switch ib.BlockID {
		case BlockAppName, BlockEnv:
			if !ib.DispatchAction {
				t.Errorf("block %s should have DispatchAction=true", ib.BlockID)
			}
		}
	}
}

func TestBuildDeployModal_TagHintWhenNoTags(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{StaleDuration: "2h"})

	for _, blk := range modal.Blocks.BlockSet {
		sec, ok := blk.(*slack.SectionBlock)
		if ok && sec.BlockID == BlockTagHint {
			return // found the hint
		}
	}
	t.Fatal("expected tag hint section when no tags provided")
}

func TestBuildDeployModal_TagSelectWhenTagsProvided(t *testing.T) {
	tags := []*slack.OptionBlockObject{
		slack.NewOptionBlockObject("v1.0.0", slack.NewTextBlockObject("plain_text", "v1.0.0", false, false), nil),
	}
	modal := buildDeployModal(DeployModalParams{
		TagOptions:    tags,
		StaleDuration: "2h",
	})

	for _, blk := range modal.Blocks.BlockSet {
		ib, ok := blk.(*slack.InputBlock)
		if ok && ib.BlockID == BlockTag {
			return // found the tag input
		}
	}
	t.Fatal("expected tag input block when tags provided")
}

func TestDeployModalState_RoundTrip(t *testing.T) {
	state := DeployModalState{SelectedApp: "nginx", SelectedEnv: "prod"}
	raw := state.Marshal()
	parsed := ParseDeployModalState(raw)
	if parsed.SelectedApp != "nginx" || parsed.SelectedEnv != "prod" {
		t.Errorf("round-trip failed: got %+v", parsed)
	}
}

func TestParseDeployModalState_Empty(t *testing.T) {
	parsed := ParseDeployModalState("")
	if parsed.SelectedApp != "" || parsed.SelectedEnv != "" {
		t.Errorf("expected empty state, got %+v", parsed)
	}
}

func TestBuildDeployModal_RollbackTitle(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{
		IsRollback:    true,
		StaleDuration: "2h",
	})
	if modal.Title.Text != "Rollback Deployment" {
		t.Errorf("title = %q, want %q", modal.Title.Text, "Rollback Deployment")
	}
}

func TestBuildDeployModal_NormalTitle(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{StaleDuration: "2h"})
	if modal.Title.Text != "Request Deployment" {
		t.Errorf("title = %q, want %q", modal.Title.Text, "Request Deployment")
	}
}

func TestBuildDeployModal_RollbackInfoSection(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{
		IsRollback:          true,
		SelectedApp:         "nginx",
		SelectedEnv:         "prod",
		RollbackCurrent:     "v1.44.2",
		RollbackCurrentDate: time.Date(2026, 4, 8, 0, 0, 0, 0, time.UTC),
		RollbackTarget:      "v1.43.0",
		RollbackTargetDate:  time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		StaleDuration:       "2h",
	})

	for _, blk := range modal.Blocks.BlockSet {
		sec, ok := blk.(*slack.SectionBlock)
		if !ok {
			continue
		}
		if sec.Text != nil && strings.Contains(sec.Text.Text, "Rolling back") {
			if !strings.Contains(sec.Text.Text, "v1.44.2") || !strings.Contains(sec.Text.Text, "v1.43.0") {
				t.Errorf("rollback info missing tag details: %s", sec.Text.Text)
			}
			if !strings.Contains(sec.Text.Text, "Apr 8") || !strings.Contains(sec.Text.Text, "Apr 1") {
				t.Errorf("rollback info missing dates: %s", sec.Text.Text)
			}
			return
		}
	}
	t.Fatal("rollback info section not found")
}

func TestBuildDeployModal_ExcludeTag(t *testing.T) {
	tags := []*slack.OptionBlockObject{
		slack.NewOptionBlockObject("v1.44.2", slack.NewTextBlockObject("plain_text", "v1.44.2", false, false), nil),
		slack.NewOptionBlockObject("v1.43.0", slack.NewTextBlockObject("plain_text", "v1.43.0", false, false), nil),
		slack.NewOptionBlockObject("v1.42.0", slack.NewTextBlockObject("plain_text", "v1.42.0", false, false), nil),
	}
	modal := buildDeployModal(DeployModalParams{
		TagOptions:    tags,
		ExcludeTag:    "v1.44.2",
		StaleDuration: "2h",
	})

	for _, blk := range modal.Blocks.BlockSet {
		ib, ok := blk.(*slack.InputBlock)
		if !ok || ib.BlockID != BlockTag {
			continue
		}
		sel, ok := ib.Element.(*slack.SelectBlockElement)
		if !ok {
			t.Fatal("tag element is not a SelectBlockElement")
		}
		for _, opt := range sel.Options {
			if opt.Value == "v1.44.2" {
				t.Error("excluded tag v1.44.2 should not appear in options")
			}
		}
		if len(sel.Options) != 2 {
			t.Errorf("expected 2 options after exclusion, got %d", len(sel.Options))
		}
		return
	}
	t.Fatal("tag input block not found")
}

func TestFindRollbackEntries(t *testing.T) {
	entries := []store.HistoryEntry{
		{App: "myapp-prod", Tag: "v1.3", EventType: "approved", CompletedAt: time.Now()},
		{App: "myapp-prod", Tag: "v1.2", EventType: "approved", CompletedAt: time.Now().Add(-time.Hour)},
	}
	cur, prev, ok := findRollbackEntries(entries, "myapp-prod")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if cur.Tag != "v1.3" || prev.Tag != "v1.2" {
		t.Errorf("got (%q, %q), want (v1.3, v1.2)", cur.Tag, prev.Tag)
	}
}

func TestDeployModalState_RollbackRoundTrip(t *testing.T) {
	state := DeployModalState{SelectedApp: "nginx", SelectedEnv: "prod", IsRollback: true}
	parsed := ParseDeployModalState(state.Marshal())
	if !parsed.IsRollback {
		t.Error("expected IsRollback=true after round-trip")
	}
}

func TestBuildDeployModal_HideManualTag(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{
		HideManualTag: true,
		StaleDuration: "2h",
	})
	for _, blk := range modal.Blocks.BlockSet {
		if ib, ok := blk.(*slack.InputBlock); ok && ib.BlockID == BlockTagManual {
			t.Fatal("manual tag block should be omitted when HideManualTag=true")
		}
	}
}

func TestBuildDeployModal_TagValidationRendered(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{
		TagValidation: ":white_check_mark: Tag `v1.2.3` found.",
		StaleDuration: "2h",
	})
	for _, blk := range modal.Blocks.BlockSet {
		sec, ok := blk.(*slack.SectionBlock)
		if !ok || sec.BlockID != BlockTagValidation {
			continue
		}
		if !strings.Contains(sec.Text.Text, "v1.2.3") {
			t.Errorf("validation section text = %q, want it to contain v1.2.3", sec.Text.Text)
		}
		return
	}
	t.Fatal("tag validation section not found")
}

func TestBuildDeployModal_TagValidationHiddenInRollback(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{
		IsRollback:    true,
		HideManualTag: true,
		TagValidation: ":x: should not appear",
		StaleDuration: "2h",
	})
	for _, blk := range modal.Blocks.BlockSet {
		if sec, ok := blk.(*slack.SectionBlock); ok && sec.BlockID == BlockTagValidation {
			t.Fatal("validation section should not render when manual tag block is hidden")
		}
	}
}

func TestBuildDeployModal_RollbackNote(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{
		IsRollback:    true,
		RollbackNote:  ":information_source: No prior deploys recorded for *myapp-prod*.",
		StaleDuration: "2h",
	})
	for _, blk := range modal.Blocks.BlockSet {
		sec, ok := blk.(*slack.SectionBlock)
		if !ok || sec.Text == nil {
			continue
		}
		if strings.Contains(sec.Text.Text, "No prior deploys recorded") {
			return
		}
	}
	t.Fatal("rollback note section not found")
}

func TestBuildDeployModal_ManualTagDispatchOnEnter(t *testing.T) {
	modal := buildDeployModal(DeployModalParams{StaleDuration: "2h"})
	for _, blk := range modal.Blocks.BlockSet {
		ib, ok := blk.(*slack.InputBlock)
		if !ok || ib.BlockID != BlockTagManual {
			continue
		}
		if !ib.DispatchAction {
			t.Error("manual tag block should have DispatchAction=true")
		}
		el, ok := ib.Element.(*slack.PlainTextInputBlockElement)
		if !ok {
			t.Fatal("manual tag element is not a PlainTextInputBlockElement")
		}
		if el.DispatchActionConfig == nil {
			t.Fatal("manual tag element missing DispatchActionConfig")
		}
		got := el.DispatchActionConfig.TriggerActionsOn
		if len(got) != 1 || got[0] != "on_enter_pressed" {
			t.Errorf("TriggerActionsOn = %v, want [on_enter_pressed]", got)
		}
		return
	}
	t.Fatal("manual tag input block not found")
}

func TestBuildFilteredModalParams_RollbackUsesHistory(t *testing.T) {
	st := newTestStore(t)
	b := newTestBot(t, &stubGH{}, &captureSlack{}, st)
	ctx := context.Background()

	now := time.Now()
	// Push three approved entries (newest first via newest-last push order
	// since PushHistory LPUSHes — see store.PushHistory).
	for _, e := range []store.HistoryEntry{
		{App: "myapp-prod", Tag: "v1.1", EventType: "approved", CompletedAt: now.Add(-2 * time.Hour)},
		{App: "myapp-prod", Tag: "v1.2", EventType: "approved", CompletedAt: now.Add(-1 * time.Hour)},
		{App: "myapp-prod", Tag: "v1.3", EventType: "approved", CompletedAt: now},
	} {
		if err := st.PushHistory(ctx, e); err != nil {
			t.Fatalf("push history: %v", err)
		}
	}

	cfg := b.cfg.Load()
	params := b.buildFilteredModalParams(ctx, cfg, "myapp", "prod", "", true)

	if params.RollbackCurrent != "v1.3" || params.RollbackTarget != "v1.2" {
		t.Errorf("rollback current/target = (%q, %q), want (v1.3, v1.2)",
			params.RollbackCurrent, params.RollbackTarget)
	}
	if params.HideManualTag != true {
		t.Error("HideManualTag should remain true when history is sufficient")
	}
	if params.RollbackNote != "" {
		t.Errorf("RollbackNote should be empty when history is sufficient, got %q", params.RollbackNote)
	}
	if params.SelectedTag != "v1.2" {
		t.Errorf("SelectedTag = %q, want v1.2 (the rollback target)", params.SelectedTag)
	}
	if len(params.TagOptions) != 3 {
		t.Errorf("expected 3 tag options from history, got %d", len(params.TagOptions))
	}
	for _, opt := range params.TagOptions {
		if !strings.Contains(opt.Text.Text, "deployed") {
			t.Errorf("tag option label %q should contain 'deployed'", opt.Text.Text)
		}
	}
}

func TestBuildFilteredModalParams_RollbackFallbackEmptyHistory(t *testing.T) {
	st := newTestStore(t)
	b := newTestBot(t, &stubGH{}, &captureSlack{}, st)
	ctx := context.Background()

	cfg := b.cfg.Load()
	params := b.buildFilteredModalParams(ctx, cfg, "myapp", "prod", "", true)

	if params.HideManualTag {
		t.Error("HideManualTag should be false when rollback history is empty (fallback path)")
	}
	if params.RollbackNote == "" {
		t.Error("RollbackNote should be set when rollback history is empty")
	}
	if !strings.Contains(params.RollbackNote, "myapp-prod") {
		t.Errorf("RollbackNote = %q, want it to mention myapp-prod", params.RollbackNote)
	}
	if params.RollbackCurrent != "" || params.RollbackTarget != "" {
		t.Error("rollback current/target should be empty in fallback path")
	}
}

func TestBuildFilteredModalParams_RollbackTagOptionsDeduped(t *testing.T) {
	st := newTestStore(t)
	b := newTestBot(t, &stubGH{}, &captureSlack{}, st)
	ctx := context.Background()

	now := time.Now()
	// v1.2 deployed twice — should appear only once.
	for _, e := range []store.HistoryEntry{
		{App: "myapp-prod", Tag: "v1.2", EventType: "approved", CompletedAt: now.Add(-3 * time.Hour)},
		{App: "myapp-prod", Tag: "v1.3", EventType: "approved", CompletedAt: now.Add(-2 * time.Hour)},
		{App: "myapp-prod", Tag: "v1.2", EventType: "approved", CompletedAt: now.Add(-1 * time.Hour)},
		{App: "myapp-prod", Tag: "v1.4", EventType: "approved", CompletedAt: now},
	} {
		if err := st.PushHistory(ctx, e); err != nil {
			t.Fatalf("push history: %v", err)
		}
	}

	cfg := b.cfg.Load()
	params := b.buildFilteredModalParams(ctx, cfg, "myapp", "prod", "", true)

	seen := map[string]int{}
	for _, opt := range params.TagOptions {
		seen[opt.Value]++
	}
	if seen["v1.2"] != 1 {
		t.Errorf("v1.2 should appear exactly once, got %d", seen["v1.2"])
	}
	if len(params.TagOptions) != 3 {
		t.Errorf("expected 3 unique tags, got %d", len(params.TagOptions))
	}
}
