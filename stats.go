package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// sessionFingerprint identifies the conversation a request belongs to: the
// hash of its first user message. A chat client resends the whole history
// every turn, so every turn of one conversation hashes identically. Only
// the hash and a short preview are retained — the content itself never
// leaves the request.
func sessionFingerprint(messages json.RawMessage) (id, preview string) {
	var items []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(messages, &items); err != nil {
		return "bilinmeyen", ""
	}
	for _, item := range items {
		if item.Role != "user" {
			continue
		}
		text := contentText(item.Content)
		sum := sha256.Sum256([]byte(text))
		return hex.EncodeToString(sum[:])[:12], previewOf(text)
	}
	return "bilinmeyen", ""
}

// contentText flattens an OpenAI content value: either a plain string or an
// array of typed blocks.
func contentText(raw json.RawMessage) string {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var flat strings.Builder
		for _, block := range blocks {
			flat.WriteString(block.Text)
		}
		return flat.String()
	}
	return ""
}

// previewOf shortens a message to one displayable line.
func previewOf(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	runes := []rune(flat)
	if len(runes) > 48 {
		return string(runes[:48]) + "…"
	}
	return flat
}

// usageRecord is one completed exchange, one line of the log.
type usageRecord struct {
	Time             time.Time `json:"time"`
	Session          string    `json:"session"`
	Preview          string    `json:"preview"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	ReasoningTokens  int       `json:"reasoning_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	Status           int       `json:"status"`
	DurationMs       int64     `json:"duration_ms"`
	Streamed         bool      `json:"streamed"`
	ClientPort       int       `json:"client_port,omitempty"`
	ClientPID        uint32    `json:"client_pid,omitempty"`
	ClientProc       string    `json:"client_proc,omitempty"`
}

// ActiveRequest represents one in-flight exchange currently executing.
type ActiveRequest struct {
	ID         string    `json:"id"`
	StartTime  time.Time `json:"start_time"`
	ElapsedMs  int64     `json:"elapsed_ms"`
	ClientPort int       `json:"client_port"`
	ClientPID  uint32    `json:"client_pid"`
	ClientProc string    `json:"client_proc"`
	Session    string    `json:"session"`
	Preview    string    `json:"preview"`
	Model      string    `json:"model"`
	Streamed   bool      `json:"streamed"`
}

// usageTotals is the all-time aggregate over the log.
type usageTotals struct {
	Requests   int   `json:"requests"`
	Active     int   `json:"active"`
	Prompt     int64 `json:"prompt"`
	Cached     int64 `json:"cached"`
	Completion int64 `json:"completion"`
	Reasoning  int64 `json:"reasoning"`
	Errors     int   `json:"errors"`
}

// sessionRow is one conversation's aggregate.
type sessionRow struct {
	Session    string         `json:"session"`
	Preview    string         `json:"preview"`
	FirstSeen  time.Time      `json:"first_seen"`
	LastSeen   time.Time      `json:"last_seen"`
	Requests   int            `json:"requests"`
	Prompt     int64          `json:"prompt"`
	Cached     int64          `json:"cached"`
	Completion int64          `json:"completion"`
	Reasoning  int64          `json:"reasoning"`
	Errors     int            `json:"errors"`
	Models     map[string]int `json:"models"`
}

// usageReport is everything /kullanim answers with.
type usageReport struct {
	Started  time.Time       `json:"started"`
	FirstLog time.Time       `json:"first_log"`
	Totals   usageTotals     `json:"totals"`
	Active   []ActiveRequest `json:"active"`
	Sessions []sessionRow    `json:"sessions"`
	Recent   []usageRecord   `json:"recent"`
}

// usageLog appends one JSON line per exchange and re-reads the whole file
// for reports. The file is the only state; it survives restarts.
type usageLog struct {
	mu         sync.Mutex
	path       string
	file       *os.File
	activeReqs map[string]*ActiveRequest
}

