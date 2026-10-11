package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// sampleStream is a Zen-shaped event stream: reasoning, content, a tool call,
// usage, [DONE], and the trailing cost note the gateway must drop.
const sampleStream = `data: {"id":"id_1","object":"chat.completion.chunk","created":1790121332,"model":"mimo-v2.6-flash-free","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}

data: {"id":"id_1","object":"chat.completion.chunk","created":1790121332,"model":"mimo-v2.6-flash-free","choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":"düşünüyorum","tool_calls":null}}]}

data: {"id":"id_1","object":"chat.completion.chunk","created":1790121332,"model":"mimo-v2.6-flash-free","choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":"Mer","tool_calls":null}}]}

data: {"id":"id_1","object":"chat.completion.chunk","created":1790121332,"model":"mimo-v2.6-flash-free","choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":"haba","tool_calls":null}}]}

data: {"id":"id_1","object":"chat.completion.chunk","created":1790121332,"model":"mimo-v2.6-flash-free","choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":"}}]}}]}

data: {"id":"id_1","object":"chat.completion.chunk","created":1790121332,"model":"mimo-v2.6-flash-free","choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"tool_calls":[{"index":0,"id":"","type":"","function":{"name":"","arguments":"\"date\"}"}}]}}]}

data: {"id":"id_1","object":"chat.completion.chunk","created":1790121332,"model":"mimo-v2.6-flash-free","choices":[{"index":0,"finish_reason":"stop","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}

data: {"id":"id_1","object":"chat.completion.chunk","created":1790121332,"model":"mimo-v2.6-flash-free","choices":[],"usage":{"prompt_tokens":525,"completion_tokens":16,"total_tokens":541}}

data: [DONE]

data: {"choices":[],"cost":"0"}

`

func TestRelayDropsTheCostNoteAfterDone(t *testing.T) {
	recorder := httptest.NewRecorder()
	if _, err := relayStream(recorder, strings.NewReader(sampleStream), false); err != nil {
		t.Fatalf("relaying: %v", err)
	}
	out := recorder.Body.String()
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("the relay lost [DONE]:\n%s", out)
	}
	if strings.Contains(out, `"cost"`) {
		t.Errorf("the trailing cost note crossed:\n%s", out)
	}
	if strings.Contains(out, "Merhaba") {
		t.Errorf("content pieces were joined — the relay must copy lines verbatim:\n%s", out)
	}
	if !strings.Contains(out, `"content":"Mer"`) {
		t.Errorf("a content line was dropped:\n%s", out)
	}
}

func TestRelayCapturesUsage(t *testing.T) {
	recorder := httptest.NewRecorder()
	usage, err := relayStream(recorder, strings.NewReader(sampleStream), false)
	if err != nil {
		t.Fatalf("relaying: %v", err)
	}
	prompt, completion, _, _ := usageTokens(usage)
	if prompt != 525 || completion != 16 {
		t.Errorf("captured usage = %s (prompt %d, completion %d)", usage, prompt, completion)
	}
}

func TestRelayDropsUsageOnlyWhenNotAskedFor(t *testing.T) {
	recorder := httptest.NewRecorder()
	if _, err := relayStream(recorder, strings.NewReader(sampleStream), true); err != nil {
		t.Fatalf("relaying: %v", err)
	}
	if !strings.Contains(recorder.Body.String(), `"usage"`) {
		t.Errorf("the usage chunk was dropped although the client asked for it:\n%s",
			recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	if _, err := relayStream(recorder, strings.NewReader(sampleStream), false); err != nil {
		t.Fatalf("relaying: %v", err)
	}
	if strings.Contains(recorder.Body.String(), `"usage"`) {
		t.Errorf("the usage chunk crossed although the client did not ask for it:\n%s",
			recorder.Body.String())
	}
}

func TestAggregateBuildsACompletion(t *testing.T) {
	aggregated, usage, err := aggregateCompletion(strings.NewReader(sampleStream))
	if err != nil {
		t.Fatalf("aggregating: %v", err)
	}
	if len(usage) == 0 {
		t.Error("the aggregate lost the usage object")
	}

	var completion struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(aggregated, &completion); err != nil {
		t.Fatalf("the aggregate is not valid JSON: %v\n%s", err, aggregated)
	}
	if completion.Object != "chat.completion" || completion.ID != "id_1" {
		t.Errorf("completion header = %q %q", completion.Object, completion.ID)
	}
	if completion.Model != "mimo-v2.6-flash-free" {
		t.Errorf("model = %q", completion.Model)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(completion.Choices))
	}
	choice := completion.Choices[0]
	if choice.Message.Role != "assistant" || choice.Message.Content != "Merhaba" {
		t.Errorf("message = %q %q", choice.Message.Role, choice.Message.Content)
	}
	if choice.Message.ReasoningContent != "düşünüyorum" {
		t.Errorf("reasoning = %q", choice.Message.ReasoningContent)
	}
	if choice.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(choice.Message.ToolCalls))
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" ||
		call.Function.Name != "bash" || call.Function.Arguments != `{"command":"date"}` {
		t.Errorf("tool call = %+v", call)
	}
	if len(completion.Usage) == 0 {
		t.Error("usage was dropped")
	}
}
