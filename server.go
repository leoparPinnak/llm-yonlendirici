package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// gateway serves the client-facing endpoints and owns the upstream pipeline.
type gateway struct {
	cfg         config
	costume     *costume
	client      *http.Client
	verbose     bool
	usage       *usageLog
	started     time.Time
	catalog     *ModelCatalog
	pool        *ProxyPool
	proxyServer *ProxyServer
	evrenServer *EvrenServer
}

// newGateway builds the gateway around a transport that dials through the
// embedded proxy pool, so every request to Zen can rotate across proxies
// and bypass IP rate limits seamlessly.
func newGateway(cfg config, verbose bool, pool *ProxyPool, proxyServer *ProxyServer, evrenServer *EvrenServer) *gateway {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	// Gömülü Proxy Havuzlu DialContext:
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		proxyItem, err := pool.SelectProxy()
		if err == nil && proxyItem != nil && proxyItem.RawURL != "" {
			dialer, dErr := NewProxyDialer(proxyItem.RawURL, 10*time.Second)
			if dErr == nil {
				start := time.Now()
				tunnelID := proxyServer.startTunnel(addr, proxyItem.Host, proxyItem.Scheme, 0, 0, "zen-kostum")
				conn, cErr := dialer.DialContext(ctx, network, addr)
				if cErr == nil {
					pool.RecordResult(proxyItem.ID, true)
					proxyServer.finishTunnel(tunnelID, "OK", start, 0, 0, addr, proxyItem.Host, proxyItem.Scheme, 0, 0, "zen-kostum")
					return conn, nil
				}
				pool.RecordResult(proxyItem.ID, false)
				proxyServer.finishTunnel(tunnelID, cErr.Error(), start, 0, 0, addr, proxyItem.Host, proxyItem.Scheme, 0, 0, "zen-kostum")
			}
		}

		var defaultDialer net.Dialer
		return defaultDialer.DialContext(ctx, network, addr)
	}

	var usage *usageLog
	if cfg.UsageLogPath != "-" {
		if opened, err := openUsageLog(cfg.UsageLogPath); err != nil {
			fmt.Fprintf(os.Stderr, "zen-kostum: kullanım kaydı açılamadı (%s): %v\n",
				cfg.UsageLogPath, err)
		} else {
			usage = opened
		}
	}
	client := &http.Client{
		Transport: transport,
	}
	cost := newCostume(cfg.UserAgent)
	catalog := newModelCatalog(cfg.Upstream, cost, client, verbose)

	return &gateway{
		cfg:         cfg,
		costume:     cost,
		client:      client,
		verbose:     verbose,
		usage:       usage,
		started:     time.Now(),
		catalog:     catalog,
		pool:        pool,
		proxyServer: proxyServer,
		evrenServer: evrenServer,
	}
}