// openUsageLog opens (or creates) the append-only log.
func openUsageLog(path string) (*usageLog, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &usageLog{
		path:       path,
		file:       file,
		activeReqs: make(map[string]*ActiveRequest),
	}, nil
}

func (l *usageLog) startRequest(req *ActiveRequest) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.activeReqs == nil {
		l.activeReqs = make(map[string]*ActiveRequest)
	}
	l.activeReqs[req.ID] = req
}

func (l *usageLog) finishRequest(id string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.activeReqs, id)
}

// record appends one exchange; a flush per line keeps a crash from losing
// more than the line being written.
func (l *usageLog) record(rec usageRecord) {
	if l == nil {
		return
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.file.Write(append(encoded, '\n'))
}

// Close releases the log file.
func (l *usageLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.file.Close()
}

// report aggregates the whole log: all-time totals, per-session rows sorted
// by last activity, and the most recent exchanges newest-first.
func (l *usageLog) report(started time.Time) usageReport {
	out := usageReport{
		Started:  started,
		Active:   []ActiveRequest{},
		Sessions: []sessionRow{},
		Recent:   []usageRecord{},
	}
	if l == nil {
		return out
	}
	l.mu.Lock()
	for _, ar := range l.activeReqs {
		cp := *ar
		cp.ElapsedMs = time.Since(ar.StartTime).Milliseconds()
		out.Active = append(out.Active, cp)
	}
	raw, err := os.ReadFile(l.path)
	l.mu.Unlock()

	out.Totals.Active = len(out.Active)
	sort.Slice(out.Active, func(i, j int) bool {
		return out.Active[i].StartTime.Before(out.Active[j].StartTime)
	})
	if err != nil {
		return out
	}

	sessions := make(map[string]*sessionRow)
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	const recentLimit = 50
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec usageRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if out.FirstLog.IsZero() {
			out.FirstLog = rec.Time
		}
		out.Totals.Requests++
		out.Totals.Prompt += int64(rec.PromptTokens)
		out.Totals.Cached += int64(rec.CachedTokens)
		out.Totals.Completion += int64(rec.CompletionTokens)
		out.Totals.Reasoning += int64(rec.ReasoningTokens)
		if rec.Status != 200 {
			out.Totals.Errors++
		}
		row := sessions[rec.Session]
		if row == nil {
			row = &sessionRow{
				Session:   rec.Session,
				Preview:   rec.Preview,
				FirstSeen: rec.Time,
				Models:    make(map[string]int),
			}
			sessions[rec.Session] = row
		}
		row.LastSeen = rec.Time
		row.Requests++
		row.Prompt += int64(rec.PromptTokens)
		row.Cached += int64(rec.CachedTokens)
		row.Completion += int64(rec.CompletionTokens)
		row.Reasoning += int64(rec.ReasoningTokens)
		if rec.Status != 200 {
			row.Errors++
		}
		if rec.Model != "" {
			row.Models[rec.Model]++
		}
		if rec.Preview != "" && row.Preview == "" {
			row.Preview = rec.Preview
		}

		out.Recent = append(out.Recent, rec)
		if len(out.Recent) > recentLimit {
			out.Recent = out.Recent[len(out.Recent)-recentLimit:]
		}
	}

	for _, row := range sessions {
		out.Sessions = append(out.Sessions, *row)
	}
	sort.Slice(out.Sessions, func(i, j int) bool {
		return out.Sessions[i].LastSeen.After(out.Sessions[j].LastSeen)
	})
	for i, j := 0, len(out.Recent)-1; i < j; i, j = i+1, j-1 {
		out.Recent[i], out.Recent[j] = out.Recent[j], out.Recent[i]
	}
	return out
}

// usageTokens pulls the token counts out of one usage object.
func usageTokens(raw json.RawMessage) (prompt, completion, reasoning, cached int) {
	if len(raw) == 0 {
		return
	}
	var u struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	return u.PromptTokens, u.CompletionTokens,
		u.CompletionTokensDetails.ReasoningTokens, u.PromptTokensDetails.CachedTokens
}
