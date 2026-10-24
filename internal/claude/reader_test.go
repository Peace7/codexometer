package claude

import (
	"context"
	"encoding/json"
	"fmt"
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

func recordLine(t *testing.T, at time.Time, messageID, model string, input, cacheWrite, cacheRead, output int64, content ...map[string]any) string {
	t.Helper()
	record := map[string]any{
		"type": "assistant", "timestamp": at.UTC().Format(time.RFC3339Nano), "sessionId": testSession,
		"cwd": `F:\Work`, "effort": "high",
		"message": map[string]any{
			"id": messageID, "model": model, "content": content,
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

func assistantLine(t *testing.T, at time.Time, messageID string, input, cacheWrite, cacheRead, output int64, content ...map[string]any) string {
	t.Helper()
	return recordLine(t, at, messageID, "claude-opus-5-5", input, cacheWrite, cacheRead, output, content...)
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
	r.alive = func(int, string) bool { return true }
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
	at := clock.at
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

func TestRecordsBeforeStartSupplyContextButNoUsage(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	appendFile(t, filepath.Join(project, testSession+".jsonl"),
		assistantLine(t, clock.at.Add(-time.Minute), "msg_old", 1000, 0, 0, 0, map[string]any{"type": "text", "text": "earlier"}))
	s := fetch(t, r).Sessions[0]
	if s.TotalTokens != 0 || len(s.ModelCalls) != 0 || s.Context.Text != "earlier" || s.ModelSettings.Model == "" {
		t.Fatalf("session = tokens %d calls %d context %q model %q", s.TotalTokens, len(s.ModelCalls), s.Context.Text, s.ModelSettings.Model)
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

func TestCopiedRecordsInAnotherFileCountOnce(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	original := assistantLine(t, clock.at, "msg_1", 30, 0, 0, 0)
	appendFile(t, filepath.Join(project, testSession+".jsonl"), original)
	fetch(t, r)
	clock.at = clock.at.Add(discoveryEvery)
	// A resumed or forked session copies earlier records into a new file.
	forked := filepath.Join(project, "99999999-2222-3333-4444-555555555555.jsonl")
	appendFile(t, forked, original+assistantLine(t, clock.at, "msg_2", 5, 0, 0, 0))
	usage := fetch(t, r)
	if usage.TotalTokens != 35 {
		t.Fatalf("total = %d, want 35", usage.TotalTokens)
	}
	calls := 0
	for _, s := range usage.Sessions {
		calls += len(s.ModelCalls)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestSyntheticMessagesCreateNoCalls(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	appendFile(t, filepath.Join(project, testSession+".jsonl"),
		assistantLine(t, clock.at, "msg_1", 10, 0, 0, 40)+
			recordLine(t, clock.at.Add(time.Second), "8c2e-uuid", "<synthetic>", 0, 0, 0, 0, map[string]any{"type": "text", "text": "API Error"}))
	s := fetch(t, r).Sessions[0]
	if len(s.ModelCalls) != 1 || s.ModelSettings.Model != "claude-opus-5-5" {
		t.Fatalf("calls = %d model = %q", len(s.ModelCalls), s.ModelSettings.Model)
	}
}

func TestCallHistoryIsBoundedWithoutPerCallCopies(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	var lines strings.Builder
	total := 2*historyMax + 10
	for i := range total {
		lines.WriteString(assistantLine(t, clock.at, fmt.Sprintf("msg_%d", i), 1, 0, 0, 1))
	}
	appendFile(t, filepath.Join(project, testSession+".jsonl"), lines.String())
	usage := fetch(t, r)
	s := usage.Sessions[0]
	if len(s.ModelCalls) != historyMax || s.ModelCalls[historyMax-1].Sequence != uint64(total) {
		t.Fatalf("calls = %d, last sequence %d", len(s.ModelCalls), s.ModelCalls[len(s.ModelCalls)-1].Sequence)
	}
	if retained := len(r.sessions[testSession].calls); retained >= 2*historyMax {
		t.Fatalf("retained %d calls", retained)
	}
	if s.TotalTokens != int64(2*total) {
		t.Fatalf("total = %d", s.TotalTokens)
	}
}

func TestDeletedFilesStopBeingPolledButKeepTotals(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	path := filepath.Join(project, testSession+".jsonl")
	appendFile(t, path, assistantLine(t, clock.at, "msg_1", 9, 0, 0, 0))
	fetch(t, r)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	clock.at = clock.at.Add(time.Second)
	usage := fetch(t, r)
	if len(r.files) != 0 || usage.TotalTokens != 9 {
		t.Fatalf("files = %d total = %d", len(r.files), usage.TotalTokens)
	}
}

func TestIdleSessionsWithoutUsageAreOmitted(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	appendFile(t, filepath.Join(project, testSession+".jsonl"), assistantLine(t, clock.at.Add(-time.Hour), "msg_1", 1, 0, 0, 0))
	if sessions := fetch(t, r).Sessions; len(sessions) != 0 {
		t.Fatalf("sessions = %d, want 0", len(sessions))
	}
}

func TestRegistryDrivesNameAndWorking(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r, project := newTestReader(t, clock)
	appendFile(t, filepath.Join(project, testSession+".jsonl"), assistantLine(t, clock.at.Add(-time.Hour), "msg_1", 1, 0, 0, 0))
	registry := `{"pid":4242,"procStart":"777","sessionId":"` + testSession + `","cwd":"F:\\Work","name":"defect backlog","status":"busy"}`
	registryPath := filepath.Join(r.SessionsRoot, "4242.json")
	appendFile(t, registryPath, registry)
	var gotStart string
	r.alive = func(_ int, procStart string) bool { gotStart = procStart; return true }
	s := fetch(t, r).Sessions[0]
	if !s.Active || !s.Working || s.Name != "defect backlog" || gotStart != "777" {
		t.Fatalf("active/working/name/procStart = %v/%v/%q/%q", s.Active, s.Working, s.Name, gotStart)
	}

	// A registry file caught mid-rewrite keeps its last good entry.
	if err := os.WriteFile(registryPath, []byte(`{"pid":42`), 0o644); err != nil {
		t.Fatal(err)
	}
	clock.at = clock.at.Add(discoveryEvery)
	if s := fetch(t, r).Sessions[0]; !s.Working {
		t.Fatal("partial registry rewrite dropped the session")
	}

	r.alive = func(int, string) bool { return false }
	clock.at = clock.at.Add(discoveryEvery)
	if sessions := fetch(t, r).Sessions; len(sessions) != 0 {
		t.Fatalf("dead idle session still listed: %+v", sessions)
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
	fetch(t, r)
	if len(r.files) != 0 {
		t.Fatalf("tracked files = %d, want 0", len(r.files))
	}
}

func TestProcessAliveChecksStartTime(t *testing.T) {
	if !processAlive(os.Getpid(), "") {
		t.Fatal("current process reported dead")
	}
	if processAlive(-1, "") {
		t.Fatal("invalid pid reported alive")
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
	t.Logf("first fetch %s: files=%d sessions=%d active=%d registry=%d", time.Since(started), len(r.files), len(usage.Sessions), usage.SessionCount, len(r.registry))
	for _, s := range usage.Sessions {
		t.Logf("%s active=%v working=%v agents=%d model=%s name=%q", s.ID, s.Active, s.Working, s.AgentCount, s.ModelSettings.Model, s.Name)
	}
	time.Sleep(discoveryEvery)
	started = time.Now()
	again := fetch(t, r)
	t.Logf("fetch with discovery %s: total=%d", time.Since(started), again.TotalTokens)
	started = time.Now()
	fetch(t, r)
	t.Logf("steady fetch %s", time.Since(started))
	if again.TotalTokens < usage.TotalTokens {
		t.Fatal("total moved backwards")
	}
}