// handler routes the client-facing endpoints and embedded proxy management.
func (g *gateway) handler() http.Handler {
	mux := http.NewServeMux()

	// Zen Kostüm API uç noktaları
	mux.HandleFunc("/v1/chat/completions", g.completions)
	mux.HandleFunc("/chat/completions", g.completions)
	mux.HandleFunc("/v1/models", g.models)
	mux.HandleFunc("/models", g.models)
	mux.HandleFunc("/healthz", g.health)

	// Zen Kostüm & Genel Arayüzler
	mux.HandleFunc("/ui", g.serveUI)
	mux.HandleFunc("/ui/proxy", g.proxyServer.serveProxyUI)
	mux.HandleFunc("/proxy", g.proxyServer.serveProxyUI)
	mux.HandleFunc("/ui/evren", g.evrenServer.handleDashboard)
	mux.HandleFunc("/dashboard", g.evrenServer.handleDashboard)
	mux.HandleFunc("/kullanim", g.serveStats)

	// Gömülü Proxy Yönlendirici REST API Uç Noktaları
	mux.HandleFunc("/api/status", g.proxyServer.apiGetStatus)
	mux.HandleFunc("/api/proxies", g.proxyServer.apiAddProxies)
	mux.HandleFunc("/api/proxies/delete", g.proxyServer.apiDeleteProxy)
	mux.HandleFunc("/api/proxies/clear", g.proxyServer.apiClearProxies)
	mux.HandleFunc("/api/mode", g.proxyServer.apiSetMode)
	mux.HandleFunc("/api/tunnels/close", g.proxyServer.apiCloseTunnels)
	mux.HandleFunc("/api/check", g.proxyServer.apiCheck)

	// EVREN REST API Uç Noktaları (8110 Portu üzerinden)
	mux.HandleFunc("/api/evren/status", g.evrenServer.handleStatus)
	mux.HandleFunc("/api/evren/keys", g.evrenServer.handleKeys)
	mux.HandleFunc("/api/evren/keys/", g.evrenServer.handleKeyAction)
	mux.HandleFunc("/api/evren/reset-now", g.evrenServer.handleResetNow)
	mux.HandleFunc("/api/evren/unexhaust-all", g.evrenServer.handleUnexhaustAll)
	mux.HandleFunc("/api/evren/logs", g.evrenServer.handleLogs)
	mux.HandleFunc("/api/evren/logs/clear-session", g.evrenServer.handleClearSessionLogs)
	mux.HandleFunc("/api/evren/logs/clear-all-sessions", g.evrenServer.handleClearAllSessions)
	mux.HandleFunc("/api/evren/logs/embeddings", g.evrenServer.handleEmbeddingLogs)
	// Ses (ASR) istek günlüğü (8110 portundan da okunur)
	mux.HandleFunc("/api/evren/logs/audio", g.evrenServer.handleAudioLogs)
	mux.HandleFunc("/api/logs/audio", g.evrenServer.handleAudioLogs)
	// Karar (decision) istek günlüğü — /v1/systemone
	mux.HandleFunc("/api/evren/logs/decisions", g.evrenServer.handleDecisionLogs)
	mux.HandleFunc("/api/logs/decisions", g.evrenServer.handleDecisionLogs)
	mux.HandleFunc("/api/evren/logs/transcript", g.evrenServer.handleTranscriptDump)
	// Oturum bazlı tek oturum logu: /api/evren/logs/transcript/<oturum_id>
	mux.HandleFunc("/api/evren/logs/transcript/", g.evrenServer.handleTranscriptSession)
	// Oturum listesi + oturuma özel üretilmiş endpoint URL'leri
	// (alt ağacın üstüne çıkar: Go'da en uzun eşleşme kazanır)
	mux.HandleFunc("/api/evren/logs/transcript/sessions", g.evrenServer.handleTranscriptSessions)
	// Karta ✏️ ile verilen özel ad (RAM)
	mux.HandleFunc("/api/evren/logs/transcript/rename", g.evrenServer.handleTranscriptRename)
	mux.HandleFunc("/api/evren/auto-switch", g.evrenServer.handleAutoSwitch)
	// Model bazlı görsel sınırı paneli (8110 portundan da okunur/yazılır)
	mux.HandleFunc("/api/evren/image-limits", g.evrenServer.handleImageLimits)

	// EVREN /evren/v1/ rotası (8110 portundan EVREN istekleri)
	mux.HandleFunc("/evren/v1/", func(w http.ResponseWriter, r *http.Request) {
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/evren")
		g.evrenServer.ServeHTTP(w, r2)
	})

	// SES (ASR) uç noktası — 8110 portundan DOĞRUDAN: /v1/audio/transcriptions
	// (ve /audio/translations). İstek evren proxy'sine devredilir: config'teki
	// upstream_base'e (https://evren-llmapi.ssyz.org.tr/v1) ve AKTİF KARTIN
	// anahtarıyla gider. Gövde multipart'tır; sohbet gövdesi (messages)
	// taşımadığı için oturum atama mantığı bu isteklerde HİÇ devreye girmez —
	// gömülü (embedding) yolların birebir aynısı. Zen kostümünün chat uçlarına
	// ve /v1/models kataloğuna dokunulmaz.
	mux.HandleFunc("/v1/audio/transcriptions", g.evrenServer.proxy.ServeHTTP)
	mux.HandleFunc("/audio/transcriptions", g.evrenServer.proxy.ServeHTTP)
	mux.HandleFunc("/v1/audio/translations", g.evrenServer.proxy.ServeHTTP)

	// GÖMÜLÜ (EMBEDDING) uç noktası — 8110 portundan DOĞRUDAN. 8088'de zaten
	// vardı; ses uç noktasıyla aynı desen: gövde JSON'dur ve "messages"
	// taşımadığı için oturum atama mantığı devreye girmez.
	mux.HandleFunc("/v1/embeddings", g.evrenServer.proxy.ServeHTTP)
	mux.HandleFunc("/embeddings", g.evrenServer.proxy.ServeHTTP)

	// KARAR (DECISION) uç noktası — POST /v1/systemone (clef = görsel destekli,
	// laya = metin). Metin ÜRETMEZ: state + choice/score/noul sorularını tek
	// geçişte olasılıklarıyla yanıtlar; chat uçlarına ve oturum atamaya
	// dokunulmaz (gövdeler "messages" taşımaz).
	mux.HandleFunc("/v1/systemone", g.evrenServer.proxy.ServeHTTP)
	mux.HandleFunc("/systemone", g.evrenServer.proxy.ServeHTTP)

	// Sürükle-bırak SES TEST SAYFASI (bağımsız HTML, ikiliye gömülü):
	// http://127.0.0.1:8110/asr-test — ses dosyasını bırak, transkripti gör.
	// Statik içerik: durum okumaz/yazmaz, oturum atamaya dokunmaz.
	mux.HandleFunc("/asr-test", handleASRTestPage)
	mux.HandleFunc("/ses-test", handleASRTestPage)

	// Root & Forward Proxy Handlers
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			g.proxyServer.handleConnect(w, r)
			return
		}
		if r.URL.IsAbs() || r.Header.Get("Proxy-Connection") != "" {
			g.proxyServer.handleHTTPForward(w, r)
			return
		}
		g.serveRoot(w, r)
	})
	return mux
}

