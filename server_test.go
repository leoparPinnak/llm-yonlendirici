package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// fakeZen records the last upstream request and answers with a canned reply,
// standing in for the real service.
type fakeZen struct {
	gotRequest *http.Request
	gotBody    []byte
	respond    func(w http.ResponseWriter)
}

func (f *fakeZen) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.gotRequest = r
	f.gotBody = body
	f.respond(w)
}

// newTestGateway wires a gateway in front of a fake Zen and returns the
// client-facing server plus the recorder. The usage log lands in a
// per-test directory so the suite never writes into the package folder.
func newTestGateway(t *testing.T, respond func(w http.ResponseWriter)) (*httptest.Server, *fakeZen) {
	t.Helper()
	zen := &fakeZen{respond: respond}
	upstream := httptest.NewServer(zen)
	t.Cleanup(upstream.Close)

	cfg := defaultConfig()
	cfg.Upstream = upstream.URL
	tempDir := t.TempDir()
	cfg.UsageLogPath = filepath.Join(tempDir, "kullanim.jsonl")

	pool := NewProxyPool(filepath.Join(tempDir, "proxies.json"))
	proxyServer := NewProxyServer("127.0.0.1:0", pool, false)
	km, _ := NewKeyManager(filepath.Join(tempDir, "keys.json"))
	evrenProxy := NewProxyHandler(km)
	evrenServer := NewEvrenServer(km, evrenProxy)

	gw := newGateway(cfg, false, pool, proxyServer, evrenServer)
	t.Cleanup(gw.usage.Close)
	front := httptest.NewServer(gw.handler())
	t.Cleanup(front.Close)
	return front, zen
}

func TestEndToEndCostumesTheRequest(t *testing.T) {
	front, zen := newTestGateway(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleStream)
	})

	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"merhaba"}],"tools":[{"type":"function","function":{"name":"bash"}}],"stream":true,"stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer sk-client-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("calling the gateway: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("content-type = %q", got)
	}

	// The costume reached the upstream.
	upstreamRequest := zen.gotRequest
	if got := upstreamRequest.Header.Get("Authorization"); got != "Bearer public" {
		t.Errorf("upstream Authorization = %q, want Bearer public", got)
	}
	if got := upstreamRequest.Header.Get("User-Agent"); got != defaultConfig().UserAgent {
		t.Errorf("upstream User-Agent = %q", got)
	}
	project := upstreamRequest.Header.Get("X-Opencode-Project")
	if len(project) != 40 {
		t.Errorf("upstream project = %q, want 40 hex characters", project)
	}
	session := upstreamRequest.Header.Get("X-Opencode-Session")
	if !strings.HasPrefix(session, "ses_") ||
		session != upstreamRequest.Header.Get("X-Session-Id") ||
		session != upstreamRequest.Header.Get("X-Session-Affinity") {
		t.Errorf("upstream session headers = %q", session)
	}
	if upstreamRequest.Header.Get("Cookie") != "" || upstreamRequest.Header.Get("B3") != "" {
		t.Error("client identity leaked upstream")
	}
	if strings.Contains(string(zen.gotBody), "sk-client-secret") {
		t.Error("the client's credential leaked into the upstream body")
	}

	// The body reached the upstream costumed.
	var sent struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
		Stream   bool            `json:"stream"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(zen.gotBody, &sent); err != nil {
		t.Fatalf("the upstream body is not JSON: %v\n%s", err, zen.gotBody)
	}
	if sent.Model != "mimo-v2.6-flash-free" || !sent.Stream {
		t.Errorf("model/stream = %q %v", sent.Model, sent.Stream)
	}
	if !bytes.Equal(sent.Messages, json.RawMessage(`[{"role":"user","content":"merhaba"}]`)) {
		t.Errorf("messages = %s", sent.Messages)
	}
	names := map[string]bool{}
	for _, tool := range sent.Tools {
		names[tool.Function.Name] = true
	}
	if !names["bash"] {
		t.Error("the client's tool was dropped")
	}
	gated := 0
	for _, name := range defaultConfig().OpencodeTools {
		if names[name] {
			gated++
		}
	}
	if gated < defaultConfig().MinGateTools {
		t.Errorf("only %d opencode names reached the upstream", gated)
	}

	// The reply crossed filtered.
	body, _ := io.ReadAll(response.Body)
	text := string(body)
	if !strings.Contains(text, "data: [DONE]") || strings.Contains(text, `"cost"`) {
		t.Errorf("the stream was not filtered:\n%s", text)
	}
}

func TestEndToEndAggregatesWhenStreamIsNotAsked(t *testing.T) {
	front, _ := newTestGateway(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleStream)
	})

	response, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"merhaba"}]}`))
	if err != nil {
		t.Fatalf("calling the gateway: %v", err)
	}
	defer response.Body.Close()

	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), `"content":"Merhaba"`) {
		t.Errorf("the aggregate lost the content:\n%s", body)
	}
	if strings.Contains(string(body), `"cost"`) {
		t.Errorf("the cost note leaked into the aggregate:\n%s", body)
	}
}

