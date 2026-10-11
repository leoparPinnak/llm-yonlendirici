package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ProxyHandler struct {
	km         *KeyManager
	httpClient *http.Client
	// streamIdle, gövde okumaları arasındaki sessizlik eşiğidir (0 = varsayılan).
	// Üretimde streamIdleTimeout kullanılır; yalnızca testler kısa değer koyar.
	streamIdle time.Duration
}

// streamIdleTimeout, upstream'ten art arda iki okuma arasındaki maksimum
// sessizliktir. Ölçüm (04.10.2026 oturum kayıtları): sağlıklı üretimde deltalar
// arası ~23 ms; 2 dakikalık sessizlik gerçek bir asılı kalmadır. Toplam ömür
// sınırı BİLEREK yoktur: eski 600 sn'lik http.Client.Timeout, uzun üreten dev
// bağlamlı istekleri gövde okunurken biçiyordu (usage_history'de
// duration_ms=6000xx, completion_tokens=0 kayıtları). Bağlanma ve başlıklar
// ayrı, kısa ve kesin sürelerle sınırlandırılır (Dialer.Timeout /
// ResponseHeaderTimeout).
const streamIdleTimeout = 2 * time.Minute

// maxSSELineBytes, tek satır SSE (dev tool_call JSON'u vb.) için bufio.Scanner
// tavanıdır: bufio.ErrTooLong'un yanıltıcı kesme üretmemesi için tavan geniştir,
// başlangıç tamponu 64 KB kalır ve tavan yalnız gerektiğinde devreye girer.
const maxSSELineBytes = 8 * 1024 * 1024

// idleBody, gövde okumaları arasında idle kadar sessizlik bırakılırsa kaynağı
// kapatan sarmalayıcıdır. Bekleyen okuma kırılır; akış pompası bunu
// scanner.Err() ile "timeout/ağ/buffer" dalında yakalar ve kontrollü
// stream_interrupted + [DONE] üretir. Gövdeyi tüketen her yol bu korumadan
// geçtiği için akış ve akış-dışı okumalar aynı güvence altındadır.
type idleBody struct {
	body  io.ReadCloser
	idle  time.Duration
	timer *time.Timer
	mu    sync.Mutex
}

