package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/merefield/codexometer/internal/codex"
)

const testSession = "11111111-2222-3333-4444-555555555555"

type testClock struct{ at time.Time }

func (c *testClock) now() time.Time { return c.at }

func assistantLine(t *testing.T, at time.Time, messageID string, input, cacheWrite, cacheRead, output int64, content ...map[string]any) string {
	t.Helper()
	record := map[string]any{
		"type": "assistant", "timestamp": at.UTC().Format(time.RFC3339Nano), "sessionId": testSession,
		"cwd": `F:\Work`, "effort": "high",
		"message": map[string]any{
			"id": messageID, "model": "claude-opus-5-5", "content": content,
			"usage": map[string]int64{
				"input_tokens": input, "cache_creation_input_tokens": cacheWrite,
				"cache_read_input_tokens": cacheRead, "output_tokens": output,
			},
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func newTestReader(t *testing.T, clock *testClock) (*Reader, string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(home)
	if err != nil {
		t.Fatal(err)
	}
	r.now = clock.now
	r.alive = func(int) bool { return true }
	return r, filepath.Join(home, "projects", "F--Work")
}

func fetch(t *testing.T, r *Reader) codex.LiveUsageSnapshot {
	t.Helper()
	usage, err := r.FetchTokenUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func TestNewReaderRequiresProjectsDirectory(t *testing.T) {
	if _, err := NewReader(t.TempDir()); err == nil {
		t.Fatal("expected an error without a projects directory")
	}
}

func TestContentBlocksOfOneMessageCountOnce(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	path := filepath.Join(project, testSession+".jsonl")
	at := clock.at.Add(-time.Minute)
	appendFile(t, path,
		assistantLine(t, at, "msg_1", 2, 100, 1000, 50, map[string]any{"type": "thinking", "thinking": "secret"})+
			assistantLine(t, at, "msg_1", 2, 100, 1000, 50, map[string]any{"type": "text", "text": "Done."})+
			assistantLine(t, at.Add(time.Second), "msg_2", 1, 0, 1152, 7, map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "go test ./...\nsecond line"}}))

	usage := fetch(t, r)
	if len(usage.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(usage.Sessions))
	}
	s := usage.Sessions[0]
	if want := int64(2 + 100 + 1000 + 50 + 1 + 1152 + 7); s.TotalTokens != want || usage.TotalTokens != want {
		t.Fatalf("total = %d/%d, want %d", s.TotalTokens, usage.TotalTokens, want)
	}
	if s.ID != idPrefix+testSession || s.Provider != Provider {
		t.Fatalf("id/provider = %q/%q", s.ID, s.Provider)
	}
	if len(s.ModelCalls) != 2 || s.ModelCalls[0].OutputTokens != 50 || s.ModelCalls[1].OutputTokens != 7 {
		t.Fatalf("model calls = %+v", s.ModelCalls)
	}
	if s.ModelSettings.Model != "claude-opus-5-5" || s.ModelSettings.ReasoningEffort != "high" || s.WorkingDirectory != `F:\Work` {
		t.Fatalf("settings = %+v cwd = %q", s.ModelSettings, s.WorkingDirectory)
	}
	if s.Context.Kind != codex.SessionContextActivity || s.Context.Text != "Bash: go test ./..." {
		t.Fatalf("context = %v %q", s.Context.Kind, s.Context.Text)
	}
}

func TestIncrementalReadsWaitForCompleteLines(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	path := filepath.Join(project, testSession+".jsonl")
	first := assistantLine(t, clock.at, "msg_1", 10, 0, 0, 5)
	second := assistantLine(t, clock.at, "msg_2", 20, 0, 0, 5)
	appendFile(t, path, first+second[:len(second)/2])
	if got := fetch(t, r).TotalTokens; got != 15 {
		t.Fatalf("total with partial line = %d, want 15", got)
	}
	appendFile(t, path, second[len(second)/2:])
	clock.at = clock.at.Add(time.Second)
	if got := fetch(t, r).TotalTokens; got != 40 {
		t.Fatalf("total after completion = %d, want 40", got)
	}
	if calls := len(fetch(t, r).Sessions[0].ModelCalls); calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestSubagentsFoldIntoParent(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	appendFile(t, filepath.Join(project, testSession+".jsonl"),
		assistantLine(t, clock.at, "msg_root", 10, 0, 0, 0, map[string]any{"type": "text", "text": "root reply"}))
	for _, agent := range []string{"agent-a", "agent-b"} {
		appendFile(t, filepath.Join(project, testSession, "subagents", agent+".jsonl"),
			assistantLine(t, clock.at, "msg_"+agent, 100, 0, 0, 0, map[string]any{"type": "text", "text": "child reply"}))
	}
	usage := fetch(t, r)
	if len(usage.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(usage.Sessions))
	}
	s := usage.Sessions[0]
	if s.TotalTokens != 210 || s.AgentCount != 2 {
		t.Fatalf("total/agents = %d/%d, want 210/2", s.TotalTokens, s.AgentCount)
	}
	if s.Context.Text != "root reply" {
		t.Fatalf("context = %q, want root reply", s.Context.Text)
	}
}

func TestFilesAppearingLaterSkipCopiedHistory(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	fetch(t, r)
	clock.at = clock.at.Add(time.Minute)
	appendFile(t, filepath.Join(project, testSession+".jsonl"),
		assistantLine(t, clock.at.Add(-48*time.Hour), "msg_old", 1000, 0, 0, 0)+
			assistantLine(t, clock.at, "msg_new", 30, 0, 0, 0))
	if got := fetch(t, r).TotalTokens; got != 30 {
		t.Fatalf("total = %d, want only post-start usage 30", got)
	}
}

func TestRegistryDrivesNameAndWorking(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	appendFile(t, filepath.Join(project, testSession+".jsonl"), assistantLine(t, clock.at.Add(-time.Hour), "msg_1", 1, 0, 0, 0))
	registry := `{"pid":4242,"sessionId":"` + testSession + `","cwd":"F:\\Work","name":"defect backlog","status":"busy"}`
	appendFile(t, filepath.Join(r.SessionsRoot, "4242.json"), registry)
	s := fetch(t, r).Sessions[0]
	if !s.Active || !s.Working || s.Name != "defect backlog" {
		t.Fatalf("active/working/name = %v/%v/%q", s.Active, s.Working, s.Name)
	}

	r.alive = func(int) bool { return false }
	clock.at = clock.at.Add(discoveryEvery)
	s = fetch(t, r).Sessions[0]
	if s.Active || s.Working {
		t.Fatalf("dead process still active/working: %v/%v", s.Active, s.Working)
	}
}

func TestOldUnregisteredFilesAreIgnored(t *testing.T) {
	clock := &testClock{at: time.Now()}
	r, project := newTestReader(t, clock)
	path := filepath.Join(project, testSession+".jsonl")
	appendFile(t, path, assistantLine(t, clock.at.Add(-72*time.Hour), "msg_1", 1, 0, 0, 0))
	old := clock.at.Add(-72 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if usage := fetch(t, r); len(usage.Sessions) != 0 {
		t.Fatalf("sessions = %d, want 0", len(usage.Sessions))
	}
}

func TestBoundTextStripsControlsAndTruncates(t *testing.T) {
	if got := boundText("a\x1b[31mb\nc"); got != "a[31mb\nc" {
		t.Fatalf("got %q", got)
	}
	long := boundText(strings.Repeat("é", contextLimit))
	if !strings.HasSuffix(long, "…") || len(long) > contextLimit+len("…") {
		t.Fatalf("truncated length %d", len(long))
	}
}

// Explicit opt-in, read-only check against the real Claude Code home.
func TestLiveClaudeHomeSmoke(t *testing.T) {
	home := os.Getenv("CODEXOMETER_CLAUDE_SMOKE_HOME")
	if home == "" {
		t.Skip("set CODEXOMETER_CLAUDE_SMOKE_HOME")
	}
	r, err := NewReader(home)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	usage := fetch(t, r)
	t.Logf("first fetch %s: files=%d sessions=%d active=%d total=%d", time.Since(started), len(r.files), len(usage.Sessions), usage.SessionCount, usage.TotalTokens)
	for _, s := range usage.Sessions {
		if s.Active {
			t.Logf("%s active=%v working=%v agents=%d tokens=%d calls=%d model=%s name=%q", s.ID, s.Active, s.Working, s.AgentCount, s.TotalTokens, len(s.ModelCalls), s.ModelSettings.Model, s.Name)
		}
	}
	started = time.Now()
	again := fetch(t, r)
	t.Logf("second fetch %s", time.Since(started))
	if again.TotalTokens < usage.TotalTokens {
		t.Fatal("total moved backwards")
	}
}
