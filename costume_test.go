package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestSessionIDMatchesTheGateFormat(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		id := sessionID()
		if seen[id] {
			t.Fatalf("session id %q repeated", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "ses_") {
			t.Errorf("id %q lacks the ses_ prefix", id)
		}
		tail := strings.TrimPrefix(id, "ses_")
		if len(tail) != 26 {
			t.Errorf("id %q has %d characters after ses_, want 26", id, len(tail))
		}
		for _, r := range tail {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				t.Errorf("id %q carries the character %q", id, r)
			}
		}
	}
}

func TestCostumeProjectIsStableAndHex(t *testing.T) {
	first := newCostume("ua")
	second := newCostume("ua")
	if first.project == second.project {
		t.Error("two runs produced the same project hash")
	}
	if len(first.project) != 40 {
		t.Errorf("project hash %q has %d characters, want 40", first.project, len(first.project))
	}
	for _, r := range first.project {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("project hash %q carries %q", first.project, r)
		}
	}
}

func TestDressReplacesTheClientIdentity(t *testing.T) {
	c := newCostume("opencode/test/1.0/cli")
	head := http.Header{}
	head.Set("Authorization", "Bearer sk-client-secret")
	head.Set("Cookie", "session=1")
	head.Set("X-Api-Key", "sk-client-secret")
	head.Set("B3", "trace")
	head.Set("Traceparent", "00-trace")
	head.Set("Connection", "close, X-Custom-Hop")
	head.Set("X-Custom-Hop", "drop-me")
	head.Set("Content-Type", "text/plain")

	c.dress(head)

	if got := head.Get("Authorization"); got != "Bearer public" {
		t.Errorf("Authorization = %q, want Bearer public", got)
	}
	for _, name := range []string{"Cookie", "X-Api-Key", "B3", "Traceparent", "X-Custom-Hop"} {
		if got := head.Get(name); got != "" {
			t.Errorf("%s = %q, want it dropped", name, got)
		}
	}
	if got := head.Get("User-Agent"); got != "opencode/test/1.0/cli" {
		t.Errorf("User-Agent = %q", got)
	}
	if got := head.Get("X-Opencode-Client"); got != "cli" {
		t.Errorf("X-Opencode-Client = %q", got)
	}
	if got := head.Get("X-Opencode-Project"); got != c.project {
		t.Errorf("X-Opencode-Project = %q, want the costume's %q", got, c.project)
	}
	session := head.Get("X-Opencode-Session")
	if session == "" || session != head.Get("X-Session-Id") ||
		session != head.Get("X-Session-Affinity") {
		t.Errorf("session headers disagree: %q / %q / %q",
			session, head.Get("X-Session-Id"), head.Get("X-Session-Affinity"))
	}
	if !strings.HasPrefix(session, "ses_") {
		t.Errorf("session %q lacks the ses_ prefix", session)
	}
	if got := head.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}
