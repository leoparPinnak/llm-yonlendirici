package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// chunk is one event in a Zen chat-completions stream. Only the fields the
// gateway filters or aggregates on are decoded; everything else crosses
// untouched.
type chunk struct {
	ID      string          `json:"id"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []chunkChoice   `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
}

// chunkChoice is one choice inside a stream event.
type chunkChoice struct {
	Index        int        `json:"index"`
	FinishReason *string    `json:"finish_reason"`
	Delta        chunkDelta `json:"delta"`
}

// chunkDelta is the incremental payload of one choice.
type chunkDelta struct {
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []chunkToolCall `json:"tool_calls"`
}

// chunkToolCall is one tool-call fragment inside a delta.
type chunkToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// dataPayload returns the payload of an SSE data line, and whether the line
// is one.
func dataPayload(line []byte) (string, bool) {
	trimmed := string(bytes.TrimRight(line, "\r\n"))
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")), true
}

// streamFilter decides which upstream stream events reach the client: usage
// chunks only when the client asked for them, and nothing after [DONE],
// which is where Zen's trailing cost note arrives. The usage object itself
// is always captured, whatever the client asked to see. It also accumulates
// the assistant content and finish_reason for the transcript store.
type streamFilter struct {
	passUsage bool
	done      bool
	usage     json.RawMessage
	content   strings.Builder
	finish    string
}

// keepLine reports whether a raw stream line crosses to the client.
func (f *streamFilter) keepLine(line []byte) bool {
	payload, isData := dataPayload(line)
	if !isData {
		return true
	}
	if f.done {
		return false
	}
	if payload == "[DONE]" {
		f.done = true
		return true
	}
	var event chunk
	if err := json.Unmarshal([]byte(payload), &event); err == nil {
		for _, choice := range event.Choices {
			if choice.Delta.Content != "" {
				f.content.WriteString(choice.Delta.Content)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				f.finish = *choice.FinishReason
			}
		}
		if len(event.Usage) > 0 {
			f.usage = event.Usage
			if len(event.Choices) == 0 {
				return f.passUsage
			}
		}
	}
	return true
}

// streamCapture, relay edilen akıştan transkript için derlenen içeriktir.
type streamCapture struct {
	Content string // birleştirilmiş asistan metni
	Finish  string // son finish_reason
	Done    bool   // upstream [DONE] gönderdi mi
	Err     string // kopma varsa kontrollü hata mesajı
}

// relayStream copies the upstream event stream to the client, dropping the
// events the filter rejects and flushing after every line so chunks are not
// held back. It returns the usage object seen on the stream, if any.
func relayStream(dst http.ResponseWriter, src io.Reader, passUsage bool) (json.RawMessage, error) {
	usage, _, err := relayStreamCaptured(dst, src, passUsage)
	return usage, err
}

// relayStreamCaptured aynı relay'i yapar, ayrıca transkript için içerik ve
// finish_reason derler. Akış [DONE] GELMEDEN koparsa istemciyi pat diye
// kapatmaz: kontrollü hata chunk'ı (veya tamamlama chunk'ı) + [DONE] gönderir;
// istemci "stream closed before a finish_reason was received" ile çökmez.
func relayStreamCaptured(dst http.ResponseWriter, src io.Reader, passUsage bool) (json.RawMessage, *streamCapture, error) {
	filter := &streamFilter{passUsage: passUsage}
	reader := bufio.NewReaderSize(src, 32*1024)

	emit := func(b []byte) {
		if _, err := dst.Write(b); err == nil {
			if flusher, canFlush := dst.(http.Flusher); canFlush {
				flusher.Flush()
			}
		}
	}
	finishChunk := []byte(`data: {"id":"stream-cutoff","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"stop"}]}` + "\n\n")
	doneChunk := []byte("data: [DONE]\n\n")

	var writeErr, readErr error
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && filter.keepLine(line) {
			if _, werr := dst.Write(line); werr != nil {
				writeErr = werr
				break
			}
			if flusher, canFlush := dst.(http.Flusher); canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}

	capture := &streamCapture{
		Content: filter.content.String(),
		Finish:  filter.finish,
		Done:    filter.done,
	}

	// Kontrollü sonlandırma: istemci soketini sert kapatma.
	if writeErr == nil && readErr == nil && !filter.done {
		// Upstream [DONE] göndermeden kapattı.
		if filter.finish == "" {
			emit(finishChunk)
		}
		emit(doneChunk)
	}
	if writeErr == nil && readErr != nil {
		// Ağ kopması / zaman aşımı / buffer hatası: kontrollü hata chunk'ı.
		capture.Err = fmt.Sprintf("Zen SSE akışı yarıda kesildi: %v", readErr)
		if b, merr := json.Marshal(map[string]any{"error": map[string]any{
			"message": capture.Err,
			"type":    "stream_interrupted",
			"code":    "stream_interrupted",
		}}); merr == nil {
			emit(append([]byte("data: "), append(b, "\n\n"...)...))
		}
		emit(doneChunk)
	}

	if writeErr != nil {
		return filter.usage, capture, writeErr
	}
	if readErr != nil {
		return filter.usage, capture, readErr
	}
	return filter.usage, capture, nil
}