func newIdleBody(body io.ReadCloser, idle time.Duration) io.ReadCloser {
	if idle <= 0 {
		idle = streamIdleTimeout
	}
	ib := &idleBody{body: body, idle: idle}
	ib.timer = time.AfterFunc(idle, func() {
		log.Printf("[STREAM IDLE] %s boyunca upstream'ten bayt gelmedi, gövde kontrollü kapatılıyor", idle)
		_ = body.Close()
	})
	return ib
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err == nil {
		b.mu.Lock()
		b.timer.Reset(b.idle)
		b.mu.Unlock()
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	b.timer.Stop()
	b.mu.Unlock()
	return b.body.Close()
}

func NewProxyHandler(km *KeyManager) *ProxyHandler {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
		},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// Faz bazlı süreler: bağlanma (Dialer) ve yanıt başlıkları kesin ve kısa
		// sürelerle sınırlıdır; GÖVDE için toplam ömür sınırı yoktur (bkz.
		// streamIdleTimeout). Eski yapı 600 sn'lik toplam http.Client.Timeout
		// kullanıyordu; bu, uzun üreten dev bağlamlı istekleri akış ortasında
		// biçiyordu (usage_history: duration_ms=6000xx, completion_tokens=0).
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 60 * time.Second,
	}

	return &ProxyHandler{
		km:         km,
		httpClient: &http.Client{Transport: transport},
		streamIdle: streamIdleTimeout,
	}
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	cfg := GetConfig()

	// 0. CORS (YALNIZCA yerel kaynaklar): sürükle-bırak test sayfası file://
	// olarak açıldığında tarayıcı yanıtı okuyabilsin. Uzak siteler için başlık
	// EKLENMEZ: rastgele bir web sayfası bu gateway'i kullanıp kota tüketemez.
	// Oturum/anahtar mantığına dokunulmaz — burada yalnızca yanıt başlığı
	// yazılır ve yerel OPTIONS isteği kısa yoldan yanıtlanır.
	if origin := r.Header.Get("Origin"); isLocalOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, x-audio-seconds")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// X-Audio-Seconds: YALNIZCA gateway'in ses sayacı için kullanılan iç başlık
	// (sürükle-bırak test sayfası sesin gerçek süresini buradan bildirir).
	// Upstream'e GÖNDERİLMEZ; gövde ve diğer tüm başlıklar aynen geçer.
	clientAudioSeconds := parseSecondsHeader(r.Header.Get("X-Audio-Seconds"))
	if clientAudioSeconds > 0 {
		r.Header.Del("X-Audio-Seconds")
	}

	// X-Source-File: yine panelin gönderdiği İÇ başlık — "hangi dosya işlendi"
	// bilgisini günlüğe yazmak için (upstream'e gönderilmez, gövde değişmez).
	sourceFile := strings.TrimSpace(r.Header.Get("X-Source-File"))
	if sourceFile != "" {
		if len(sourceFile) > 160 {
			sourceFile = sourceFile[:160]
		}
		r.Header.Del("X-Source-File")
	}

	// 1. İstek gövdesini oku ve model/stream/embedding kontrolü yap
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"İstek gövdesi okunamadı","type":"proxy_error"}}`, http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	// 1b. MODEL BAZLI GÖRSEL SINIRI: upstream her model için farklı bir görsel
	// sınırı koyabilir (ör. free havuzda mimo-v2.6-pro için 8). Aşan istek 400
	// "At most 8 image(s)" ile döner ve istemci tarihçeyi her tur yeniden
	// gönderdiği için hata, sınır aşıldığı sürece HER TURDA tekrar eder.
	// Burada yalnızca yapılandırmada sınırı OLAN modeller kırpılır: en ESKİ
	// görseller metne çevrilir, en yenileri korunur. Hata durumunda istek
	// olduğu gibi geçer (fail-open): asla isteği bozmaz.
	imageLimit := cfg.imageLimitForModel(chatModelName(bodyBytes))
	if imageLimit > 0 {
		if capped, dropped, capErr := capImagesInChatBody(bodyBytes, imageLimit); capErr == nil && dropped > 0 {
			bodyBytes = capped
			log.Printf("[GÖRSEL SINIR] model=%s: %d görsel sınırdan çıkarıldı (limit %d, istekte %d kaldı)",
				chatModelName(capped), dropped, imageLimit, imageLimit)
		}
	}

	if bytes.Contains(bodyBytes, []byte(`"role":"developer"`)) {
		bodyBytes = bytes.ReplaceAll(bodyBytes, []byte(`"role":"developer"`), []byte(`"role":"system"`))
	}
	if bytes.Contains(bodyBytes, []byte(`"role": "developer"`)) {
		bodyBytes = bytes.ReplaceAll(bodyBytes, []byte(`"role": "developer"`), []byte(`"role": "system"`))
	}

	// PROTOKOL UYUMLU TRANSCRIPT: giden isteği (messages + params) zaman
	// damgalı olarak yakala. Yalnızca "messages" taşıyan chat gövdeleri kayda
	// girer; models/embedding gibi gövdeler transkripte değil RequestLog'a yazılır.
	trReq := BuildTranscriptRequest(bodyBytes, "evren", startTime)
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
			resp.DurationMs = time.Since(startTime).Milliseconds()
		}
		globalTranscripts.Record(trReq, resp)
	}

	// OTURUM TESPİTİ (evren_session.go): upstream'in oturum anahtarını
	// gateway'de yeniden üret — explicit id > header > fp > bridge köprüsü.
	bodyID := ""
	fp, sessionPreview := "", ""
	if trReq != nil {
		trReq.ParentSessionID = CanonicalSessionID(HeaderParentSessionHint(r)) // kök kimlik de aynı önek kuralından geçer
		bodyID = trReq.ConversationID
		fp = sessionFingerprintMsgs(trReq.Messages)
		for _, m := range trReq.Messages {
			if m.Role == "user" {
				sessionPreview = previewOf(contentText(m.Content))
				break
			}
		}
	}
	headerID := CanonicalSessionID(HeaderConversationHint(r)) // session-… → dsh-session-… (yeni oturumlarda)
	// ERKEN SOY BAĞI: X-Parent-Session-Id varsa bağ, ANAHTAR ATAMADAN ÖNCE
	// kurulur — subagent'ın İLK isteği de kök oturumun kartını alsın; yetim
	// (kendine bağlı) bağ oluşmasın. Kayıt tarafı (Record) bunu yineler.
	if trReq != nil && trReq.ParentSessionID != "" && headerID != "" && trReq.ParentSessionID != headerID {
		chatSessions.SetParent(headerID, trReq.ParentSessionID)
		if p.km != nil {
			p.km.Unbind(headerID) // önceki ilk-istekten kalmış yetim bağı temizle
		}
	}
	var clientPID int
	clientProc := ""
	if _, portStr, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil {
		if p, convErr := strconv.Atoi(portStr); convErr == nil {
			pid, proc := resolveClientInfo(p)
			clientPID = int(pid)
			clientProc = proc
		}
	}
	clientHeaders := captureSessionHeaders(r.Header)

	sess := SessionInfo{Tier: tierNone, FP: fp, Preview: sessionPreview}
	if trReq != nil {
		sess = chatSessions.Resolve(bodyID, headerID, fp, trReq.Messages, clientPID)
		sess.FP = fp
		sess.Preview = sessionPreview
		if sess.ID != "" {
			// Transkript grouping de AYNI anahtarı kullanır: oturum
			// asla bölünmez (compaction'da bile).
			trReq.ConversationID = sess.ID
		}
	} else if headerID != "" {
		sess = SessionInfo{ID: headerID, Tier: tierHeader, FP: fp, Preview: sessionPreview}
	}

	// stampSession, bir istek kaydına oturum alanlarını yazar.
	stampSession := func(e *RequestLog) {
		e.SessionID = sess.ID
		e.SessionTier = sess.Tier
		e.SessionFP = sess.FP
		e.SessionPreview = sess.Preview
		e.ClientPID = clientPID
		e.ClientProc = clientProc
		e.ClientHeaders = clientHeaders
	}

	var reqBodyMap map[string]interface{}
	isStream := false
	modelName := "unknown"
	isEmbedding := strings.Contains(r.URL.Path, "embeddings")
	// SES (ASR): /v1/audio/transcriptions ve /audio/translations uçları
	// multipart/form-data taşır; JSON ayrıştırması ATLANIR ve gövde ham geçer.
	// Sohbet gövdesi (messages) olmadığı için transcript/oturum atama mantığı
	// bu isteklerde HİÇ devreye girmez: evren proxy'si aktif kartın anahtarını
	// kullanır — gömülü (embedding) yolların birebir aynısı.
	isAudio := isAudioPath(r.URL.Path)
	// KARAR (DECISION): POST /v1/systemone — clef (görsel destekli) ve laya.
	// Gövde JSON'dur ama sohbet DEĞİLDİR: "messages" taşımaz, bu yüzden
	// transcript/oturum atama mantığı bu isteklerde HİÇ devreye girmez.
	// Bu modeller metin üretmez; state + sorular (choice/score/noul) tek geçişte
	// olasılıklarıyla yanıtlanır (usage.output_tokens = 0). Embedding/ses gibi
	// ayrı sayaçlara işlenir, chat net formülüne ve rotasyona girmez.
	isDecision := isDecisionPath(r.URL.Path)
	inputPreview := ""

	// Multipart ses gövdesinden model adı + dosya adı (yalnızca günlük/önizleme;
	// gövde bayt bayt korunur, upstream'e dokunulmaz).
	if isAudio {
		if m, fn := audioFormMeta(r.Header.Get("Content-Type"), bodyBytes); m != "" || fn != "" {
			if m != "" {
				modelName = m
			}
			if fn != "" {
				inputPreview = fn
			}
		}
	}

	if len(bodyBytes) > 0 && (r.Header.Get("Content-Type") == "application/json" || strings.Contains(r.Header.Get("Content-Type"), "json")) {
		if err := json.Unmarshal(bodyBytes, &reqBodyMap); err == nil {
			if s, ok := reqBodyMap["stream"].(bool); ok {
				isStream = s
			}
			if m, ok := reqBodyMap["model"].(string); ok {
				modelName = m
			}

			// Embedding girdi önizlemesi al
			if isEmbedding {
				if inp, ok := reqBodyMap["input"].(string); ok {
					if len(inp) > 70 {
						inputPreview = inp[:67] + "..."
					} else {
						inputPreview = inp
					}
				} else if inps, ok := reqBodyMap["input"].([]interface{}); ok {
					inputPreview = fmt.Sprintf("[%d metin parçası]", len(inps))
				}
			}

			// Streaming chat isteklerinde otomatik stream_options.include_usage ekle
			if isStream && cfg.AutoInjectStreamUsage {
				streamOpts, ok := reqBodyMap["stream_options"].(map[string]interface{})
				if !ok {
					streamOpts = make(map[string]interface{})
				}
				streamOpts["include_usage"] = true
				reqBodyMap["stream_options"] = streamOpts

				if updatedBytes, err := json.Marshal(reqBodyMap); err == nil {
					bodyBytes = updatedBytes
				}
			}
		}
	}

	// 2. Oturum anahtarını al — İSTEK BAZLI DİNAMİK DAĞITIM: kök oturum
	// (subagent'lar Root() ile köke bağlanır, tek oturum=tek anahtar) havuzdan
	// kendi anahtarını alır; kotası biten anahtarın oturumu HEMEN havuzdan
	// yenisine taşınır. root=="" yolları (models/gömülü) aktif anahtarı alır.
	activeKey, err := p.km.KeyForSession(chatSessions.Root(sess.ID))
	if err != nil {
		log.Printf("[HATA] Aktif API anahtarı alınamadı: %v", err)
		recordTranscript(TranscriptResponse{StatusCode: http.StatusTooManyRequests, Error: "aktif anahtar alınamadı: " + err.Error()})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"%s","type":"quota_exceeded","code":"daily_limit_exhausted"}}`, err.Error())))
		return
	}
	if trReq != nil {
		trReq.APIKeyID = activeKey.ID
		trReq.APIKeyName = activeKey.Name
	}

	// 3. Upstream URL'i oluştur
	upstreamURL, err := p.buildUpstreamURL(cfg.UpstreamBase, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		http.Error(w, `{"error":{"message":"Geçersiz upstream URL","type":"proxy_error"}}`, http.StatusInternalServerError)
		return
	}

	// 4. İsteği yürüt (gerektiğinde şartlar kabulü). GEÇİŞ TETİKLEYİCİLERİ:
	//  1) geçiş eşiği (marj) aşımı — DeductUsage,
	//  2) günlük kota 429 — code=daily_token_limit_exceeded (1 kez yineleme).
	// Dakikalık rate_limit 429 dahil diğer hiçbir olay geçiş yapmaz.
	resp, usedKey, err := p.executeUpstreamRequest(r, upstreamURL, bodyBytes, activeKey)
	if err != nil {
		log.Printf("[HATA] Upstream isteği başarısız: %v", err)
		if usedKey != nil {
			p.km.RecordError(usedKey.ID)
		}
		recordTranscript(TranscriptResponse{StatusCode: http.StatusBadGateway, Error: "upstream bağlantı hatası: " + err.Error()})
		// Bağlantı hataları da OTURUMU ile loglanır (429/kesinti korelasyonu).
		failEntry := RequestLog{
			ID:         fmt.Sprintf("req-%d", time.Now().UnixNano()),
			Model:      modelName,
			Method:     r.Method,
			Path:       r.URL.Path,
			StatusCode: http.StatusBadGateway,
			DurationMs: time.Since(startTime).Milliseconds(),
			Stream:     isStream,
			ErrorMessage: err.Error(),
		}
		if usedKey != nil {
			failEntry.KeyID = usedKey.ID
			failEntry.KeyName = usedKey.Name
		}
		stampSession(&failEntry)
		globalLogger.Log(failEntry)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"Upstream EVREN sunucusuna bağlanılamadı: %s","type":"upstream_error"}}`, err.Error())))
		return
	}
	if trReq != nil && usedKey != nil {
		// 429 sonrası anahtar dönmüş olabilir: kayda gerçekten kullanılan anahtarı yaz.
		trReq.APIKeyID = usedKey.ID
		trReq.APIKeyName = usedKey.Name
	}
	defer resp.Body.Close()

	// 4b. Upstream HTTP 200 DIŞINDA bir kod döndü (400/401/403/429/5xx...):
	// ham gövde hem konsola [UPSTREAM HATA], hem RequestLog.error_message'a
	// birebir yazılır; istemciye AYNI GÖVDE ve AYNI KOD iletilir — hiçbir
	// hata mesajı yutulmaz.
	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			log.Printf("[UPSTREAM HATA] %s %s -> HTTP %d | gövde okunamadı: %v", r.Method, r.URL.Path, resp.StatusCode, readErr)
		}
		log.Printf("[UPSTREAM HATA] %s %s -> HTTP %d | gövde: %s", r.Method, r.URL.Path, resp.StatusCode, strings.TrimSpace(string(respBody)))

		upstreamErrMsg := fmt.Sprintf("HTTP %d | %s", resp.StatusCode, strings.TrimSpace(string(respBody)))

		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)

		upstreamErrEntry := RequestLog{
			ID:           fmt.Sprintf("req-%d", time.Now().UnixNano()),
			Timestamp:    time.Now().Format("2006-01-02 15:04:05"),
			KeyID:        usedKey.ID,
			KeyName:      usedKey.Name,
			Model:        modelName,
			Method:       r.Method,
			Path:         r.URL.Path,
			StatusCode:   resp.StatusCode,
			DurationMs:   time.Since(startTime).Milliseconds(),
			Stream:       isStream,
			ErrorMessage: upstreamErrMsg,
			IsEmbedding:  isEmbedding,
			IsAudio:      isAudio,
			InputPreview: inputPreview,
			RequestBody:  clipDetailBody(string(bodyBytes)),
			ResponseBody: clipDetailBody(string(respBody)),
		}
		stampSession(&upstreamErrEntry)
		globalLogger.Log(upstreamErrEntry)
		recordTranscript(TranscriptResponse{
			StatusCode: resp.StatusCode,
			Error:      upstreamErrMsg,
			Raw:        rawJSONOrString(respBody),
		})
		return
	}

	// 5. Yanıtı istemciye aktar ve Token kullanımını yakala
	var capturedUsage UsageResult
	hasUsage := false
	var respMsg *TranscriptMessage
	var respFinish string
	var streamErr string
	var rawBody []byte
	var streamContent string // SSE'de derlenen asistan metni (detay gövdesi için)

	if isStream && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		out := p.handleSSEStream(w, resp)
		capturedUsage, hasUsage = out.Usage, out.HasUsage
		streamErr = out.Err
		respFinish = out.Finish
		streamContent = out.Content
		if out.Content != "" {
			contentJSON, _ := json.Marshal(out.Content)
			respMsg = &TranscriptMessage{Role: "assistant", Content: contentJSON, Timestamp: tsNow()}
		}
	} else {
		capturedUsage, hasUsage, rawBody = p.handleStandardResponse(w, resp)
		msg, finish := extractAssistantMessage(rawBody, time.Now())
		respMsg, respFinish = msg, finish
	}

	durationMs := time.Since(startTime).Milliseconds()
	reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())

	// 6a. SES (ASR) İSTEĞİ İSE: embedding ile AYNI mantık — ayrı ses sayaçlarına
	// işle, chat net formülüne ve anahtar rotasyonuna GİRME. Upstream ses süresi
	// bildirdiyse (duration/audio_seconds) log ve sayaçlara yazılır; token
	// bildirirse ham olarak günlük/toplam kotaya düşülür (embedding ile tutarlı).
	if isAudio {
		tokens := capturedUsage.PromptTokens
		if tokens == 0 {
			tokens = capturedUsage.TotalTokens
		}
		// Sürenin kaynağı sırayla: upstream yanıtı → istemci başlığı
		// (X-Audio-Seconds; test sayfası sesin GERÇEK süresini bilir) → WAV
		// başlığından yerel hesap. Upstream ASR yanıtı token/kullanım
		// raporlamıyor (istek bazlı sayıyor), bu yüzden süre yerelde ölçülür.
		seconds := extractAudioSeconds(rawBody)
		if seconds == 0 {
			seconds = clientAudioSeconds
		}
		if seconds == 0 {
			seconds = wavSecondsFromMultipart(bodyBytes)
		}
		p.km.RecordAudioUsage(usedKey.ID, tokens, seconds)

		audioEntry := RequestLog{
			ID:           reqID,
			Timestamp:    time.Now().Format("2006-01-02 15:04:05"),
			KeyID:        usedKey.ID,
			KeyName:      usedKey.Name,
			Model:        modelName,
			Method:       r.Method,
			Path:         r.URL.Path,
			StatusCode:   resp.StatusCode,
			DurationMs:   durationMs,
			Stream:       false,
			Usage:        capturedUsage,
			IsAudio:      true,
			AudioSeconds: seconds,
			SourceFile:   sourceFile,
			InputPreview: inputPreview,
			RequestBody:  clipDetailBody(string(bodyBytes)),
			ResponseBody: clipDetailBody(string(rawBody)),
		}
		stampSession(&audioEntry)
		globalLogger.Log(audioEntry)

		log.Printf("[SES/ASR PROXY] [%s] Model: %s | Dosya: %s | Token: %d | Süre: %d sn | %d ms (chat formülü dışı)",
			usedKey.Name, modelName, inputPreview, tokens, seconds, durationMs)
		return
	}

	// 6b. KARAR (DECISION) İSTEĞİ İSE: /v1/systemone — clef (görsel destekli) ve
	// laya. Embedding/ses ile AYNI mantık: ayrı sayaç, ayrı günlük akışı, chat
	// net formülü ve anahtar rotasyonu DIŞI. Model metin üretmediği için
	// (output_tokens = 0) yalnızca girdi token'ı sayılır; görseller karo karo
	// token üretir (32×32 px = 1 token).
	if isDecision {
		tokens := capturedUsage.PromptTokens
		if tokens == 0 && capturedUsage.TotalTokens > 0 {
			tokens = capturedUsage.TotalTokens
		}
		p.km.RecordDecisionUsage(usedKey.ID, tokens)

		decEntry := RequestLog{
			ID:           reqID,
			Timestamp:    time.Now().Format("2006-01-02 15:04:05"),
			KeyID:        usedKey.ID,
			KeyName:      usedKey.Name,
			Model:        modelName,
			Method:       r.Method,
			Path:         r.URL.Path,
			StatusCode:   resp.StatusCode,
			DurationMs:   durationMs,
			Stream:       false,
			Usage:        capturedUsage,
			IsDecision:   true,
			SourceFile:   sourceFile,
			InputPreview: decisionSummary(bodyBytes),
			RequestBody:  clipDetailBody(string(bodyBytes)),
			ResponseBody: clipDetailBody(string(rawBody)),
		}
		stampSession(&decEntry)
		globalLogger.Log(decEntry)

		log.Printf("[KARAR/SYSTEMONE PROXY] [%s] Model: %s | %s | Girdi Token: %d | %d ms (chat formülü dışı)",
			usedKey.Name, modelName, decEntry.InputPreview, tokens, durationMs)
		return
	}

	// 6. EMBEDDING İSTEĞİ İSE: Ayrı embedding sayacına işle, 10M chat kotasından DÜŞME
	if isEmbedding {
		tokens := capturedUsage.PromptTokens
		if tokens == 0 && capturedUsage.TotalTokens > 0 {
			tokens = capturedUsage.TotalTokens
		}
		p.km.RecordEmbeddingUsage(usedKey.ID, tokens)

		embedEntry := RequestLog{
			ID:           reqID,
			Timestamp:    time.Now().Format("2006-01-02 15:04:05"),
			KeyID:        usedKey.ID,
			KeyName:      usedKey.Name,
			Model:        modelName,
			Method:       r.Method,
			Path:         r.URL.Path,
			StatusCode:   resp.StatusCode,
			DurationMs:   durationMs,
			Stream:       false,
			Usage:        capturedUsage,
			IsEmbedding:  true,
			SourceFile:   sourceFile,
			InputPreview: inputPreview,
			RequestBody:  clipDetailBody(string(bodyBytes)),
			ResponseBody: clipDetailBody(string(rawBody)),
		}
		stampSession(&embedEntry)
		globalLogger.Log(embedEntry)

		log.Printf("[EMBEDDING PROXY] [%s] Model: %s | Harcanan Token: %d | Süre: %d ms (10M kotadan bağımsız)",
			usedKey.Name, modelName, tokens, durationMs)
		return
	}

	// 7. CHAT İSTEĞİ İSE: 10M kotadan düş ve gerektiğinde anahtar rotasyonu yap
	rotated := false
	var newActiveKey *APIKey
	if hasUsage {
		newKey, wasRotated, err := p.km.DeductUsage(usedKey.ID, capturedUsage, cfg.CalculationMode)
		if err != nil {
			log.Printf("[DEDUCT HATA] Token düşümü kaydedilemedi: %v", err)
		}
		rotated = wasRotated
		newActiveKey = newKey

		log.Printf("[USAGE] [%s] Model: %s | Prompt: %d (Cached: %d, %%%.1f) | Output: %d | Net Düşen: %d | Günlük: %d / %d",
			usedKey.Name, modelName, capturedUsage.PromptTokens, capturedUsage.CachedTokens,
			capturedUsage.CacheHitRatio, capturedUsage.CompletionTokens, capturedUsage.NetDeduction,
			usedKey.UsedDaily, usedKey.DailyLimit)
	} else if r.URL.Path != "/v1/models" && !strings.Contains(r.URL.Path, "models") {
		log.Printf("[USAGE BİLGİSİ YOK] [%s] İstek tamamlandı ancak usage nesnesi dönmedi (%d ms)", usedKey.Name, durationMs)
	}

	newKeyID := ""
	if newActiveKey != nil {
		newKeyID = newActiveKey.ID
	}

	// Detay gövdesi: standart yanıtta ham JSON, SSE'de derlenen içerik + finish.
	respBodyForLog := string(rawBody)
	if isStream {
		payload := map[string]any{"content": streamContent, "finish_reason": respFinish}
		if hasUsage {
			payload["usage"] = capturedUsage
		}
		if b, marshalErr := json.Marshal(payload); marshalErr == nil {
			respBodyForLog = string(b)
		}
	}

	chatEntry := RequestLog{
		ID:           reqID,
		Timestamp:    time.Now().Format("2006-01-02 15:04:05"),
		KeyID:        usedKey.ID,
		KeyName:      usedKey.Name,
		Model:        modelName,
		Method:       r.Method,
		Path:         r.URL.Path,
		StatusCode:   resp.StatusCode,
		DurationMs:   durationMs,
		Stream:       isStream,
		Usage:        capturedUsage,
		Rotated:      rotated,
		NewKeyID:     newKeyID,
		IsEmbedding:  false,
		ErrorMessage: streamErr, // SSE kopması/upstream stream hatası varsa birebir
		RequestBody:  clipDetailBody(string(bodyBytes)),
		ResponseBody: clipDetailBody(respBodyForLog),
	}
	stampSession(&chatEntry)
	globalLogger.Log(chatEntry)

	// TRANSCRIPT: istek + dönen yanıt (asistan mesajı, usage, hata) protokol
	// şemasına uygun, zaman damgalı olarak bellekteki transkripte yazılır.
	trResp := TranscriptResponse{
		StatusCode:   resp.StatusCode,
		Message:      respMsg,
		FinishReason: respFinish,
		Stream:       isStream,
		Error:        streamErr,
		DurationMs:   durationMs,
	}
	if hasUsage {
		u := capturedUsage
		trResp.Usage = &u
	}
	if !isStream && len(rawBody) > 0 {
		trResp.Raw = rawJSONOrString(rawBody)
	}
	recordTranscript(trResp)
}

func (p *ProxyHandler) buildUpstreamURL(base, requestPath, query string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}

	cleanPath := strings.TrimPrefix(requestPath, "/")
	if strings.HasSuffix(u.Path, "/v1") && strings.HasPrefix(cleanPath, "v1/") {
		cleanPath = strings.TrimPrefix(cleanPath, "v1/")
	}

	if u.Path == "" || u.Path == "/" {
		u.Path = "/" + cleanPath
	} else {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + cleanPath
	}

	if query != "" {
		u.RawQuery = query
	}

	return u.String(), nil
}

func (p *ProxyHandler) executeUpstreamRequest(r *http.Request, upstreamURL string, bodyBytes []byte, key *APIKey) (*http.Response, *APIKey, error) {
	currentKey := key
	dailyRetried := false

	for {
		req, err := http.NewRequest(r.Method, upstreamURL, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, currentKey, err
		}

		for k, v := range r.Header {
			req.Header[k] = v
		}
		// Accept-Encoding UPSTREAM'E İLETİLMEZ. Tarayıcı ve Python gibi istemciler
		// "gzip, deflate, br" gönderiyor; upstream gövdeyi sıkıştırınca Go'nun
		// http taşıması şeffaf çözme YAPMAZ (başlığı açıkça biz yazdığımız için
		// kendi gzip'ini eklemez), dolayısıyla gövde ikili kalıyordu: kullanım/
		// token ayrıştırması sessizce başarısız oluyor, günlüğe 0 token yazılıyor
		// ve KOTA DÜŞÜMÜ yapılmıyordu (ölçüm: aynı istek gzip'siz 16 token,
		// "Accept-Encoding: gzip" ile 0). SSE akışı da sıkıştırılmış gelirse
		// satır okuyucu bozulur. Başlığı silince Go kendi gzip'ini ekleyip
		// şeffafça çözer; istemciye de düz (ve doğru) gövde gider.
		req.Header.Del("Accept-Encoding")
		req.Host = ""
		req.Header.Set("x-api-key", currentKey.Key)
		req.Header.Set("Authorization", "Bearer "+currentKey.Key)

		resp, err := p.httpClient.Do(req)
		if err != nil {
			return nil, currentKey, err
		}
		// Gövdeyi tüketen her yol (akış pompası, 403/429 ham okumaları, akış-
		// dışı iletim) aynı sessizlik korumasından geçsin.
		resp.Body = newIdleBody(resp.Body, p.streamIdle)

		// Durum: EVREN Kullanım şartları onaylanmamış (403 terms_not_accepted)
		if resp.StatusCode == http.StatusForbidden {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if strings.Contains(string(respBody), "terms_not_accepted") {
				log.Printf("[ŞARTLAR ONAYI] Anahtar %s için EVREN kullanım şartları (v1) otomatik onaylanıyor...", currentKey.Name)
				if p.autoAcceptTerms(currentKey.Key) {
					log.Printf("[ŞARTLAR ONAYLANDI] Anahtar %s şartları başarıyla kabul edildi! İstek yineleniyor.", currentKey.Name)
					continue
				}
			}

			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			return resp, currentKey, nil
		}

		// 429'DA İKİ FARKLI LIMIT TÜRÜ VARDIR:
		//  - dakikalık rate_limit  → geçiş YOK, gövde birebir iletilir,
		//  - GÜNLÜK kota (code=daily_token_limit_exceeded) → geçiş tetikleyicisi:
		//    sıradaki havuz kartına geçilir, istek 1 kez yeni kartla yinelenir.
		if resp.StatusCode == http.StatusTooManyRequests {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			isDaily := bytes.Contains(respBody, []byte("daily_token_limit_exceeded"))
			if isDaily && !dailyRetried {
				if nextKey, ok := p.km.RotateAfterDailyLimit(currentKey.ID); ok && nextKey != nil {
					log.Printf("[UPSTREAM GÜNLÜK LİMİT] %s HTTP 429 (daily_token_limit_exceeded): → '%s' kartına geçildi, istek yineleniyor.", currentKey.Name, nextKey.Name)
					currentKey = nextKey
					dailyRetried = true
					continue
				}
			}

			// Geçiş yoksa (oto geçiş kapalı / aday yok / dakikalık limit / zaten
			// yinelendi): gövde geri konur ve 429 istemciye BİREBİR iletilir.
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			return resp, currentKey, nil
		}

		return resp, currentKey, nil
	}
}

func (p *ProxyHandler) autoAcceptTerms(apiKey string) bool {
	cfg := GetConfig()
	acceptURL := strings.TrimRight(cfg.UpstreamBase, "/") + "/terms/accept"

	body := []byte(`{"version": 1}`)
	req, err := http.NewRequest("POST", acceptURL, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// streamOutcome, SSE akışının istemciye relay edilirken transkript için
// derlediği sonuçtur.
type streamOutcome struct {
	Usage    UsageResult
	HasUsage bool
	Content  string // birleştirilmiş asistan metni
	Finish   string // en son finish_reason
	Err      string // [STREAM HATA] mesajı (kopsa/upstream hatası varsa)
}

func (p *ProxyHandler) handleStandardResponse(w http.ResponseWriter, resp *http.Response) (UsageResult, bool, []byte) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Yanıt okunamadı", http.StatusInternalServerError)
		return UsageResult{}, false, nil
	}

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)

	usage, ok := ParseUsageFromBytes(respBody)
	return usage, ok, respBody
}

// handleSSEStream SSE akışını istemciye aktarırken ayrıca:
//   - upstream "error" nesnelerini tam içeriğiyle [STREAM HATA] olarak loglar,
//   - scanner.Err() ile timeout / ağ kopması / buffer taşmasını yakalar,
//   - akış [DONE] ve finish_reason GÖNMEDEN kopsa bile soketi pat diye
//     kapatmaz: kontrollü hata chunk'ı veya tamamlama chunk'ı + [DONE] gönderir
//     (istemci "stream closed before a finish_reason" ile çökmez).
func (p *ProxyHandler) handleSSEStream(w http.ResponseWriter, resp *http.Response) streamOutcome {
	out := streamOutcome{}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	flusher, isFlusher := w.(http.Flusher)
	if isFlusher {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxSSELineBytes)

	var content strings.Builder
	sawDone := false
	sawFinish := false
	sawUpstreamError := false
	streamErr := ""

	// finishReasonChunk, finish_reason gelmeden kapanan akışa kontrollü
	// tamamlama chunk'ı yazar.
	finishReasonChunk := func() {
		_, _ = fmt.Fprint(w, `data: {"id":"stream-cutoff","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"stop"}]}`+"\n\n")
	}

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			_, _ = fmt.Fprintf(w, "%s\n", line)
			if isFlusher {
				flusher.Flush()
			}
			continue
		}

		dataPayload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))

		if dataPayload == "[DONE]" {
			sawDone = true
			// Normal durumda finish_reason [DONE]'dan önce gelir: satırı
			// aynen ilet. Gelmediyse istemciyi çökertmeden kontrollü kapat.
			switch {
			case sawFinish || sawUpstreamError || streamErr != "":
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			default:
				finishReasonChunk()
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			}
			if isFlusher {
				flusher.Flush()
			}
			continue
		}

		// Upstream SSE error nesnesi: tam içeriğiyle logla, hataya ekle.
		var errProbe struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(dataPayload), &errProbe) == nil && len(errProbe.Error) > 0 && string(errProbe.Error) != "null" {
			sawUpstreamError = true
			streamErr = fmt.Sprintf("HTTP %d upstream SSE hatası: %s", resp.StatusCode, dataPayload)
			log.Printf("[STREAM HATA] %s", streamErr)
		}

		if strings.Contains(dataPayload, `"usage"`) {
			if usage, ok := ParseUsageFromBytes([]byte(dataPayload)); ok {
				out.Usage = usage
				out.HasUsage = true
			}
		}

		// Asistan metni ve finish_reason derlemesi (transkript için).
		var event chunk
		if json.Unmarshal([]byte(dataPayload), &event) == nil {
			for _, choice := range event.Choices {
				if choice.Delta.Content != "" {
					content.WriteString(choice.Delta.Content)
				}
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					out.Finish = *choice.FinishReason
					sawFinish = true
				}
			}
		}

		_, _ = fmt.Fprintf(w, "%s\n", line)
		if isFlusher {
			flusher.Flush()
		}
	}

	// Kopma denetimi: zaman aşımı / ağ kopması / buffer taşması.
	if err := scanner.Err(); err != nil {
		streamErr = fmt.Sprintf("SSE akışı yarıda kesildi (timeout/ağ/buffer): %v", err)
		log.Printf("[STREAM HATA] %v", err)
	}
	if !sawDone && !sawFinish && streamErr == "" {
		streamErr = "Upstream akışı [DONE] ve finish_reason gelmeden koptu"
		log.Printf("[STREAM HATA] %s", streamErr)
	}

	// KONTROLLÜ SONLANDIRMA (yalnızca [DONE] hiç gelmediyse):
	if !sawDone {
		switch {
		case streamErr != "" && !sawUpstreamError:
			// Açık hata chunk'ı: SDK'larda "stream closed before a
			// finish_reason" çökmesi yerine net kontrollü hata oluşur.
			if b, err := json.Marshal(map[string]any{"error": map[string]any{
				"message": streamErr,
				"type":    "stream_interrupted",
				"code":    "stream_interrupted",
			}}); err == nil {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		case streamErr != "":
			// Upstream error nesnesi zaten iletildi: yalnızca temiz kapanış.
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		case !sawFinish:
			finishReasonChunk()
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}
	if isFlusher {
		flusher.Flush()
	}

	out.Content = content.String()
	out.Err = streamErr
	return out
}

// isAudioPath, ses (konuşma tanıma / çeviri) uç noktalarını tanır:
// /v1/audio/transcriptions, /evren/v1/audio/transcriptions, /audio/translations.
// Yol karşılaştırması önekten bağımsızdır; böylece hem 8088 (doğrudan /v1)
// hem 8110 (/v1 ve /evren/v1) aynı davranışı alır.
func isAudioPath(p string) bool {
	return strings.Contains(p, "audio/transcriptions") || strings.Contains(p, "audio/translations")
}

// audioFormMeta, multipart/form-data gövdesinden model adı ve yüklenen dosyanın
// adını çıkarır — YALNIZCA günlük/önizleme içindir, gövde değiştirilmez ve
// upstream'e giden baytlar etkilenmez. Sınır (boundary) yoksa, gövde JSON ise
// veya gövde tarama tavanını aşıyorsa boş döner: hata isteği ASLA bozmaz.
// @param contentType - isteğin Content-Type başlığı (boundary buradan okunur).
// @param body - ham istek gövdesi (multipart).
// @returns model alanı, dosya adı (file alanı); bulunamayanlar boş string.
func audioFormMeta(contentType string, body []byte) (model, filename string) {
	const maxScan = 8 << 20 // 8 MiB: form alanları gövdenin başında gelir
	if len(body) == 0 || !strings.Contains(contentType, "multipart/form-data") {
		return "", ""
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", ""
	}
	scan := body
	if len(scan) > maxScan {
		scan = scan[:maxScan]
	}
	mr := multipart.NewReader(bytes.NewReader(scan), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return model, filename
		}
		name := part.FormName()
		if name != "model" && name != "file" {
			_ = part.Close()
			continue
		}
		val, _ := io.ReadAll(io.LimitReader(part, 4<<10))
		if name == "model" {
			model = strings.TrimSpace(string(val))
		} else {
			filename = part.FileName()
			if filename == "" {
				filename = "ses akışı"
			}
		}
		_ = part.Close()
		if model != "" && filename != "" {
			return model, filename
		}
	}
}

