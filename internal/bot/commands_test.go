package bot

import (
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