// completions repacks a chat-completions request into the OpenCode costume,
// sends it upstream, and streams or aggregates the reply back.
func (g *gateway) completions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		http.Error(w, "zen-kostum: POST the chat-completions body here", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxClientBytes))
	if err != nil {
		http.Error(w, "zen-kostum: cannot read the request body", http.StatusBadRequest)
		return
	}

	request, err := parseClientRequest(body)
	if err != nil {
		http.Error(w, "zen-kostum: invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// PROTOKOL UYUMLU TRANSCRIPT (Zen yolu): giden messages + dönen yanıt,
	// mutlak zaman damgalı olarak bellekteki transkripte yazılır.
	trReq := BuildTranscriptRequest(body, "zen", started)
	if trReq != nil && trReq.ConversationID == "" {
		// Kademe 1 uzantısı: gövde id'si yoksa header'daki oturum ipucu
		// (X-Conversation-Id / X-Session-Id) kullanılır; DSH kimliklerine
		// geri-uyumlu "dsh-" öneki uygulanır (eski kartlar ham kalır).
		trReq.ConversationID = CanonicalSessionID(HeaderConversationHint(r))
	}
	if trReq != nil {
		// Subagent soy ipucu: evren yoluyla AYNI kayıt defterine child→parent
		// bağı yazılır — yalnızca kitaplık bookkeeping'i, zen proxy akışına
		// dokunulmaz.
		trReq.ParentSessionID = CanonicalSessionID(HeaderParentSessionHint(r))
	}

	session, preview := sessionFingerprint(request.Messages)
	if trReq != nil && trReq.ConversationID != "" {
		// Açık oturum id'si varsa /kullanim istatistik oturumu da ONU kullanır;
		// içerik fingerprint'i yalnızca yedek (id'siz istemciler için).
		session = trReq.ConversationID
	}
	model := request.Model
	if model == "" {
		model = g.cfg.DefaultModel
	}
	transcriptDone := false
	recordTranscript := func(resp TranscriptResponse) {
		if trReq == nil || transcriptDone {
			return
		}
		transcriptDone = true
		if resp.CompletedAt == "" {
			resp.CompletedAt = tsNow()
		}
		if resp.DurationMs == 0 {
			resp.DurationMs = time.Since(started).Milliseconds()
		}
		globalTranscripts.Record(trReq, resp)
	}

	repacked, err := request.repack(g.cfg)
	if err != nil {
		http.Error(w, "zen-kostum: cannot repack the request: "+err.Error(), http.StatusBadRequest)
		return
	}

	clientPort := 0
	if _, portStr, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		clientPort, _ = strconv.Atoi(portStr)
	}
	clientPID, clientProc := resolveClientInfo(clientPort)

	reqID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), clientPort)
	activeReq := &ActiveRequest{
		ID:         reqID,
		StartTime:  started,
		ClientPort: clientPort,
		ClientPID:  clientPID,
		ClientProc: clientProc,
		Session:    session,
		Preview:    preview,
		Model:      model,
		Streamed:   request.wantsStream(),
	}
	g.usage.startRequest(activeReq)
	defer g.usage.finishRequest(reqID)

	response, err := g.sendUpstream(r, repacked)
	if err != nil {
		http.Error(w, "zen-kostum: "+err.Error(), http.StatusBadGateway)
		recordTranscript(TranscriptResponse{StatusCode: http.StatusBadGateway, Error: "upstream bağlantı hatası: " + err.Error()})
		g.recordUsage(session, preview, model, http.StatusBadGateway, 0, 0, 0, 0, started, request.wantsStream(), clientPort, clientPID, clientProc)
		g.note(r, started, http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		// Upstream hata gövdesini hem logla hem transkripte birebir kaydet,
		// istemciye AYNI gövde ve AYNI kod ile ilet (hiçbir şey yutulmaz).
		upstreamBody, _ := io.ReadAll(response.Body)
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		removeHopByHop(w.Header())
		w.Header().Del("Content-Length")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(upstreamBody)

		upstreamErrMsg := fmt.Sprintf("HTTP %d | %s", response.StatusCode, strings.TrimSpace(string(upstreamBody)))
		log.Printf("[UPSTREAM HATA] ZEN %s %s -> HTTP %d | gövde: %s", r.Method, r.URL.Path, response.StatusCode, strings.TrimSpace(string(upstreamBody)))
		recordTranscript(TranscriptResponse{
			StatusCode: response.StatusCode,
			Error:      upstreamErrMsg,
			Raw:        rawJSONOrString(upstreamBody),
		})
		g.recordUsage(session, preview, model, response.StatusCode, 0, 0, 0, 0, started, request.wantsStream(), clientPort, clientPID, clientProc)
		g.note(r, started, response.StatusCode)
		return
	}

	for name, values := range response.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	removeHopByHop(w.Header())

	if request.wantsStream() {
		rawUsage, capture, _ := relayStreamCaptured(w, response.Body, request.passUsage())
		prompt, comp, reas, cached := usageTokens(rawUsage)

		// TRANSCRIPT: derlenen asistan metni + finish_reason + usage.
		trResp := TranscriptResponse{
			StatusCode: http.StatusOK,
			Stream:     true,
			DurationMs: time.Since(started).Milliseconds(),
		}
		if capture != nil {
			if capture.Content != "" {
				c, _ := json.Marshal(capture.Content)
				trResp.Message = &TranscriptMessage{Role: "assistant", Content: c, Timestamp: tsNow()}
			}
			trResp.FinishReason = capture.Finish
			trResp.Error = capture.Err
		}
		usage := CalculateUsage(int64(prompt), int64(cached), int64(comp))
		trResp.Usage = &usage
		recordTranscript(trResp)

		g.recordUsage(session, preview, model, http.StatusOK, prompt, comp, reas, cached, started, true, clientPort, clientPID, clientProc)
		g.note(r, started, http.StatusOK)
		return
	}

	aggregated, rawUsage, err := aggregateCompletion(response.Body)
	if err != nil {
		http.Error(w, "zen-kostum: cannot decode upstream stream: "+err.Error(), http.StatusBadGateway)
		recordTranscript(TranscriptResponse{StatusCode: http.StatusBadGateway, Error: "upstream stream çözümlenemedi: " + err.Error(), DurationMs: time.Since(started).Milliseconds()})
		g.recordUsage(session, preview, model, http.StatusBadGateway, 0, 0, 0, 0, started, false, clientPort, clientPID, clientProc)
		g.note(r, started, http.StatusBadGateway)
		return
	}
	prompt, comp, reas, cached := usageTokens(rawUsage)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(aggregated)

	// TRANSCRIPT: tamamlanmış yanıt (asistan mesajı + finish_reason + usage).
	msg, finish := extractAssistantMessage(aggregated, time.Now())
	usage := CalculateUsage(int64(prompt), int64(cached), int64(comp))
	recordTranscript(TranscriptResponse{
		StatusCode:   http.StatusOK,
		Message:      msg,
		FinishReason: finish,
		Raw:          rawJSONOrString(aggregated),
		DurationMs:   time.Since(started).Milliseconds(),
		Usage:        &usage,
	})

	g.recordUsage(session, preview, model, http.StatusOK, prompt, comp, reas, cached, started, false, clientPort, clientPID, clientProc)
	g.note(r, started, http.StatusOK)
}