// extractAudioSeconds, ses yanıtındaki süre alanını toleranslı arar: kök
// düzeyde duration/audio_seconds/seconds, evren nesnesi içinde
// audio_seconds/duration/duration_seconds. Sayı ya da sayısal metin kabul eder.
// Alan yoksa 0 döner (sayaç yalnızca isteği sayar).
// @param raw - upstream yanıt gövdesi (JSON).
// @returns saniye cinsinden ses süresi; bilinmiyorsa 0.
func extractAudioSeconds(raw []byte) int64 {
	if len(raw) == 0 {
		return 0
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return 0
	}
	pick := func(v any) (int64, bool) {
		switch t := v.(type) {
		case float64:
			return int64(t), true
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return int64(f), true
			}
		}
		return 0, false
	}
	for _, k := range []string{"duration", "audio_seconds", "seconds"} {
		if v, ok := m[k]; ok {
			if n, ok2 := pick(v); ok2 {
				return n
			}
		}
	}
	if ev, ok := m["evren"].(map[string]any); ok {
		for _, k := range []string{"audio_seconds", "duration", "duration_seconds"} {
			if v, ok := ev[k]; ok {
				if n, ok2 := pick(v); ok2 {
					return n
				}
			}
		}
	}
	return 0
}

// isDecisionPath, KARAR (decision) uçlarını tanır: POST /v1/systemone
// (clef = görsel destekli, laya = metin). Batch ucu (/v1/systemone/batch)
// upstream'de kapalıdır; yine de aynı yola düşer ve hata olduğu gibi döner.
// @param p - istek yolu.
// @returns karar ucu ise true.
func isDecisionPath(p string) bool {
	return strings.Contains(p, "systemone")
}

