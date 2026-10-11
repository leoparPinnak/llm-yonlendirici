package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionFingerprintIsStablePerConversation(t *testing.T) {
	// Two turns of one conversation: the same first user message, a grown
	// history — every turn of one chat must hash identically.
	turn1 := json.RawMessage(`[{"role":"system","content":"sys"},{"role":"user","content":"selam nasılsın"}]`)
	turn2 := json.RawMessage(`[{"role":"system","content":"sys"},{"role":"user","content":"selam nasılsın"},{"role":"assistant","content":"iyiyim"},{"role":"user","content":"ya sen"}]`)
	id1, preview1 := sessionFingerprint(turn1)
	id2, preview2 := sessionFingerprint(turn2)
	if id1 == "" || id1 != id2 {
		t.Errorf("fingerprint changed across turns: %q vs %q", id1, id2)
	}
	if preview1 != preview2 || preview1 != "selam nasılsın" {
		t.Errorf("preview = %q / %q", preview1, preview2)
	}

	other := json.RawMessage(`[{"role":"user","content":"bambaşka bir sohbet"}]`)
	id3, _ := sessionFingerprint(other)
	if id3 == id1 {
		t.Error("different conversations share a fingerprint")
	}

	// Block-shaped content flattens too.
	blocks := json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"parça bir"},{"type":"text","text":" parça iki"}]}]`)
	id4, preview4 := sessionFingerprint(blocks)
	if id4 == "" || preview4 != "parça bir parça iki" {
		t.Errorf("block content fingerprint = %q %q", id4, preview4)
	}

	// No user message falls back without failing.
	id5, _ := sessionFingerprint(json.RawMessage(`[{"role":"system","content":"x"}]`))
	if id5 != "bilinmeyen" {
		t.Errorf("fallback id = %q", id5)
	}
}

func TestPreviewIsShortened(t *testing.T) {
	long := strings.Repeat("kelime ", 30)
	_, preview := sessionFingerprint(json.RawMessage(`[{"role":"user","content":"` + long + `"}]`))
	runes := []rune(preview)
	if len(runes) > 49 {
		t.Errorf("preview is %d runes long", len(runes))
	}
	if !strings.HasSuffix(preview, "…") {
		t.Errorf("preview not marked as shortened: %q", preview)
	}
}

func TestUsageLogRecordsAndReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kullanim.jsonl")
	first, err := openUsageLog(path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	first.record(usageRecord{
		Time: time.Now(), Session: "aaa", Preview: "bir", Model: "m",
		PromptTokens: 100, CompletionTokens: 20, ReasoningTokens: 15,
		CachedTokens: 40, Status: 200,
	})
	first.record(usageRecord{
		Time: time.Now(), Session: "aaa", Model: "m",
		PromptTokens: 50, CompletionTokens: 10, Status: 403,
	})
	first.Close()

	// A second handle over the same file sees the history: the log is the
	// only state, so restarts keep it.
	second, err := openUsageLog(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer second.Close()
	report := second.report(time.Now())
	if report.Totals.Requests != 2 || report.Totals.Prompt != 150 || report.Totals.Completion != 30 {
		t.Errorf("totals = %+v", report.Totals)
	}
	if report.Totals.Errors != 1 {
		t.Errorf("errors = %d", report.Totals.Errors)
	}
	if report.Totals.Reasoning != 15 {
		t.Errorf("reasoning = %d", report.Totals.Reasoning)
	}
	if report.Totals.Cached != 40 {
		t.Errorf("cached = %d", report.Totals.Cached)
	}
	if len(report.Sessions) != 1 {
		t.Fatalf("sessions = %d", len(report.Sessions))
	}
	row := report.Sessions[0]
	if row.Session != "aaa" || row.Requests != 2 || row.Errors != 1 || row.Preview != "bir" {
		t.Errorf("session row = %+v", row)
	}
	if row.Cached != 40 {
		t.Errorf("session cached = %d", row.Cached)
	}
	if row.Models["m"] != 2 {
		t.Errorf("model counts = %v", row.Models)
	}
	if len(report.Recent) != 2 || report.Recent[0].Status != 403 {
		t.Errorf("recent = %+v", report.Recent)
	}
}

func TestUsageTokensParsesDetails(t *testing.T) {
	raw := json.RawMessage(`{"prompt_tokens":525,"completion_tokens":16,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":13}}`)
	prompt, completion, reasoning, cached := usageTokens(raw)
	if prompt != 525 || completion != 16 || reasoning != 13 || cached != 4 {
		t.Errorf("usageTokens = %d %d %d %d", prompt, completion, reasoning, cached)
	}
	if prompt, completion, reasoning, cached := usageTokens(nil); prompt != 0 || completion != 0 || reasoning != 0 || cached != 0 {
		t.Errorf("nil usage = %d %d %d %d", prompt, completion, reasoning, cached)
	}
}
