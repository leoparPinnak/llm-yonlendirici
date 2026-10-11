package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// extractValue returns the raw bytes of a top-level field of a JSON object.
func extractValue(t *testing.T, body []byte, field string) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("parsing %s: %v", body, err)
	}
	return fields[field]
}

func TestMessagesCrossVerbatim(t *testing.T) {
	body := []byte(`{ "model" : "mimo-v2.6-flash-free" , "messages" : [ { "role":"user", "content":"café \u00e9 😀 ve\nyeni satır" } ], "temperature":0.7, "tool_choice":"auto" }`)
	request, err := parseClientRequest(body)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	repacked, err := request.repack(defaultConfig())
	if err != nil {
		t.Fatalf("repacking: %v", err)
	}

	sent := extractValue(t, repacked, "messages")
	if !bytes.Equal(sent, request.Messages) {
		t.Errorf("messages changed:\nsent:   %s\nwanted: %s", sent, request.Messages)
	}

	var check map[string]any
	if err := json.Unmarshal(repacked, &check); err != nil {
		t.Fatalf("the repacked body is not valid JSON: %v\n%s", err, repacked)
	}
	if check["temperature"] != 0.7 || check["tool_choice"] != "auto" {
		t.Errorf("unknown fields were dropped: %v", check)
	}
	if check["stream"] != true {
		t.Errorf("stream = %v, want true", check["stream"])
	}
	options, isObject := check["stream_options"].(map[string]any)
	if !isObject || options["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage true", check["stream_options"])
	}
}