// decisionSummary, karar isteğinin gövdesinden panelde gösterilecek kısa bir
// özet üretir: kaç soru, hangi tipler (choice/score/noul) ve kaç görsel.
// Gövde DEĞİŞMEZ, yalnızca okunur. Ayrıştırılamazsa boş metin döner.
// @param body - ham JSON istek gövdesi.
// @returns "3 soru (choice, noul, score) • 1 görsel" gibi bir özet.
func decisionSummary(body []byte) string {
	var req struct {
		Questions map[string]struct {
			Type string `json:"type"`
		} `json:"questions"`
		Images []json.RawMessage `json:"images"`
	}
	if len(body) == 0 || json.Unmarshal(body, &req) != nil {
		return ""
	}
	types := make([]string, 0, 4)
	for _, q := range req.Questions {
		if q.Type != "" {
			types = append(types, q.Type)
		}
	}
	sort.Strings(types)
	parts := []string{fmt.Sprintf("%d soru", len(req.Questions))}
	if len(types) > 0 {
		parts = append(parts, "("+strings.Join(types, ", ")+")")
	}
	if n := len(req.Images); n > 0 {
		parts = append(parts, fmt.Sprintf("• %d görsel", n))
	}
	return strings.Join(parts, " ")
}

// isLocalOrigin, CORS başlığı verilebilecek YEREL kaynakları tanır: file://
// ile açılan sayfalar tarayıcıda "null" origin gönderir; yerel test araçları
// ise 127.0.0.1/localhost kullanır. Uzak adresler (https://rastgele-site)
// kasıtlı olarak reddedilir — böylece tarayıcı üzerinden gateway'in
// anahtarları ve kotası kullanılamaz.
// @param origin - isteğin Origin başlığı.
// @returns yerel kaynak ise true.
func isLocalOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	if origin == "null" { // file:// sayfaları ve sandbox'lı iframe'ler
		return true
	}
	for _, p := range []string{
		"http://127.0.0.1", "https://127.0.0.1",
		"http://localhost", "https://localhost",
		"http://[::1]", "https://[::1]",
	} {
		if origin != p && !strings.HasPrefix(origin, p+":") {
			continue
		}
		// "http://localhost.saldirgan.com" gibi alan adlarını ele: önek eşleşse
		// bile port/bitiş gelmeli.
		rest := origin[len(p):]
		if rest == "" || (len(rest) > 1 && rest[0] == ':' && isAllDigits(rest[1:])) {
			return true
		}
	}
	return false
}

