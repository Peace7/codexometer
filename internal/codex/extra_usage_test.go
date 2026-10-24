package codex

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubUsageSource struct {
	usage LiveUsageSnapshot
	err   error
}

func (s stubUsageSource) FetchTokenUsage(context.Context) (LiveUsageSnapshot, error) {
	return s.usage, s.err
}

func TestMergeExtraUsageAddsSessionsAndSkipsFailures(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	base := LiveUsageSnapshot{TotalTokens: 100, SessionCount: 1, LastActivity: at, CodexUp: true, APIEqUSD: 1.5,
		Sessions: []LiveUsageSession{{ID: "codex-thread", TotalTokens: 100}}}
	extra := LiveUsageSnapshot{TotalTokens: 40, SessionCount: 2, LastActivity: at.Add(time.Minute),
		Sessions: []LiveUsageSession{{ID: "claude:a", Provider: "claude", TotalTokens: 40}}}
	merged := mergeExtraUsage(context.Background(), base, []UsageSource{
		stubUsageSource{usage: extra},
		stubUsageSource{err: errors.New("unreadable")},
		nil,
	})
	if merged.TotalTokens != 140 || merged.SessionCount != 3 || !merged.LastActivity.Equal(at.Add(time.Minute)) {
		t.Fatalf("merged totals = %+v", merged)
	}
	if len(merged.Sessions) != 2 || merged.Sessions[1].Provider != "claude" {
		t.Fatalf("sessions = %+v", merged.Sessions)
	}
	if !merged.CodexUp || merged.APIEqUSD != 1.5 {
		t.Fatal("Codex status and accounting must be preserved")
	}
}

func TestClientWithOnlyExtraUsageMonitors(t *testing.T) {
	client := Client{ExtraUsage: []UsageSource{stubUsageSource{usage: LiveUsageSnapshot{TotalTokens: 7}}}}
	usage, err := client.FetchTokenUsage(context.Background())
	if err != nil || usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v err = %v", usage, err)
	}
	if _, err := (Client{}).FetchTokenUsage(context.Background()); err == nil {
		t.Fatal("expected an error with no telemetry sources")
	}
}
