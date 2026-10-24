// Package claude observes local Claude Code sessions for the monitor. It is
// read-only: it tails the JSONL transcripts Claude Code writes under its
// projects directory and reads the live-session registry, and never talks to
// Claude itself.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/merefield/codexometer/internal/codex"
)

// Provider labels Claude sessions in codex.LiveUsageSession.Provider.
const Provider = "claude"

const (
	idPrefix       = "claude:"
	discoveryEvery = 5 * time.Second
	activeHorizon  = 24 * time.Hour
	// Matches the Codex reader: recent writes keep an unregistered session active.
	activeQuietAfter = 5 * time.Minute
	historyMax       = 4_096
	contextLimit     = 4_096
	readBufferBytes  = 1 << 20
)

// Reader incrementally observes token usage written by local Claude Code
// sessions. Like the Codex reader it is a process-local accounting view: only
// responses recorded after the reader started count as tokens or calls. Earlier
// records still supply names, model, context and message-id deduplication.
// Message content is never retained beyond a bounded display excerpt.
type Reader struct {
	ProjectsRoot string
	SessionsRoot string
	now          func() time.Time
	alive        func(pid int, procStart string) bool

	mu            sync.Mutex
	initialized   bool
	startedAt     time.Time
	lastDiscovery time.Time
	files         map[string]*cursor
	sessions      map[string]*session
	// registry is keyed by session id; registryFiles keeps the last good entry
	// per registry file so a file caught mid-rewrite does not drop its session.
	registry      map[string]registryEntry
	registryFiles map[string]registryEntry
	// Claude Code writes one record per content block, and resumed or forked
	// transcripts copy earlier records, all repeating the same message id and
	// usage. Tokens are counted once per message id across all files.
	messages map[string]int64
	sequence uint64
	buffer   *bufio.Reader
}

type cursor struct {
	sessionID  string
	subagent   bool
	agent      string
	offset     int64
	lastGrowth time.Time
}

type session struct {
	id           string
	title        string
	agentName    string
	cwd          string
	startedAt    time.Time
	lastActivity time.Time
	totalTokens  int64
	model        string
	effort       string
	agents       map[string]struct{}
	calls        []codex.LiveModelCall
	context      codex.SessionContext
}