// isAllDigits, s dizisinin tamamı rakam mı diye bakar (port doğrulaması).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseSecondsHeader, istemcinin bildirdiği ses süresini okur
// (X-Audio-Seconds). Sürükle-bırak test sayfası, sesin gerçek süresini
// tarayıcıdan bilir ve bu başlıkla gönderir; böylece upstream süre
// bildirmese de "ses saniyesi" sayacı doğru işler. Geçersiz/0 ise 0 döner.
// @param v - başlık değeri (saniye, tam sayı).
// @returns saniye; okunamazsa 0.
func parseSecondsHeader(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 || n > 24*3600 {
		return 0
	}
	return int64(n + 0.5)
}

// wavSecondsFromMultipart, multipart gövde içindeki WAV (RIFF) verisinden ses
// süresini hesaplar: data chunk boyutu / byte rate. Upstream süre bildirmediği
// ve istemci başlık göndermediği durumda (ör. curl ile test) yerel ve dürüst
// bir ölçüm sağlar. WAV dışı biçimlerde 0 döner — uydurma yapılmaz.
// @param body - ham multipart istek gövdesi.
// @returns saniye cinsinden süre; hesaplanamazsa 0.
func wavSecondsFromMultipart(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	scan := body
	if len(scan) > 8<<20 { // başlık her zaman başta: 8 MB yeter
		scan = scan[:8<<20]
	}
	i := bytes.Index(scan, []byte("RIFF"))
	if i < 0 || i+12 > len(scan) || string(scan[i+8:i+12]) != "WAVE" {
		return 0
	}
	byteRate := 0
	if j := bytes.Index(scan[i:], []byte("fmt ")); j >= 0 {
		if p := i + j + 16; p+4 <= len(scan) { // fmt kimliği(4)+boyut(4)+ilk 8 bayt
			byteRate = int(binary.LittleEndian.Uint32(scan[p : p+4]))
		}
	}
	if byteRate <= 0 {
		return 0
	}
	j := bytes.Index(scan[i:], []byte("data"))
	if j < 0 {
		return 0
	}
	p := i + j + 4
	if p+4 > len(scan) {
		return 0
	}
	size := int(binary.LittleEndian.Uint32(scan[p : p+4]))
	if size <= 0 {
		return 0
	}
	secs := int64(size / byteRate)
	if secs <= 0 || secs > 24*3600 { // makul olmayan değeri yazma
		return 0
	}
	return secs
}
