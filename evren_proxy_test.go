package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withFakeEvren bir sahte Evren upstream'i kurar ve global yapılandırmayı
// test sonunda geri alır.
func withFakeEvren(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	ts := httptest.NewServer(handler)

	evrenCfgLock.Lock()
	old := globalEvrenCfg
	cfg := globalEvrenCfg
	cfg.UpstreamBase = ts.URL + "/v1"
	globalEvrenCfg = cfg
	evrenCfgLock.Unlock()

	t.Cleanup(func() {
		evrenCfgLock.Lock()
		globalEvrenCfg = old
		evrenCfgLock.Unlock()
		ts.Close()
	})
}

// newTestProxy temiz anahtar/gövde üzerinde çalışan bir ProxyHandler kurar.
func newTestProxy(t *testing.T) *ProxyHandler {
	t.Helper()
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "keys.json")
	logFile := filepath.Join(dir, "log.jsonl")

	km, err := NewKeyManager(keysFile)
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	k1, _ := km.AddKey("Test Anahtarı", "test-key-value", 10000000, 50000)
	if err := km.SetActiveKey(k1.ID); err != nil {
		t.Fatalf("SetActiveKey: %v", err)
	}

	InitLogger(logFile, 0)
	old := globalTranscripts
	t.Cleanup(func() { globalTranscripts = old })
	InitTranscripts(maxTranscriptConversations, maxTranscriptExchanges)

	return NewProxyHandler(km)
}

func chatRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"merhaba"}]}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestProxy_UpstreamHataBirebirYakalanir upstream 400 döndüğünde:
// ham gövde istemciye aynen gider, konsol/RequestLog/Transcript hepsi HTTP
// kodu + ham gövdeyle kaydeder.
func TestProxy_UpstreamHataBirebirYakalanir(t *testing.T) {
	withFakeEvren(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"auth"}}`))
	})
	p := newTestProxy(t)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, chatRequest())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("durum kodu birebir iletilmeli: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid api key") {
		t.Fatalf("hata gövdesi yutulmamalı: %s", rec.Body.String())
	}

	logs := globalLogger.GetRecent(1, "")
	if len(logs) != 1 {
		t.Fatalf("RequestLog girdisi bekleniyordu, alındı: %d", len(logs))
	}
	if logs[0].StatusCode != http.StatusBadRequest {
		t.Fatalf("log durum kodu 400 olmalı: %d", logs[0].StatusCode)
	}
	if !strings.Contains(logs[0].ErrorMessage, "HTTP 400") ||
		!strings.Contains(logs[0].ErrorMessage, "invalid api key") {
		t.Fatalf("error_message ham hatayı birebir içermeli: %q", logs[0].ErrorMessage)
	}

	var d TranscriptDump
	if err := json.Unmarshal(globalTranscripts.DumpBytes(), &d); err != nil {
		t.Fatalf("dump çözümlenemedi: %v", err)
	}
	if len(d.Conversations) != 1 || d.Conversations[0].RequestCount != 1 {
		t.Fatalf("transcript kaydı eksik: %+v", d.Stats)
	}
	resp := d.Conversations[0].Exchanges[0].Response
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("transcript durum kodu 400 olmalı: %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Error, "HTTP 400") || !strings.Contains(resp.Error, "invalid api key") {
		t.Fatalf("transcript hatası birebir olmalı: %q", resp.Error)
	}
	if len(resp.Raw) == 0 || !strings.Contains(string(resp.Raw), "invalid api key") {
		t.Fatalf("transcript ham gövde saklamalı: %s", string(resp.Raw))
	}
}

// TestProxy_SSEKopmasindaKontrolluSonlandirma finish_reason ve [DONE] GELMEDEN
// biten akışta istemcinin çökmemesi için kontrollü hata chunk'ı + [DONE]
// gönderildiğini; kopmanın RequestLog/transcript'e yazıldığını kanıtlar.
func TestProxy_SSEKopmasindaKontrolluSonlandirma(t *testing.T) {
	withFakeEvren(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Merhaba\"}}]}\n\n"))
		flusher.Flush()
		// finish_reason ve [DONE] göndermeden bağlantı kapanır (klasik kopma)
	})
	p := newTestProxy(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.6-pro","stream":true,"messages":[{"role":"user","content":"merhaba"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, "Merhaba") {
		t.Fatalf("akıştaki içerik istemciye ulaşmalı: %s", out)
	}
	if !strings.Contains(out, "stream_interrupted") {
		t.Fatalf("kontrollü hata chunk'ı gönderilmeli: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("kontrollü [DONE] gönderilmeli: %s", out)
	}

	logs := globalLogger.GetRecent(1, "")
	if len(logs) != 1 {
		t.Fatalf("RequestLog girdisi bekleniyordu")
	}
	if !strings.Contains(logs[0].ErrorMessage, "koptu") && !strings.Contains(logs[0].ErrorMessage, "kesildi") {
		t.Fatalf("RequestLog.ErrorMessage kopmayı içermeli: %q", logs[0].ErrorMessage)
	}

	var d TranscriptDump
	if err := json.Unmarshal(globalTranscripts.DumpBytes(), &d); err != nil {
		t.Fatalf("dump: %v", err)
	}
	ex := d.Conversations[0].Exchanges[0]
	if ex.Response.Error == "" {
		t.Fatal("transcript yanıt hatası boş kalmamalı")
	}
	if ex.Response.Message == nil || !strings.Contains(string(ex.Response.Message.Content), "Merhaba") {
		t.Fatalf("derlenen asistan metni transkripte yazılmalı: %+v", ex.Response.Message)
	}
}

// TestProxy_SSEHataNesnesiYakalanir akış içindeki upstream "error" nesnesinin
// tam içeriğiyle [STREAM HATA] olarak kaydedildiğini ve istemciye iletildiğini doğrular.
func TestProxy_SSEHataNesnesiYakalanir(t *testing.T) {
	withFakeEvren(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"quota blown\",\"type\":\"rate_limit\"}}\n\n"))
	})
	p := newTestProxy(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.6-pro","stream":true,"messages":[{"role":"user","content":"merhaba"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, "quota blown") {
		t.Fatalf("upstream hata nesnesi istemciye iletilmeli: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("hata sonrası temiz kapanış [DONE] olmalı: %s", out)
	}

	logs := globalLogger.GetRecent(1, "")
	if len(logs) != 1 || !strings.Contains(logs[0].ErrorMessage, "upstream SSE hatası") ||
		!strings.Contains(logs[0].ErrorMessage, "quota blown") {
		t.Fatalf("RequestLog stream hatasını içermeli: %+v", logs)
	}

	var d TranscriptDump
	if err := json.Unmarshal(globalTranscripts.DumpBytes(), &d); err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := d.Conversations[0].Exchanges[0].Response.Error; !strings.Contains(err, "quota blown") {
		t.Fatalf("transcript stream hatası birebir olmalı: %q", err)
	}
}

// TestProxy_SSEFinishVarDoneYok temiz kapanışta (finish var, [DONE] yok)
// istemciye yalnızca kontrollü [DONE] gittiğini ve finish_reason'un
// transkripte işlendiğini doğrular.
func TestProxy_SSEFinishVarDoneYok(t *testing.T) {
	withFakeEvren(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tamam\"},\"finish_reason\":\"stop\"}]}\n\n"))
	})
	p := newTestProxy(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.6-pro","stream":true,"messages":[{"role":"user","content":"merhaba"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] gönderilmeli: %s", out)
	}
	if strings.Contains(out, "stream_interrupted") {
		t.Fatalf("finish_reason gelmişken hata chunk'ı gönderilmemeli: %s", out)
	}

	var d TranscriptDump
	if err := json.Unmarshal(globalTranscripts.DumpBytes(), &d); err != nil {
		t.Fatalf("dump: %v", err)
	}
	ex := d.Conversations[0].Exchanges[0]
	if ex.Response.FinishReason != "stop" {
		t.Fatalf("finish_reason transkripte işlenmeli: %q", ex.Response.FinishReason)
	}
	if ex.Response.Error != "" {
		t.Fatalf("hata olmamalı: %q", ex.Response.Error)
	}
}

// TestProxy_Kota429GecisTetiklemez tek-tetik kuralının regresyonudur: upstream
// 429 döndüğünde yanıt gövdesiyle BİREBİR iletilir (eski kapalı-gövde/boş yanıt
// bug'ı yok) ve aktif anahtar DEĞİŞMEZ — geçiş yalnızca geçiş eşiğiyle olur.
func TestProxy_Kota429GecisTetiklemez(t *testing.T) {
	withFakeEvren(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded","type":"rate_limit"}}`))
	})
	p := newTestProxy(t)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, chatRequest())

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("429 birebir iletilmeli: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "quota exceeded") {
		t.Fatalf("429 gövdesi yutulmamalı/boşaltılmamalı: %s", rec.Body.String())
	}

	active, err := p.km.GetActiveKey()
	if err != nil {
		t.Fatalf("GetActiveKey: %v", err)
	}
	if active.Name != "Test Anahtarı" {
		t.Fatalf("429 aktif anahtarı değiştirmemeli: %s", active.Name)
	}
	for _, k := range p.km.GetAllKeys() {
		if k.IsActive && k.ID != active.ID {
			t.Fatalf("429 sonrası yanlış/çift aktif kart: %s", k.Name)
		}
	}
}

