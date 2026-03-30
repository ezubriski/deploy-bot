package bot

import (
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/yourorg/deploy-bot/internal/store"
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
	modal := buildDeployModal(nil, nil, "myapp", "v1.2.3", "2h")

	// Find the manual tag input block and check its InitialValue.
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
	modal := buildDeployModal(nil, nil, "myapp", "", "2h")

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