func TestReservedToolsTopUpToTheGateThreshold(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"messages":[{"role":"user","content":"merhaba"}],"tools":[{"type":"function","function":{"name":"bash","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`)
	request, err := parseClientRequest(body)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	repacked, err := request.repack(cfg)
	if err != nil {
		t.Fatalf("repacking: %v", err)
	}

	var upstream struct {
		Tools []struct {
			Function struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(repacked, &upstream); err != nil {
		t.Fatalf("parsing the repacked body: %v", err)
	}

	names := map[string]bool{}
	gated := 0
	for _, tool := range upstream.Tools {
		if names[tool.Function.Name] {
			t.Errorf("tool %q appears twice", tool.Function.Name)
		}
		names[tool.Function.Name] = true
		for _, want := range cfg.OpencodeTools {
			if tool.Function.Name == want {
				gated++
				if tool.Function.Description == "" {
					t.Errorf("reserved tool %q has no description", tool.Function.Name)
				}
			}
		}
	}
	if gated < cfg.MinGateTools {
		t.Errorf("only %d opencode tool names crossed, want at least %d",
			gated, cfg.MinGateTools)
	}
	if !names["bash"] {
		t.Error("the client's own tool was dropped")
	}
}

func TestClientToolsThatAlreadyCoverTheGateAreLeftAlone(t *testing.T) {
	cfg := defaultConfig()
	// The first six opencode names, supplied by the client itself — the
	// sixth, shell, also satisfies the command-tool requirement.
	tools := `[` + strings.Join([]string{
		`{"type":"function","function":{"name":"edit"}}`,
		`{"type":"function","function":{"name":"glob"}}`,
		`{"type":"function","function":{"name":"grep"}}`,
		`{"type":"function","function":{"name":"question"}}`,
		`{"type":"function","function":{"name":"read"}}`,
		`{"type":"function","function":{"name":"shell"}}`,
	}, ",") + `]`
	body := []byte(`{"messages":[{"role":"user","content":"merhaba"}],"tools":` + tools + `}`)
	request, err := parseClientRequest(body)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	repacked, err := request.repack(cfg)
	if err != nil {
		t.Fatalf("repacking: %v", err)
	}

	var upstream struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(repacked, &upstream); err != nil {
		t.Fatalf("parsing the repacked body: %v", err)
	}
	if len(upstream.Tools) != 6 {
		t.Errorf("%d tools crossed, want the client's 6 with nothing added",
			len(upstream.Tools))
	}
}

func TestCommandToolIsEnsured(t *testing.T) {
	cfg := defaultConfig()
	// Six real opencode names but no command tool: the exact shape the
	// live gate rejected on 23.09.2026.
	tools := `[` + strings.Join([]string{
		`{"type":"function","function":{"name":"edit"}}`,
		`{"type":"function","function":{"name":"glob"}}`,
		`{"type":"function","function":{"name":"grep"}}`,
		`{"type":"function","function":{"name":"read"}}`,
		`{"type":"function","function":{"name":"skill"}}`,
		`{"type":"function","function":{"name":"write"}}`,
	}, ",") + `]`
	body := []byte(`{"messages":[{"role":"user","content":"merhaba"}],"tools":` + tools + `}`)
	request, err := parseClientRequest(body)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	repacked, err := request.repack(cfg)
	if err != nil {
		t.Fatalf("repacking: %v", err)
	}
	var upstream struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(repacked, &upstream); err != nil {
		t.Fatalf("parsing the repacked body: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range upstream.Tools {
		names[tool.Function.Name] = true
	}
	if !names["shell"] {
		t.Errorf("no command tool crossed: %v", names)
	}
	if len(upstream.Tools) != 7 {
		t.Errorf("%d tools crossed, want the client's 6 plus one reserved shell",
			len(upstream.Tools))
	}
}

func TestClientCommandToolSatisfiesTheGate(t *testing.T) {
	cfg := defaultConfig()
	// The client's own bash beside six opencode names — the shape the
	// live gate accepted on 23.09.2026: nothing may be appended, not
	// even a reserved shell.
	tools := `[` + strings.Join([]string{
		`{"type":"function","function":{"name":"bash"}}`,
		`{"type":"function","function":{"name":"edit"}}`,
		`{"type":"function","function":{"name":"glob"}}`,
		`{"type":"function","function":{"name":"grep"}}`,
		`{"type":"function","function":{"name":"read"}}`,
		`{"type":"function","function":{"name":"skill"}}`,
		`{"type":"function","function":{"name":"write"}}`,
	}, ",") + `]`
	body := []byte(`{"messages":[{"role":"user","content":"merhaba"}],"tools":` + tools + `}`)
	request, err := parseClientRequest(body)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	repacked, err := request.repack(cfg)
	if err != nil {
		t.Fatalf("repacking: %v", err)
	}
	var upstream struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(repacked, &upstream); err != nil {
		t.Fatalf("parsing the repacked body: %v", err)
	}
	if len(upstream.Tools) != 7 {
		t.Errorf("%d tools crossed, want the client's 7 with nothing added",
			len(upstream.Tools))
	}
	for _, tool := range upstream.Tools {
		if tool.Function.Name == "shell" {
			t.Error("a reserved shell was appended although the client sent bash")
		}
	}
}

func TestModelDefaultsAndStreamIsForced(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"merhaba"}],"stream":false}`)
	request, err := parseClientRequest(body)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if request.wantsStream() {
		t.Error("the client asked stream:false but wantsStream reported true")
	}
	repacked, err := request.repack(defaultConfig())
	if err != nil {
		t.Fatalf("repacking: %v", err)
	}
	if got := extractValue(t, repacked, "model"); string(got) != `"mimo-v2.6-flash-free"` {
		t.Errorf("model = %s, want the default", got)
	}
	if got := extractValue(t, repacked, "stream"); string(got) != "true" {
		t.Errorf("stream = %s, want true upstream", got)
	}
}

func TestParseRejectsBadBodies(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        `merhaba`,
		"not an object":   `["messages"]`,
		"no messages":     `{"model":"m"}`,
		"messages scalar": `{"messages":"merhaba"}`,
	} {
		if _, err := parseClientRequest([]byte(body)); err == nil {
			t.Errorf("%s: parsing accepted %q", name, body)
		}
	}
}

func TestRepairBrokenToolMessages(t *testing.T) {
	// A broken history with empty tool_call_id and empty assistant tool_call id
	brokenBody := []byte(`{
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [{"id": "", "type": "function", "function": {"name": "", "arguments": "{}"}}]},
			{"role": "tool", "content": "Error: unknown tool \"\"", "tool_call_id": ""}
		]
	}`)

	req, err := parseClientRequest(brokenBody)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	repacked, err := req.repack(defaultConfig())
	if err != nil {
		t.Fatalf("repack: %v", err)
	}

	var parsed struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(repacked, &parsed); err != nil {
		t.Fatalf("unmarshal repacked: %v", err)
	}

	if len(parsed.Messages) != 3 {
		t.Fatalf("messages count = %d, want 3", len(parsed.Messages))
	}

	asstCallID := parsed.Messages[1].ToolCalls[0].ID
	toolCallID := parsed.Messages[2].ToolCallID

	if strings.TrimSpace(toolCallID) == "" {
		t.Error("tool_call_id is still empty after repair")
	}
	if strings.TrimSpace(asstCallID) == "" {
		t.Error("assistant tool call id is still empty after repair")
	}
	if toolCallID != asstCallID {
		t.Errorf("ids do not match: tool=%q asst=%q", toolCallID, asstCallID)
	}
}
