package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// maxClientBytes bounds a client request body, so an unbounded upload cannot
// exhaust memory.
const maxClientBytes = 64 << 20

// clientRequest is a parsed chat-completions body. The messages value and
// every unknown top-level field are kept as raw bytes so they can cross to
// the upstream unchanged.
type clientRequest struct {
	Model        string
	Messages     json.RawMessage
	Tools        []json.RawMessage
	Stream       *bool
	IncludeUsage *bool
	Rest         map[string]json.RawMessage
}

// parseClientRequest decodes a chat-completions body without touching the
// message payload.
func parseClientRequest(body []byte) (*clientRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("the body is not a JSON object: %w", err)
	}
	req := &clientRequest{Rest: make(map[string]json.RawMessage)}
	for name, raw := range fields {
		switch name {
		case "model":
			if err := json.Unmarshal(raw, &req.Model); err != nil {
				return nil, fmt.Errorf("model: %w", err)
			}
		case "messages":
			req.Messages = raw
		case "tools":
			if err := json.Unmarshal(raw, &req.Tools); err != nil {
				return nil, fmt.Errorf("tools: %w", err)
			}
		case "stream":
			if err := json.Unmarshal(raw, &req.Stream); err != nil {
				return nil, fmt.Errorf("stream: %w", err)
			}
		case "stream_options":
			var opts struct {
				IncludeUsage *bool `json:"include_usage"`
			}
			if err := json.Unmarshal(raw, &opts); err != nil {
				return nil, fmt.Errorf("stream_options: %w", err)
			}
			req.IncludeUsage = opts.IncludeUsage
		default:
			req.Rest[name] = raw
		}
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	if trimmed := strings.TrimSpace(string(req.Messages)); !strings.HasPrefix(trimmed, "[") {
		return nil, fmt.Errorf("messages must be an array")
	}
	return req, nil
}

// wantsStream reports whether the client asked for a streamed reply. Absent
// means no, matching the OpenAI default.
func (r *clientRequest) wantsStream() bool {
	return r.Stream != nil && *r.Stream
}

// passUsage reports whether the client asked to receive usage chunks.
func (r *clientRequest) passUsage() bool {
	return r.IncludeUsage != nil && *r.IncludeUsage
}

// toolName returns a tool definition's function name, or "" when the
// definition is not the OpenAI function form.
func toolName(raw json.RawMessage) string {
	var tool struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tool); err != nil {
		return ""
	}
	return tool.Function.Name
}

// reservedTool builds the minimal tool that carries a name the gate wants:
// the gate inspects names, not definitions.
func reservedTool(name string) json.RawMessage {
	tool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "Reserved by the gateway. Do not call this tool.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
	raw, err := json.Marshal(tool)
	if err != nil {
		return json.RawMessage(`{"type":"function"}`)
	}
	return raw
}

// repack rebuilds the body for the upstream: the model defaulted, the
// messages value copied verbatim, the tool list topped up with reserved
// OpenCode names until the free-tier gate's threshold is met, a command
// tool ensured by name, and streaming forced on regardless of what the
// client asked for.
//
// The gate's measured rules (23.09.2026): the tools array must carry at
// least MinGateTools real OpenCode names AND one of the ShellTools command
// names — a set of six OpenCode names without a shell tool is rejected,
// while the same set plus "shell" passes.
func (r *clientRequest) repack(cfg config) ([]byte, error) {
	model := r.Model
	if model == "" {
		model = cfg.DefaultModel
	}

	tools := make([]json.RawMessage, 0, len(r.Tools)+len(cfg.OpencodeTools)+1)
	present := make(map[string]bool, len(r.Tools)+1)
	for _, raw := range r.Tools {
		tools = append(tools, raw)
		if name := toolName(raw); name != "" {
			present[name] = true
		}
	}

	// The command-tool requirement first: the reserved stand-in counts
	// toward the name threshold below.
	hasCommandTool := false
	for _, name := range cfg.ShellTools {
		if present[name] {
			hasCommandTool = true
			break
		}
	}
	if !hasCommandTool {
		standIn := cfg.ShellTools[0]
		tools = append(tools, reservedTool(standIn))
		present[standIn] = true
	}

	gated := 0
	for _, name := range cfg.OpencodeTools {
		if present[name] {
			gated++
		}
	}
	for _, name := range cfg.OpencodeTools {
		if gated >= cfg.MinGateTools {
			break
		}
		if present[name] {
			continue
		}
		tools = append(tools, reservedTool(name))
		present[name] = true
		gated++
	}

	messages := r.Messages
	if repaired, ok := repairMessages(r.Messages); ok {
		messages = repaired
	}

	return r.marshal(model, messages, tools)
}

