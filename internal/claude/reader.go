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
	// Without a live registry entry, recent writes are the only liveness signal.
	activeQuietAfter = 2 * time.Minute
	historyMax       = 4_096
	contextLimit     = 4_096
	readBufferBytes  = 1 << 20
)

// Reader incrementally observes token usage written by local Claude Code
// sessions. Message content is never retained beyond a bounded display excerpt.
type Reader struct {
	ProjectsRoot string
	SessionsRoot string
	now          func() time.Time
	alive        func(pid int) bool

	mu            sync.Mutex
	initialized   bool
	startedAt     time.Time
	lastDiscovery time.Time
	files         map[string]*cursor
	sessions      map[string]*session
	registry      map[string]registryEntry
	sequence      uint64
}

type cursor struct {
	sessionID string
	subagent  bool
	agent     string
	offset    int64
	// countFrom excludes records copied from history into files that appear
	// after monitoring started (resumed sessions), so they are not new usage.
	countFrom time.Time
	// Claude Code writes one record per content block; blocks of one response
	// repeat its usage, so tokens are counted once per message id.
	messages map[string]int64
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
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Status    string `json:"status"`
}

type record struct {
	Type        string    `json:"type"`
	Timestamp   time.Time `json:"timestamp"`
	SessionID   string    `json:"sessionId"`
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
		ProjectsRoot: projects,
		SessionsRoot: filepath.Join(claudeHome, "sessions"),
		files:        make(map[string]*cursor),
		sessions:     make(map[string]*session),
	}, nil
}

func (r *Reader) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// FetchTokenUsage implements codex.UsageSource. Totals never decrease: files
// that fail to read are retried on the next call and keep their counted tokens.
func (r *Reader) FetchTokenUsage(ctx context.Context) (codex.LiveUsageSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	if !r.initialized {
		r.startedAt = now
	}
	if !r.initialized || now.Sub(r.lastDiscovery) >= discoveryEvery {
		r.readRegistry()
		r.discover(now)
		r.lastDiscovery = now
	}
	for path, c := range r.files {
		if err := ctx.Err(); err != nil {
			return r.snapshot(now), nil
		}
		_ = r.consume(ctx, path, c)
	}
	r.initialized = true
	return r.snapshot(now), nil
}

// discover tracks session transcripts at projects/<project>/<id>.jsonl and
// subagent transcripts at projects/<project>/<id>/subagents/*.jsonl that were
// written recently or belong to a live registered session.
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
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() && strings.HasSuffix(name, ".jsonl") {
				id := strings.TrimSuffix(name, ".jsonl")
				r.track(filepath.Join(projectDir, name), id, false, entry, now)
				continue
			}
			if !entry.IsDir() {
				continue
			}
			subagentDir := filepath.Join(projectDir, name, "subagents")
			subagents, err := os.ReadDir(subagentDir)
			if err != nil {
				continue
			}
			for _, sub := range subagents {
				if !sub.IsDir() && strings.HasSuffix(sub.Name(), ".jsonl") {
					r.track(filepath.Join(subagentDir, sub.Name()), name, true, sub, now)
				}
			}
		}
	}
}

func (r *Reader) track(path, sessionID string, subagent bool, entry os.DirEntry, now time.Time) {
	if _, known := r.files[path]; known {
		return
	}
	_, live := r.registry[sessionID]
	if !live {
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) > activeHorizon {
			return
		}
	}
	c := &cursor{sessionID: sessionID, subagent: subagent, messages: make(map[string]int64)}
	if subagent {
		c.agent = filepath.Base(path)
	}
	if r.initialized {
		c.countFrom = r.startedAt
	}
	r.files[path] = c
}

func (r *Reader) readRegistry() {
	r.registry = make(map[string]registryEntry)
	entries, err := os.ReadDir(r.SessionsRoot)
	if err != nil {
		return
	}
	alive := r.alive
	if alive == nil {
		alive = processAlive
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.SessionsRoot, entry.Name()))
		if err != nil {
			continue
		}
		var reg registryEntry
		if json.Unmarshal(data, &reg) != nil || reg.SessionID == "" || reg.PID <= 0 || !alive(reg.PID) {
			continue
		}
		r.registry[reg.SessionID] = reg
	}
}

// consume reads complete lines appended since the cursor's offset. The size
// comes from an open handle because Windows directory metadata can lag behind
// a file another process still holds open.
func (r *Reader) consume(ctx context.Context, path string, c *cursor) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size < c.offset {
		// Rewritten in place: keep counted ids so nothing is double counted.
		c.offset = 0
	}
	if size == c.offset {
		return nil
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(f, c.offset, size-c.offset), readBufferBytes)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			// An incomplete final record is read again once it is finished.
			return nil
		}
		if err != nil {
			return err
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
	if model := rec.Message.Model; model != "" && !strings.HasPrefix(model, "<") && !c.subagent {
		s.model = model
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
	previous, seen := c.messages[id]
	if id != "" {
		if seen && tokens <= previous {
			return
		}
		c.messages[id] = tokens
	}
	if !c.countFrom.IsZero() && rec.Timestamp.Before(c.countFrom) {
		return
	}
	s.totalTokens += tokens - previous
	if seen {
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
	if len(s.calls) > historyMax {
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

func (r *Reader) snapshot(now time.Time) codex.LiveUsageSnapshot {
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var usage codex.LiveUsageSnapshot
	for _, id := range ids {
		s := r.sessions[id]
		reg, live := r.registry[id]
		active := live || (!s.lastActivity.IsZero() && now.Sub(s.lastActivity) <= activeQuietAfter)
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
		usage.TotalTokens += s.totalTokens
		if s.lastActivity.After(usage.LastActivity) {
			usage.LastActivity = s.lastActivity
		}
		if active {
			usage.SessionCount++
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
			ModelCalls:       append([]codex.LiveModelCall(nil), s.calls...),
		})
	}
	return usage
}