func TestUpstreamFailurePassesThrough(t *testing.T) {
	front, _ := newTestGateway(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"FreeTierError","message":"no"}}`)
	})

	response, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"merhaba"}]}`))
	if err != nil {
		t.Fatalf("calling the gateway: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "FreeTierError") {
		t.Errorf("the upstream error body was lost:\n%s", body)
	}
}

func TestModelsAndHealthEndpoints(t *testing.T) {
	front, _ := newTestGateway(t, func(w http.ResponseWriter) {})

	response, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatalf("listing models: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "mimo-v2.6-flash-free") {
		t.Errorf("the model list lost the default model:\n%s", body)
	}

	health, err := http.Get(front.URL + "/healthz")
	if err != nil {
		t.Fatalf("reading health: %v", err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("health status = %d", health.StatusCode)
	}
}

func TestInvalidClientBodyIsRejected(t *testing.T) {
	front, _ := newTestGateway(t, func(w http.ResponseWriter) {
		t.Error("the upstream was called with an invalid body")
	})

	response, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`merhaba`))
	if err != nil {
		t.Fatalf("calling the gateway: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

func TestIntermittent403IsRetried(t *testing.T) {
	calls := 0
	front, _ := newTestGateway(t, func(w http.ResponseWriter) {
		calls++
		if calls <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"FreeTierError","message":"no"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleStream)
	})

	response, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"merhaba"}],"stream":true}`))
	if err != nil {
		t.Fatalf("calling the gateway: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d after %d upstream calls, want 200", response.StatusCode, calls)
	}
	if calls != 3 {
		t.Errorf("the upstream saw %d calls, want 2 rejections then a pass", calls)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Errorf("the retried stream was lost:\n%s", body)
	}
}

func TestTransientNetworkErrorIsRetried(t *testing.T) {
	calls := 0
	front, _ := newTestGateway(t, func(w http.ResponseWriter) {
		calls++
		if calls == 1 {
			// Simulate sudden connection abort / reset
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleStream)
	})

	response, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"merhaba"}],"stream":true}`))
	if err != nil {
		t.Fatalf("calling the gateway: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retry", response.StatusCode)
	}
	if calls != 2 {
		t.Errorf("upstream saw %d calls, want 2", calls)
	}
}

func TestPathsWithoutV1PrefixAreAccepted(t *testing.T) {
	front, zen := newTestGateway(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleStream)
	})

	response, err := http.Post(front.URL+"/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"merhaba"}]}`))
	if err != nil {
		t.Fatalf("calling the gateway: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 on the /v1-less path", response.StatusCode)
	}
	if zen.gotRequest == nil {
		t.Fatal("the /v1-less path never reached the upstream")
	}
}