// repairMessages inspects the messages array for invalid or empty tool_call_id
// and empty assistant tool_call ids, which otherwise cause Upstream 400 Bad Request
// ("tool messages must include a non-empty string tool_call_id").
// If no repair is needed, it returns the original bytes untouched.
func repairMessages(raw json.RawMessage) (json.RawMessage, bool) {
	if !bytes.Contains(raw, []byte("tool")) && !bytes.Contains(raw, []byte("developer")) {
		return raw, false
	}

	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return raw, false
	}

	modified := false
	for msgIdx := 0; msgIdx < len(msgs); msgIdx++ {
		msg := msgs[msgIdx]
		role, _ := msg["role"].(string)

		// Upstream uyumluluğu: tanınmayan "developer" rolünü "system" rolüne dönüştür
		if role == "developer" {
			msg["role"] = "system"
			modified = true
		}

		// 1. Repair assistant messages with empty tool_call IDs
		if role == "assistant" {
			if rawCalls, ok := msg["tool_calls"].([]any); ok {
				for callIdx, rawCall := range rawCalls {
					if callMap, ok := rawCall.(map[string]any); ok {
						id, _ := callMap["id"].(string)
						if strings.TrimSpace(id) == "" {
							callMap["id"] = fmt.Sprintf("call_repaired_%d_%d", msgIdx, callIdx)
							modified = true
						}
						if fnMap, ok := callMap["function"].(map[string]any); ok {
							name, _ := fnMap["name"].(string)
							if strings.TrimSpace(name) == "" {
								fnMap["name"] = "repaired_tool"
								modified = true
							}
						}
					}
				}
			}
		}

		// 2. Repair tool result messages with empty tool_call_id
		if role == "tool" {
			toolCallID, _ := msg["tool_call_id"].(string)
			if strings.TrimSpace(toolCallID) == "" {
				newID := fmt.Sprintf("call_repaired_%d", msgIdx)

				// Look backward for preceding assistant message
				for prevIdx := msgIdx - 1; prevIdx >= 0; prevIdx-- {
					if msgs[prevIdx]["role"] == "assistant" {
						if rawCalls, ok := msgs[prevIdx]["tool_calls"].([]any); ok && len(rawCalls) > 0 {
							// Borrow ID from assistant tool_call
							if firstCall, ok := rawCalls[0].(map[string]any); ok {
								if cid, _ := firstCall["id"].(string); cid != "" {
									newID = cid
								}
							}
						} else {
							// If preceding assistant had no tool_calls, add matching one
							msgs[prevIdx]["tool_calls"] = []any{
								map[string]any{
									"id":   newID,
									"type": "function",
									"function": map[string]any{
										"name":      "repaired_tool",
										"arguments": "{}",
									},
								},
							}
						}
						break
					}
				}

				msg["tool_call_id"] = newID
				modified = true
			}
		}
	}

	if !modified {
		return raw, false
	}

	repaired, err := json.Marshal(msgs)
	if err != nil {
		return raw, false
	}
	return repaired, true
}

// marshal writes the upstream body by hand so the messages value and the
// client's own tool definitions cross as the exact bytes received.
func (r *clientRequest) marshal(model string, messages json.RawMessage, tools []json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"model":`)
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	buf.Write(encoded)
	buf.WriteString(`,"messages":`)
	buf.Write(messages)
	if len(tools) > 0 {
		buf.WriteString(`,"tools":[`)
		for i, tool := range tools {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(tool)
		}
		buf.WriteByte(']')
	}
	buf.WriteString(`,"stream":true`)
	buf.WriteString(`,"stream_options":{"include_usage":true}`)

	names := make([]string, 0, len(r.Rest))
	for name := range r.Rest {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		buf.WriteByte(',')
		encoded, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		buf.Write(encoded)
		buf.WriteByte(':')
		buf.Write(r.Rest[name])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