type registryEntry struct {
	PID       int    `json:"pid"`
	ProcStart string `json:"procStart"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Status    string `json:"status"`
}

type record struct {
	Type        string    `json:"type"`
	Timestamp   time.Time `json:"timestamp"`
	Cwd         string    `json:"cwd"`
	Effort      string    `json:"effort"`
	CustomTitle string    `json:"customTitle"`
	AgentName   string    `json:"agentName"`
	Message     *struct {
		ID      string         `json:"id"`
		Model   string         `json:"model"`
		Usage   *messageUsage  `json:"usage"`
		Content []contentBlock `json:"content"`
	} `json:"message"`
}

type messageUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

// total matches Codex's total_tokens basis: all input, cached or not, plus output.
func (u messageUsage) total() int64 {
	return max(u.InputTokens, 0) + max(u.CacheCreationInputTokens, 0) +
		max(u.CacheReadInputTokens, 0) + max(u.OutputTokens, 0)
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// DefaultHome returns CLAUDE_CONFIG_DIR, or ~/.claude.
func DefaultHome() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// NewReader observes the Claude Code home directory; empty means DefaultHome.
// It fails when the home has no projects directory, i.e. Claude Code is absent.
func NewReader(claudeHome string) (*Reader, error) {
	if claudeHome == "" {
		var err error
		if claudeHome, err = DefaultHome(); err != nil {
			return nil, err
		}
	}
	projects := filepath.Join(claudeHome, "projects")
	if info, err := os.Stat(projects); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, errors.New("Claude projects path is not a directory")
	}
	return &Reader{
		ProjectsRoot:  projects,
		SessionsRoot:  filepath.Join(claudeHome, "sessions"),
		files:         make(map[string]*cursor),
		sessions:      make(map[string]*session),
		registryFiles: make(map[string]registryEntry),
		messages:      make(map[string]int64),
	}, nil
}

func (r *Reader) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// FetchTokenUsage implements codex.UsageSource. Totals never decrease: files
// that fail to read are retried on the next call and counted tokens are kept.
func (r *Reader) FetchTokenUsage(ctx context.Context) (codex.LiveUsageSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	if !r.initialized {
		r.startedAt = now
		r.initialized = true
	}
	if r.lastDiscovery.IsZero() || now.Sub(r.lastDiscovery) >= discoveryEvery {
		r.readRegistry()
		r.discover(now)
		r.lastDiscovery = now
	}
	for path, c := range r.files {
		if ctx.Err() != nil {
			break
		}
		r.consume(ctx, path, c, now)
	}
	return r.snapshot(now), nil
}

// discover tracks session transcripts at projects/<project>/<id>.jsonl that
// were written recently or belong to a live registered session, and subagent
// transcripts at projects/<project>/<id>/subagents/*.jsonl of those sessions
// only, so idle history is never walked.
func (r *Reader) discover(now time.Time) {
	projects, err := os.ReadDir(r.ProjectsRoot)
	if err != nil {
		return
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(r.ProjectsRoot, project.Name())
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		var sessionDirs []string
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				sessionDirs = append(sessionDirs, name)
			} else if strings.HasSuffix(name, ".jsonl") {
				r.track(filepath.Join(projectDir, name), strings.TrimSuffix(name, ".jsonl"), false, entry, now)
			}
		}
		for _, id := range sessionDirs {
			if _, live := r.registry[id]; !live && r.files[filepath.Join(projectDir, id+".jsonl")] == nil {
				continue
			}
			subagentDir := filepath.Join(projectDir, id, "subagents")
			subagents, err := os.ReadDir(subagentDir)
			if err != nil {
				continue
			}
			for _, sub := range subagents {
				if !sub.IsDir() && strings.HasSuffix(sub.Name(), ".jsonl") {
					r.track(filepath.Join(subagentDir, sub.Name()), id, true, sub, now)
				}
			}
		}
	}
}

func (r *Reader) track(path, sessionID string, subagent bool, entry os.DirEntry, now time.Time) {
	if _, known := r.files[path]; known {
		return
	}
	if _, live := r.registry[sessionID]; !live {
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) > activeHorizon {
			return
		}
	}
	c := &cursor{sessionID: sessionID, subagent: subagent, lastGrowth: now}
	if subagent {
		c.agent = filepath.Base(path)
	}
	r.files[path] = c
}

func (r *Reader) readRegistry() {
	alive := r.alive
	if alive == nil {
		alive = processAlive
	}
	entries, _ := os.ReadDir(r.SessionsRoot)
	files := make(map[string]registryEntry, len(entries))
	r.registry = make(map[string]registryEntry, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		var reg registryEntry
		data, err := os.ReadFile(filepath.Join(r.SessionsRoot, name))
		if err != nil || json.Unmarshal(data, &reg) != nil {
			// Possibly caught mid-rewrite: keep the last good entry.
			previous, ok := r.registryFiles[name]
			if !ok {
				continue
			}
			reg = previous
		}
		if reg.SessionID == "" || reg.PID <= 0 || !alive(reg.PID, reg.ProcStart) {
			continue
		}
		files[name] = reg
		r.registry[reg.SessionID] = reg
	}
	r.registryFiles = files
}

// consume reads complete lines appended since the cursor's offset. The size
// comes from an open handle because Windows directory metadata can lag behind
// a file another process still holds open.
func (r *Reader) consume(ctx context.Context, path string, c *cursor, now time.Time) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		delete(r.files, path)
		return
	}
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	size := info.Size()
	if size < c.offset {
		// Rewritten in place; message-id deduplication prevents recounting.
		c.offset = 0
	}
	if size == c.offset {
		if _, live := r.registry[c.sessionID]; !live && now.Sub(c.lastGrowth) > activeHorizon {
			// Stop polling idle history; discovery re-tracks it if it is written again.
			delete(r.files, path)
		}
		return
	}
	c.lastGrowth = now
	section := io.NewSectionReader(f, c.offset, size-c.offset)
	if r.buffer == nil {
		r.buffer = bufio.NewReaderSize(section, readBufferBytes)
	} else {
		r.buffer.Reset(section)
	}
	for ctx.Err() == nil {
		line, err := r.buffer.ReadBytes('\n')
		if err != nil {
			// io.EOF leaves an incomplete final record to be read once finished.
			return
		}
		c.offset += int64(len(line))
		r.observe(c, line)
	}
}

var (
	assistantMarker   = []byte(`"type":"assistant"`)
	customTitleMarker = []byte(`"type":"custom-title"`)
	agentNameMarker   = []byte(`"type":"agent-name"`)
)

func (r *Reader) observe(c *cursor, line []byte) {
	if !bytes.Contains(line, assistantMarker) && !bytes.Contains(line, customTitleMarker) && !bytes.Contains(line, agentNameMarker) {
		return
	}
	var rec record
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	s := r.session(c.sessionID)
	switch rec.Type {
	case "custom-title":
		if !c.subagent && rec.CustomTitle != "" {
			s.title = rec.CustomTitle
		}
		return
	case "agent-name":
		if !c.subagent && rec.AgentName != "" {
			s.agentName = rec.AgentName
		}
		return
	case "assistant":
	default:
		return
	}
	if rec.Message == nil || rec.Timestamp.IsZero() {
		return
	}
	if s.startedAt.IsZero() || rec.Timestamp.Before(s.startedAt) {
		s.startedAt = rec.Timestamp
	}
	if rec.Cwd != "" && !c.subagent {
		s.cwd = rec.Cwd
	}
	if c.subagent {
		s.agents[c.agent] = struct{}{}
	}
	// Synthetic messages (model "<synthetic>") are local bookkeeping such as
	// API errors and interrupts, not model responses.
	synthetic := strings.HasPrefix(rec.Message.Model, "<")
	if rec.Message.Model != "" && !synthetic && !c.subagent {
		s.model = rec.Message.Model
		s.effort = rec.Effort
	}
	if rec.Timestamp.After(s.lastActivity) {
		s.lastActivity = rec.Timestamp
	}
	if !c.subagent {
		r.observeContext(s, rec)
	}
	if rec.Message.Usage == nil {
		return
	}
	tokens := rec.Message.Usage.total()
	id := rec.Message.ID
	previous, seen := r.messages[id]
	if id != "" {
		if seen && tokens <= previous {
			return
		}
		r.messages[id] = tokens
	}
	if rec.Timestamp.Before(r.startedAt) {
		return
	}
	s.totalTokens += tokens - previous
	if seen || synthetic {
		return
	}
	r.sequence++
	s.calls = append(s.calls, codex.LiveModelCall{
		Sequence:        r.sequence,
		At:              rec.Timestamp,
		OutputTokens:    rec.Message.Usage.OutputTokens,
		OutputAvailable: true,
		Model:           rec.Message.Model,
		SessionID:       idPrefix + c.sessionID,
		ResponseID:      id,
		Effort:          rec.Effort,
		UsageSource:     Provider,
	})
	// Trim in batches so a long session does not copy the history per call.
	if len(s.calls) >= 2*historyMax {
		s.calls = append([]codex.LiveModelCall(nil), s.calls[len(s.calls)-historyMax:]...)
	}
}

// observeContext keeps the latest reply text or tool activity as a bounded
// display excerpt. Thinking blocks are never retained.
func (r *Reader) observeContext(s *session, rec record) {
	for _, block := range rec.Message.Content {
		var kind codex.SessionContextKind
		var text string
		switch block.Type {
		case "text":
			kind, text = codex.SessionContextReply, block.Text
		case "tool_use":
			kind, text = codex.SessionContextActivity, toolActivity(block)
		default:
			continue
		}
		text = boundText(text)
		if text == "" {
			continue
		}
		s.context = codex.SessionContext{
			Kind:     kind,
			Text:     text,
			At:       rec.Timestamp,
			ThreadID: idPrefix + s.id,
			TurnID:   rec.Message.ID,
			Source:   Provider,
		}
	}
}

func toolActivity(block contentBlock) string {
	var input struct {
		Description string `json:"description"`
		Command     string `json:"command"`
		FilePath    string `json:"file_path"`
		Pattern     string `json:"pattern"`
		URL         string `json:"url"`
		Query       string `json:"query"`
	}
	_ = json.Unmarshal(block.Input, &input)
	for _, detail := range []string{input.Description, input.Command, input.FilePath, input.Pattern, input.URL, input.Query} {
		if detail = strings.TrimSpace(detail); detail != "" {
			if line, _, _ := strings.Cut(detail, "\n"); line != "" {
				return block.Name + ": " + line
			}
		}
	}
	return block.Name
}

// boundText strips terminal control characters and truncates on a rune boundary.
func boundText(text string) string {
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, strings.TrimSpace(text))
	if len(text) <= contextLimit {
		return text
	}
	cut := contextLimit
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + "…"
}

func (r *Reader) session(id string) *session {
	s := r.sessions[id]
	if s == nil {
		s = &session{id: id, agents: make(map[string]struct{})}
		r.sessions[id] = s
	}
	return s
}

// snapshot reports sessions that are active or have usage since the reader
// started, like the Codex reader; totals include every session ever counted.
func (r *Reader) snapshot(now time.Time) codex.LiveUsageSnapshot {
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var usage codex.LiveUsageSnapshot
	for _, id := range ids {
		s := r.sessions[id]
		usage.TotalTokens += s.totalTokens
		reg, live := r.registry[id]
		active := live || (!s.lastActivity.IsZero() && now.Sub(s.lastActivity) <= activeQuietAfter)
		if !active && s.totalTokens == 0 && len(s.calls) == 0 {
			continue
		}
		name := s.title
		if name == "" {
			name = s.agentName
		}
		if live && reg.Name != "" && s.title == "" {
			name = reg.Name
		}
		cwd := s.cwd
		if cwd == "" && live {
			cwd = reg.Cwd
		}
		if s.lastActivity.After(usage.LastActivity) {
			usage.LastActivity = s.lastActivity
		}
		if active {
			usage.SessionCount++
		}
		calls := s.calls
		if len(calls) > historyMax {
			calls = calls[len(calls)-historyMax:]
		}
		usage.Sessions = append(usage.Sessions, codex.LiveUsageSession{
			ModelSettings:    codex.SessionModelSettings{Model: s.model, ReasoningEffort: s.effort},
			Provider:         Provider,
			ID:               idPrefix + id,
			Name:             name,
			WorkingDirectory: cwd,
			StartedAt:        s.startedAt,
			TotalTokens:      s.totalTokens,
			LastActivity:     s.lastActivity,
			AgentCount:       len(s.agents),
			Active:           active,
			Working:          live && reg.Status == "busy",
			Context:          s.context,
			ModelCalls:       append([]codex.LiveModelCall(nil), calls...),
		})
	}
	return usage
}