// TestProxy_GunlukLimit429GecisTetikler yalnızca code=daily_token_limit_exceeded
// taşıyan 429'un geçiş TETİKLEDİĞİNİ ve isteğin yeni kartla yinelendiğini kanıtlar:
// istemci nihayetinde 200 + içerik alır, aktif anahtar değişir.
func TestProxy_GunlukLimit429GecisTetikler(t *testing.T) {
	var calls int
	withFakeEvren(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"Günlük token limitiniz aşıldı (tüm API anahtarlarınızın toplamı).","type":"rate_limit_error","param":null,"code":"daily_token_limit_exceeded","evren":{"resets_at":"2026-10-01T00:00:00Z","remaining_tokens":0,"limit_tokens":10000000}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"yeniden calisti"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	p := newTestProxy(t)

	// Yedek kart ekle (otomatik olarak havuza katılır) ve ilk kartı aktif yap.
	k2, _ := p.km.AddKey("Yedek Kart", "yedek-deger", 10000000, 50000)
	all := p.km.GetAllKeys()
	var first *APIKey
	for _, k := range all {
		if k.IsActive {
			first = k
			break
		}
	}
	if first == nil {
		t.Fatal("aktif kart bekleniyordu")
	}
	_ = k2

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, chatRequest())

	if calls < 2 {
		t.Fatalf("429 sonrası istek yeni kartla yinelendi (calls=%d)", calls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("istemci 200 almalı: %d (gövde: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "yeniden calisti") {
		t.Fatalf("yeniden deneme içeriği gelmeli: %s", rec.Body.String())
	}

	active, err := p.km.GetActiveKey()
	if err != nil {
		t.Fatalf("GetActiveKey: %v", err)
	}
	if active.ID == first.ID {
		t.Fatalf("aktif anahtar günlük 429 sonrası değişmeliydi")
	}
	if active.ID != k2.ID {
		t.Fatalf("aktif yeni kart k2 olmalı, alındı: %s", active.Name)
	}
	for _, k := range p.km.GetAllKeys() {
		if k.ID == first.ID && !k.IsExhausted {
			t.Fatal("429 yiyen kart IsExhausted olmalı (gün sıfırlamasına kadar havuz dışı)")
		}
	}
}

// TestProxy_StreamIdleKesintisiKontrollu, upstream sessizliğe gömüldüğünde
// akışın saatlerce asılı KALMADIĞINI (eski 600 sn toplam http.Client.Timeout
// yerine okuma-sessizlik sayacı) ve istemcinin kontrollü stream_interrupted
// + [DONE] aldığını kanıtlar.
func TestProxy_StreamIdleKesintisiKontrollu(t *testing.T) {
	withFakeEvren(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"bas\"}}]}\n\n")
		flusher.Flush()
		time.Sleep(1500 * time.Millisecond) // idle (200 ms) çok üzerinde sessizlik
	})
	p := newTestProxy(t)
	p.streamIdle = 200 * time.Millisecond

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.6-pro","stream":true,"messages":[{"role":"user","content":"merhaba"}]}`))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	out := rec.Body.String()
	if elapsed > 2*time.Second {
		t.Fatalf("idle sayacı devreye girmeliydi, istek sürdü: %s", elapsed)
	}
	if !strings.Contains(out, "stream_interrupted") {
		t.Fatalf("kontrollü stream_interrupted chunk'ı bekleniyordu: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("kontrollü [DONE] bekleniyordu: %s", out)
	}
}

// TestProxy_DevSSESatiriErTooLongYapmaz, tek satırda gelen dev tool_call /
// içerik deltasının bufio.ErrTooLong ile KESİLMEDİĞİNİ kanıtlar. Satır 2 MB;
// eski tavan 1 MB olduğundan bu test değişiklik ÖNCESİ kırılırdı.
func TestProxy_DevSSESatiriErTooLongYapmaz(t *testing.T) {
	big := strings.Repeat("A", 2*1024*1024)
	withFakeEvren(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", big)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	p := newTestProxy(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.6-pro","stream":true,"messages":[{"role":"user","content":"merhaba"}]}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, big) {
		t.Fatalf("dev satır birebir iletilmeli (ErrTooLong kesmesi olmamalı)")
	}
	if strings.Contains(out, "stream_interrupted") {
		t.Fatalf("sağlıklı akışta hata chunk'ı olmamalı")
	}
}