func (g *gateway) recordUsage(session, preview, model string, status, prompt, comp, reas, cached int, started time.Time, streamed bool, clientPort int, clientPID uint32, clientProc string) {
	rec := usageRecord{
		Time:             started,
		Session:          session,
		Preview:          preview,
		Model:            model,
		PromptTokens:     prompt,
		CompletionTokens: comp,
		ReasoningTokens:  reas,
		CachedTokens:     cached,
		Status:           status,
		DurationMs:       time.Since(started).Milliseconds(),
		Streamed:         streamed,
		ClientPort:       clientPort,
		ClientPID:        clientPID,
		ClientProc:       clientProc,
	}
	g.usage.record(rec)
}

func (g *gateway) note(r *http.Request, started time.Time, status int) {
	if g.verbose {
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, status, time.Since(started).Round(time.Millisecond))
	}
}

// retry403Delay is the pause between attempts at an intermittently
// rejected free-tier request.
const retry403Delay = 750 * time.Millisecond

// isTransientNetworkError reports whether an upstream transport error
// (such as bad record MAC, connection reset, or timeout) is worth retrying.
func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "tls: bad record mac") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "reset by peer") ||
		strings.Contains(msg, "forcibly closed") ||
		strings.Contains(msg, "aborted") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "eof")
}

