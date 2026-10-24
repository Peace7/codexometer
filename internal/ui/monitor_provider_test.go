package ui

import (
	"testing"
	"time"

	"github.com/merefield/codexometer/internal/codex"
)

func TestMonitorOnlySessionsGetNoPromptOffer(t *testing.T) {
	m, _ := promptTestModel()
	if m.monitorPromptOffer().Token == "" {
		t.Fatal("precondition: Codex session should be offered a prompt")
	}
	index := m.monitorSessionIndex("root-one")
	if index < 0 {
		t.Fatal("root-one session missing")
	}
	m.monitorSessionData[index].provider = "claude"
	if !m.monitorTargetReadOnly() {
		t.Fatal("Claude session should be read-only")
	}
	if o := m.monitorPromptOffer(); o.Token != "" {
		t.Fatalf("Claude session offered prompt %+v", o)
	}
	if m.pollMonitorQueue() != nil || m.pollMonitorHistory(time.Now()) != nil {
		t.Fatal("Claude session should not be polled through Codex")
	}
}

func TestProviderIsCarriedIntoMonitorSessions(t *testing.T) {
	now := time.Unix(1000, 0)
	m := Model{meterView: viewMonitor, monitorState: monitorRunning, monitorStartedAt: now}
	m.startMonitorSessions(codex.LiveUsageSnapshot{Sessions: []codex.LiveUsageSession{{ID: "claude:a", Provider: "claude", Active: true}}}, now)
	m.syncMonitorSessions(codex.LiveUsageSnapshot{Sessions: []codex.LiveUsageSession{
		{ID: "claude:a", Provider: "claude", Active: true},
		{ID: "claude:b", Provider: "claude", Active: true, TotalTokens: 5},
	}}, now.Add(time.Second))
	for _, s := range m.monitorSessionData {
		if s.provider != "claude" {
			t.Fatalf("session %s provider = %q", s.id, s.provider)
		}
	}
}

func TestCodexShareExcludesMonitorOnlySessions(t *testing.T) {
	now := time.Unix(1000, 0)
	m := Model{monitorStartedAt: now, monitorBaseline: 0, monitorLatest: 1000,
		monitorSessionData: []monitorSession{
			{id: "codex", baseline: 0, latest: 250},
			{id: "claude:a", provider: "claude", baseline: 0, latest: 750},
		}}
	if got := m.monitorCodexShare(250); got != 1 {
		t.Fatalf("Codex share = %v, want 1", got)
	}
	if got := m.monitorSessionShare(250); got != 0.25 {
		t.Fatalf("local share = %v, want 0.25", got)
	}
}