// aggregator assembles a streamed completion into the single JSON object a
// non-streaming client expects.
type aggregator struct {
	id        string
	created   int64
	model     string
	role      string
	content   strings.Builder
	reasoning strings.Builder
	finish    string
	hadFinish bool
	usage     json.RawMessage
	calls     []*callAccum
	byIndex   map[int]*callAccum
}

// callAccum accumulates one tool call's streamed fragments.
type callAccum struct {
	id   string
	typ  string
	name string
	args strings.Builder
}

// add folds one stream payload into the completion being assembled.
func (a *aggregator) add(payload string) {
	var event chunk
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return
	}
	if a.id == "" {
		a.id = event.ID
		a.created = event.Created
		a.model = event.Model
	}
	for _, choice := range event.Choices {
		if choice.Delta.Role != "" {
			a.role = choice.Delta.Role
		}
		a.content.WriteString(choice.Delta.Content)
		a.reasoning.WriteString(choice.Delta.ReasoningContent)
		if choice.FinishReason != nil {
			a.finish = *choice.FinishReason
			a.hadFinish = true
		}
		for _, call := range choice.Delta.ToolCalls {
			if a.byIndex == nil {
				a.byIndex = make(map[int]*callAccum)
			}
			accum := a.byIndex[call.Index]
			if accum == nil {
				accum = &callAccum{typ: "function"}
				a.byIndex[call.Index] = accum
				a.calls = append(a.calls, accum)
			}
			if accum.id == "" {
				accum.id = call.ID
			}
			if accum.name == "" {
				accum.name = call.Function.Name
			}
			accum.args.WriteString(call.Function.Arguments)
		}
	}
	if len(event.Usage) > 0 {
		a.usage = event.Usage
	}
}

// completion renders the assembled chat.completion.
func (a *aggregator) completion() ([]byte, error) {
	message := map[string]any{
		"role":    orDefault(a.role, "assistant"),
		"content": a.content.String(),
	}
	if reasoning := a.reasoning.String(); reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(a.calls) > 0 {
		calls := make([]map[string]any, 0, len(a.calls))
		for _, accum := range a.calls {
			calls = append(calls, map[string]any{
				"id":   accum.id,
				"type": orDefault(accum.typ, "function"),
				"function": map[string]any{
					"name":      accum.name,
					"arguments": accum.args.String(),
				},
			})
		}
		message["tool_calls"] = calls
	}

	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": nil,
	}
	if a.hadFinish {
		choice["finish_reason"] = a.finish
	}

	out := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{choice},
	}
	if len(a.usage) > 0 {
		out["usage"] = a.usage
	}
	return json.Marshal(out)
}

// aggregateCompletion reads an upstream event stream and answers with the
// single JSON body a client that asked stream:false expects, alongside the
// usage object seen on the stream.
func aggregateCompletion(src io.Reader) ([]byte, json.RawMessage, error) {
	agg := &aggregator{}
	reader := bufio.NewReaderSize(src, 32*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if payload, isData := dataPayload(line); isData {
			if payload == "[DONE]" {
				break
			}
			agg.add(payload)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, agg.usage, err
		}
	}
	body, err := agg.completion()
	return body, agg.usage, err
}

// orDefault returns value when it is not empty, otherwise fallback.
func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