func TestUsageUIAndStatsEndpoints(t *testing.T) {
	front, _ := newTestGateway(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleStream)
	})

	// One streamed exchange, so there is something to report.
	response, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"merhaba"}],"stream":true}`))
	if err != nil {
		t.Fatalf("calling the gateway: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)

	stats, err := http.Get(front.URL + "/kullanim")
	if err != nil {
		t.Fatalf("reading stats: %v", err)
	}
	defer stats.Body.Close()
	var report usageReport
	if err := json.NewDecoder(stats.Body).Decode(&report); err != nil {
		t.Fatalf("decoding stats: %v", err)
	}
	if report.Totals.Requests != 1 || report.Totals.Prompt != 525 || report.Totals.Completion != 16 {
		t.Errorf("totals = %+v", report.Totals)
	}
	if len(report.Sessions) != 1 || report.Sessions[0].Preview != "merhaba" {
		t.Errorf("sessions = %+v", report.Sessions)
	}
	if len(report.Recent) != 1 || !report.Recent[0].Streamed {
		t.Errorf("recent = %+v", report.Recent)
	}

	page, err := http.Get(front.URL + "/ui")
	if err != nil {
		t.Fatalf("reading the page: %v", err)
	}
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body), "zen-kostum") {
		t.Errorf("the usage page is missing: %d", page.StatusCode)
	}
	if got := page.Header.Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("page content-type = %q", got)
	}

	root, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatalf("reading the root: %v", err)
	}
	defer root.Body.Close()
	if root.Request.URL.Path != "/ui" {
		t.Errorf("the root redirect landed on %s", root.Request.URL.Path)
	}
}

// TestZenTranscriptKaydi Zen'e giden isteklerin de transcript'e yakalandığını
// kanıtlar: başarılı akış (giden messages + dönen asistan metni + finish_reason
// + usage, source:"zen") ve upstream 403 (ham hata gövdesi birebir).
func TestZenTranscriptKaydi(t *testing.T) {
	old := globalTranscripts
	t.Cleanup(func() { globalTranscripts = old })
	InitTranscripts(maxTranscriptConversations, maxTranscriptExchanges)

	mode := "ok"
	front, _ := newTestGateway(t, func(w http.ResponseWriter) {
		if mode == "err" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"zen-forbidden-marker","type":"auth"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ZEN-DENEME\"}}]}\n\n"+
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n"+
				"data: [DONE]\n\n")
	})

	// 1) Başarılı akış isteği
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"zen mesaji"}]}`))
	if err != nil {
		t.Fatalf("istek: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("beklenen 200, alındı: %d", resp.StatusCode)
	}

	// 2) Upstream 403 hatası
	mode = "err"
	respErr, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hatali zen istegi"}]}`))
	if err != nil {
		t.Fatalf("istek: %v", err)
	}
	errBody, _ := io.ReadAll(respErr.Body)
	_ = respErr.Body.Close()
	if respErr.StatusCode != http.StatusForbidden {
		t.Fatalf("403 hatası istemciye birebir geçmeli, alındı: %d", respErr.StatusCode)
	}
	if !strings.Contains(string(errBody), "zen-forbidden-marker") {
		t.Fatalf("hata gövdesi yutulmuş: %s", string(errBody))
	}

	var d TranscriptDump
	if err := json.Unmarshal(globalTranscripts.DumpBytes(), &d); err != nil {
		t.Fatalf("dump çözümlenemedi: %v", err)
	}
	if len(d.Conversations) != 2 {
		t.Fatalf("iki ayrı konuşma bekleniyordu, alındı: %d", len(d.Conversations))
	}

	// Yeni eklenen konuşma en başta (en yeniden eskiye)
	errConv, okConv := d.Conversations[0], d.Conversations[1]
	if okConv.Source != "zen" || errConv.Source != "zen" {
		t.Fatalf("kaynak 'zen' olmalı: %q / %q", errConv.Source, okConv.Source)
	}

	// Başarılı kayıt: giden messages + dönen yanıt + usage + zaman damgaları
	if okConv.RequestCount != 1 || len(okConv.Messages) != 2 {
		t.Fatalf("başarılı kayıt hatalı: istek=%d mesaj=%d", okConv.RequestCount, len(okConv.Messages))
	}
	okEx := okConv.Exchanges[0]
	if !strings.Contains(string(okEx.Request.Messages[0].Content), "zen mesaji") {
		t.Fatalf("giden messages kaydı eksik: %s", string(okEx.Request.Messages[0].Content))
	}
	if okEx.Response.Message == nil || !strings.Contains(string(okEx.Response.Message.Content), "ZEN-DENEME") {
		t.Fatalf("dönen asistan metni kayıtlı değil: %+v", okEx.Response.Message)
	}
	if okEx.Response.FinishReason != "stop" || okEx.Response.StatusCode != http.StatusOK {
		t.Fatalf("yanıt meta hatalı: finish=%q status=%d", okEx.Response.FinishReason, okEx.Response.StatusCode)
	}
	if okEx.Response.Usage == nil || okEx.Response.Usage.PromptTokens != 7 {
		t.Fatalf("usage kayıtlı değil: %+v", okEx.Response.Usage)
	}
	if okEx.Request.ReceivedAt == "" || okEx.Response.CompletedAt == "" {
		t.Fatal("zaman damgaları eksik")
	}

	// Hatalı kayıt: ham hata gövdesi + kod birebir
	errEx := errConv.Exchanges[0]
	if errEx.Response.StatusCode != http.StatusForbidden {
		t.Fatalf("hata durum kodu 403 olmalı: %d", errEx.Response.StatusCode)
	}
	if !strings.Contains(errEx.Response.Error, "HTTP 403") || !strings.Contains(errEx.Response.Error, "zen-forbidden-marker") {
		t.Fatalf("transcript hatası birebir olmalı: %q", errEx.Response.Error)
	}
	if !strings.Contains(string(errEx.Response.Raw), "zen-forbidden-marker") {
		t.Fatalf("ham hata gövdesi saklanmalı: %s", string(errEx.Response.Raw))
	}
}