// sendUpstream posts the repacked body to Zen, retrying an intermittent
// 403, 429 IP rate limit, transient network failure, or a flapping 5xx
// provider outage with a freshly dressed request.
func (g *gateway) sendUpstream(r *http.Request, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
			g.cfg.Upstream+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("cannot form upstream request: %w", err)
		}
		g.costume.dress(upstream.Header)

		response, err := g.client.Do(upstream)
		if err != nil {
			if attempt < g.cfg.Retry403 && isTransientNetworkError(err) {
				if g.verbose {
					log.Printf("%s %s -> ağ/TLS hatası (%v), yeniden deneniyor (%d/%d)",
						r.Method, r.URL.Path, err, attempt+1, g.cfg.Retry403)
				}
				if tr, ok := g.client.Transport.(*http.Transport); ok {
					tr.CloseIdleConnections()
				}
				select {
				case <-time.After(retry403Delay):
					continue
				case <-r.Context().Done():
					return nil, fmt.Errorf("the client went away: %w", r.Context().Err())
				}
			}
			return nil, fmt.Errorf("upstream unreachable: %w", err)
		}

		// 403/429 (kostüm/IP limiti) VE 5xx (provider geçici sağlıksızlığı —
		// ör. "Endpoint is unavailable") durumlarında proxy rotasyonuyla yeniden
		// dene. 5xx'te bekleme biraz daha geniş: provider'ın ayağa kalkma
		// pencerelerini yakala. Deneme bitince yanıt BİREBİR relay edilir.
		statusRetry := response.StatusCode == http.StatusForbidden ||
			response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError
		if statusRetry && attempt < g.cfg.Retry403 {
			response.Body.Close()
			delay := retry403Delay
			if response.StatusCode >= http.StatusInternalServerError {
				delay = 2 * time.Second
			}
			if g.verbose {
				log.Printf("%s %s -> HTTP %d, proxy rotasyonu ile yeniden deneniyor (%d/%d)",
					r.Method, r.URL.Path, response.StatusCode, attempt+1, g.cfg.Retry403)
			}
			if tr, ok := g.client.Transport.(*http.Transport); ok {
				tr.CloseIdleConnections()
			}
			select {
			case <-time.After(delay):
				continue
			case <-r.Context().Done():
				return nil, fmt.Errorf("the client went away: %w", r.Context().Err())
			}
		}

		return response, nil
	}
}

// models answers the catalog endpoint dynamically with rich model metadata from OpenCode Zen.
func (g *gateway) models(w http.ResponseWriter, r *http.Request) {
	returnAll := r.URL.Query().Get("all") == "true"
	models := g.catalog.GetModels(r.Context(), returnAll)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

// health answers the liveness endpoint.
func (g *gateway) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("cannot encode json response: %v", err)
	}
}
