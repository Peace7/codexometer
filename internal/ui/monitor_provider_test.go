package ui

import (
	"context"
	"testing"
	"time"

	"github.com/merefield/codexometer/internal/codex"
)

// providerTestClient implements every Codex session control the monitor polls
// or opens, so each guard is the only thing that can stop a call.
type providerTestClient struct {
	promptTestClient
	commands int
}

func (c *providerTestClient) SessionQueue(context.Context, string) ([]codex.SessionQueuedMessage, error) {
	return nil, nil
}
func (c *providerTestClient) ChangeSessionQueue(context.Context, string, codex.SessionQueuedMessage, string, bool) error {
	return nil
}
func (c *providerTestClient) SessionQueueRevision() uint64 { return 0 }
func (c *providerTestClient) SessionHistory(context.Context, string) ([]codex.SessionContext, error) {
	return nil, nil
}
func (c *providerTestClient) SessionCommands(context.Context, string, string) (codex.SessionCommandMenu, error) {
	c.commands++
	return codex.SessionCommandMenu{}, nil
}
func (c *providerTestClient) ExecuteSessionCommand(context.Context, string, string, string, string) error {
	c.commands++
	return nil
}

func providerTestModel(t *testing.T, provider string) (Model, *providerTestClient) {
	t.Helper()
	m := contextTestModel()
	c := &providerTestClient{promptTestClient: promptTestClient{offer: codex.SessionPromptOffer{Token: "prompt", ThreadID: "root-one"}}}
	m.fetcher = c
	m.monitorSessionData[m.monitorSessionIndex("root-one")].provider = provider
	m.openMonitorContext("root-one")
	return m, c
}

func TestMonitorOnlySessionsGetNoCodexControls(t *testing.T) {
	codexModel, codexClient := providerTestModel(t, "")
	if codexModel.monitorPromptOffer().Token == "" {
		t.Fatal("control: Codex session should be offered a prompt")
	}
	if codexModel.pollMonitorQueue() == nil || codexModel.pollMonitorHistory(time.Now()) == nil {
		t.Fatal("control: Codex session should be polled")
	}
	updated, _, _ := codexModel.openMonitorCommands("")
	if cmd := updated.monitorCommands; !cmd.busy {
		t.Fatal("control: Codex session should load its command catalogue")
	}
	_ = codexClient

	m, c := providerTestModel(t, "claude")
	if !m.monitorTargetReadOnly() {
		t.Fatal("Claude session should be read-only")
	}
	if o := m.monitorPromptOffer(); o.Token != "" {
		t.Fatalf("Claude session offered prompt %+v", o)
	}
	if m.pollMonitorQueue() != nil || m.pollMonitorHistory(time.Now()) != nil {
		t.Fatal("Claude session should not be polled through Codex")
	}
	updated, cmd, _ := m.openMonitorCommands("")
	if cmd != nil || updated.monitorCommands.busy || c.commands != 0 {
		t.Fatal("Claude session loaded Codex session commands")
	}
	updated, _, _ = m.openMonitorCommands("model")
	if updated.monitorPrompt.notice != "Claude sessions are monitor-only." || c.commands != 0 {
		t.Fatalf("notice = %q commands = %d", updated.monitorPrompt.notice, c.commands)
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
