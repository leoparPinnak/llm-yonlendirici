package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTranscript_HeaderOturumIpucu, Zen için kademe-1 uzantısını kanıtlar:
// gövde yerine X-Conversation-Id başlığı verilirse içerik prefix'inden bağımsız
// olarak aynı oturuma bağlanır; header'sız istek yeni oturum açar.
func TestTranscript_HeaderOturumIpucu(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	send := func(body, headerID string) {
		t.Helper()
		raw := []byte(body)
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		if headerID != "" {
			r.Header.Set("X-Conversation-Id", headerID)
		}
		req := BuildTranscriptRequest(raw, "zen", time.Now())
		if req == nil {
			t.Fatal("istek ayrıştırılamadı")
		}
		if req.ConversationID == "" {
			req.ConversationID = HeaderConversationHint(r)
		}
		st.Record(req, TranscriptResponse{StatusCode: 200})
	}

	// Aynı header, TAMAMEN farklı içerik → tek oturumda kalmalı
	send(`{"model":"mimo","messages":[{"role":"user","content":"birinci konu"}]}`, "zen-oturum-7")
	send(`{"model":"mimo","messages":[{"role":"user","content":"bambaşka içerik"}]}`, "zen-oturum-7")

	d := dumpConvs(t, st)
	if len(d.Conversations) != 1 {
		t.Fatalf("aynı header tek oturumda kalmalı, alındı: %d", len(d.Conversations))
	}
	if d.Conversations[0].ConversationID != "zen-oturum-7" {
		t.Fatalf("oturum id header'dan gelmeli: %s", d.Conversations[0].ConversationID)
	}
	if d.Conversations[0].RequestCount != 2 {
		t.Fatalf("2 istek bekleniyordu: %d", d.Conversations[0].RequestCount)
	}

	// Header'sız istek (içerik örtüşmüyor) → yeni oturum
	send(`{"model":"mimo","messages":[{"role":"user","content":"başka sohbet"}]}`, "")
	if d2 := dumpConvs(t, st); len(d2.Conversations) != 2 {
		t.Fatalf("header'sız istek yeni oturum açmalı: %d", len(d2.Conversations))
	}

	// Başlık okuma: X-Session-Id de kabul + trim
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Session-Id", "  sid-9  ")
	if got := HeaderConversationHint(r); got != "sid-9" {
		t.Fatalf("X-Session-Id trim edilerek okunmalı: %q", got)
	}
	if HeaderConversationHint(nil) != "" {
		t.Fatal("nil istek için ipuç boş olmalı")
	}
}

func withTranscriptStore(t *testing.T, convs, exchanges int) *TranscriptStore {
	t.Helper()
	old := globalTranscripts
	t.Cleanup(func() { globalTranscripts = old })
	return InitTranscripts(convs, exchanges)
}

// TestTranscript_OturumBazliDump tek oturumun response-uyumlu endpoint
// çıktısını (yalnızca istenen oturum + doğru şema) ve olmayan id için dürüst
// ok=false'ı doğrular.
func TestTranscript_OturumBazliDump(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	body := []byte(`{"model":"mimo","messages":[{"role":"user","content":"selam"}]}`)
	req := BuildTranscriptRequest(body, "evren", time.Now())
	if req == nil {
		t.Fatal("istek ayrıştırılamadı")
	}
	st.Record(req, TranscriptResponse{
		StatusCode: 200,
		Usage:      &UsageResult{PromptTokens: 10, CompletionTokens: 5, CachedTokens: 4, ReasoningTokens: 2, NetDeduction: 6},
	})

	var full TranscriptDump
	if err := json.Unmarshal(st.DumpBytes(), &full); err != nil || len(full.Conversations) != 1 {
		t.Fatalf("kurulum dump'ı bozuk: %v", err)
	}
	id := full.Conversations[0].ConversationID

	data, ok := st.DumpConversationBytes(id)
	if !ok {
		t.Fatal("mevcut oturum dump'ı bulunmalı")
	}
	var one TranscriptDump
	if err := json.Unmarshal(data, &one); err != nil {
		t.Fatalf("oturum dump'ı çözümlenemedi: %v", err)
	}
	if one.Format != transcriptFormat {
		t.Fatalf("şema adı korunmalı: %q", one.Format)
	}
	if len(one.Conversations) != 1 || one.Conversations[0].ConversationID != id {
		t.Fatalf("yalnızca istenen oturum dönmeli: %+v", one.Conversations)
	}
	if one.Conversations[0].RequestCount != 1 {
		t.Fatalf("istek sayısı 1 olmalı: %d", one.Conversations[0].RequestCount)
	}
	ex0 := one.Conversations[0].Exchanges[0]
	if ex0.Response.Usage == nil || ex0.Response.Usage.ReasoningTokens != 2 {
		t.Fatalf("oturum logunda usage korunmalı: %+v", ex0.Response.Usage)
	}

	if _, ok := st.DumpConversationBytes("olmayan-oturum"); ok {
		t.Fatal("olmayan oturum ok=false dönmeli (404 için)")
	}
}

func dumpConvs(t *testing.T, st *TranscriptStore) *TranscriptDump {
	t.Helper()
	var d TranscriptDump
	if err := json.Unmarshal(st.DumpBytes(), &d); err != nil {
		t.Fatalf("dump çözümlenemedi: %v", err)
	}
	return &d
}

// TestTranscript_DinamikBaglamZinciri aynı sohbetin mesajlarını istekler
// boyunca dinamik biriktirir; delta yalnızca YENİ mesajları içerir.
func TestTranscript_DinamikBaglamZinciri(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	body1 := []byte(`{"model":"mimo","messages":[{"role":"user","content":"merhaba"}]}`)
	req1 := BuildTranscriptRequest(body1, "evren", time.Now())
	if req1 == nil {
		t.Fatal("istek 1 ayrıştırılamadı")
	}
	st.Record(req1, TranscriptResponse{
		StatusCode: 200,
		Message:    &TranscriptMessage{Role: "assistant", Content: json.RawMessage(`"selam"`), Timestamp: tsNow()},
	})

	body2 := []byte(`{"model":"mimo","stream":true,"messages":[
		{"role":"user","content":"merhaba"},
		{"role":"assistant","content":"selam"},
		{"role":"user","content":"nasılsın"}]}`)
	req2 := BuildTranscriptRequest(body2, "evren", time.Now())
	if req2 == nil {
		t.Fatal("istek 2 ayrıştırılamadı")
	}
	st.Record(req2, TranscriptResponse{
		StatusCode: 200,
		Message:    &TranscriptMessage{Role: "assistant", Content: json.RawMessage(`"iyiyim"`), Timestamp: tsNow()},
	})

	d := dumpConvs(t, st)
	if len(d.Conversations) != 1 {
		t.Fatalf("aynı sohbet 1 konuşmada olmalı, alındı: %d", len(d.Conversations))
	}
	conv := d.Conversations[0]
	if conv.RequestCount != 2 {
		t.Fatalf("toplam istek 2 olmalı, alındı: %d", conv.RequestCount)
	}
	if len(conv.Messages) != 4 {
		t.Fatalf("tam bağlam 4 mesaj olmalı (user, asistan, user, asistan), alındı: %d", len(conv.Messages))
	}
	if len(req2.MessagesNew) != 1 || req2.MessagesNew[0].Role != "user" {
		t.Fatalf("delta yalnızca yeni mesaj içermeli, alındı: %+v", req2.MessagesNew)
	}
	if conv.LastSeenAt == "" || conv.FirstSeenAt == "" {
		t.Fatal("konuşma zaman damgaları boş olamaz")
	}
	if len(conv.Models) != 1 || conv.Models[0] != "mimo" {
		t.Fatalf("model listesi hatalı: %+v", conv.Models)
	}
	// Tüm mesajlar zaman damgalı olmalı
	for _, m := range conv.Messages {
		if m.Timestamp == "" {
			t.Fatalf("mesaj zamansız: %+v", m)
		}
	}
}

// TestTranscript_DallanmaTekOturumdaKalir: aynı ilk kullanıcı mesajını (fp)
// taşıyan dallanan istek OTURUMU BÖLMEZ — panoda tek kart kalır, bağlam
// yeni dala döner. (Eski "dallanma yeni konuşma açar" davranışı, tek-oturum
// şartıyla bilinçli olarak kaldırıldı: zen LCP dönemi DİKEYDİZİLERİM
// sohbetini 3 karta bölmüştü — bkz. fixture analizi.)
func TestTranscript_DallanmaTekOturumdaKalir(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	r1 := BuildTranscriptRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"soru A"}]}`), "evren", time.Now())
	st.Record(r1, TranscriptResponse{StatusCode: 200, Message: &TranscriptMessage{Role: "assistant", Content: json.RawMessage(`"yanıt A"`), Timestamp: tsNow()}})

	// Aynı ilk mesaj, FARKLI ikinci mesaj → eskiden bölerdi; şimdi tek kart.
	r2 := BuildTranscriptRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"soru A"},{"role":"assistant","content":"BAŞKA yanıt"},{"role":"user","content":"devam"}]}`), "evren", time.Now())
	st.Record(r2, TranscriptResponse{StatusCode: 200})

	d := dumpConvs(t, st)
	if len(d.Conversations) != 1 {
		t.Fatalf("aynı fp tek oturumda kalmalı, konuşma sayısı: %d", len(d.Conversations))
	}
	conv := d.Conversations[0]
	if conv.RequestCount != 2 {
		t.Fatalf("istek sayısı 2 olmalı: %d", conv.RequestCount)
	}
	// Bağlam yeni dala dönmeli: son mesaj "devam" (user).
	last := conv.Messages[len(conv.Messages)-1]
	if last.Role != "user" || string(last.Content) != `"devam"` {
		t.Fatalf("bağlam yeni dala dönmeli, son mesaj: %s %s", last.Role, string(last.Content))
	}
}

// TestTranscript_BolunmusKartBenimseme: panoda ÖNCEDEN var olan (bölünmüş)
// bir konuşmanın fp'siyle gelen istek YENİ KART AÇMAZ, o kartı benimser ve
// registry'ye takma ad bağlanır.
func TestTranscript_BolunmusKartBenimseme(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	// Eski dönemden kalma kart: conv- id'li, fp'si hesaplanabilir konuşmayı
	// doğrudan depoya enjekte et (o dönemin LCP davranışı böyle kart açmıştı).
	legacyMsgs := []TranscriptMessage{msg("user", "eski diger diziler favorisi"), msg("assistant", "tamam")}
	legacy := &TranscriptConversation{
		ConversationID: "conv-eski-donem",
		Source:         "zen",
		FirstSeenAt:    tsNow(),
		LastSeenAt:     tsNow(),
		RequestCount:   1,
		Messages:       legacyMsgs,
	}
	st.convs = append(st.convs, legacy)
	st.byID[legacy.ConversationID] = legacy

	// Aynı ilk mesajla (fp aynı) gelen istek YENİ KART açmamalı.
	r := BuildTranscriptRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"eski diger diziler favorisi"},{"role":"assistant","content":"tamam"},{"role":"user","content":"yeni tur"}]}`), "zen", time.Now())
	st.Record(r, TranscriptResponse{StatusCode: 200})

	if len(st.convs) != 1 {
		t.Fatalf("yeni kart açılmamalı, konuşma sayısı: %d", len(st.convs))
	}
	if st.convs[0].ConversationID != "conv-eski-donem" {
		t.Fatalf("eski kart benimsenmeli: %s", st.convs[0].ConversationID)
	}
	if st.convs[0].RequestCount != 2 {
		t.Fatalf("istek 2 olmalı: %d", st.convs[0].RequestCount)
	}
	if id, _ := chatSessions.Lookup(sessionFingerprintMsgs(r.Messages), r.Messages); id != "conv-eski-donem" {
		t.Fatalf("registry takma adı eski karta bağlanmalı: %q", id)
	}
}

// TestTranscript_AcikOturumIdSi istekte conversation_id varsa onu kullanır.
func TestTranscript_AcikOturumIdSi(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	r1 := BuildTranscriptRequest([]byte(`{"conversation_id":"oturum-42","model":"m","messages":[{"role":"user","content":"selam"}]}`), "evren", time.Now())
	if r1.ConversationID != "oturum-42" {
		t.Fatalf("oturum ipucu yakalanmalı, alındı: %q", r1.ConversationID)
	}
	st.Record(r1, TranscriptResponse{StatusCode: 200})

	r2 := BuildTranscriptRequest([]byte(`{"conversation_id":"oturum-42","model":"m","messages":[{"role":"user","content":"selam"},{"role":"assistant","content":"ne var"},{"role":"user","content":"yeni tur"}]}`), "evren", time.Now())
	st.Record(r2, TranscriptResponse{StatusCode: 200})

	d := dumpConvs(t, st)
	if len(d.Conversations) != 1 {
		t.Fatalf("aynı oturum id'si tek konuşma olmalı, alındı: %d", len(d.Conversations))
	}
	if d.Conversations[0].ConversationID != "oturum-42" {
		t.Fatalf("konuşma id'si korunmalı, alındı: %s", d.Conversations[0].ConversationID)
	}
	if d.Conversations[0].RequestCount != 2 {
		t.Fatalf("istek sayısı 2 olmalı, alındı: %d", d.Conversations[0].RequestCount)
	}
}

// TestTranscript_HalkaTamponuSiniri 50 oturum / 200 istek sınırını zorlar.
func TestTranscript_HalkaTamponuSiniri(t *testing.T) {
	st := withTranscriptStore(t, 2, 3)

	for i := range 4 {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"soru` + string(rune('A'+i)) + `"}` + `]}`)
		req := BuildTranscriptRequest(body, "evren", time.Now())
		st.Record(req, TranscriptResponse{StatusCode: 200})
	}

	d := dumpConvs(t, st)
	if len(d.Conversations) > 2 {
		t.Fatalf("konuşma sayısı sınırı 2, alındı: %d", len(d.Conversations))
	}
	if d.Stats["exchanges"] > 3 {
		t.Fatalf("istek sınırı 3, alındı: %d", d.Stats["exchanges"])
	}
	if d.Stats["max_conversations"] != 2 || d.Stats["max_exchanges"] != 3 {
		t.Fatalf("dump stats kapasiteleri hatalı: %+v", d.Stats)
	}
}

// TestTranscript_HataDökümü upstream hatasının dump'ta birebir durduğunu kanıtlar.
func TestTranscript_HataDökümü(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	req := BuildTranscriptRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"soru"}]}`), "evren", time.Now())
	st.Record(req, TranscriptResponse{
		StatusCode: 429,
		Error:      `HTTP 429 | {"error":{"message":"quota exceeded"}}`,
		Raw:        rawJSONOrString([]byte(`{"error":{"message":"quota exceeded"}}`)),
	})

	d := dumpConvs(t, st)
	if len(d.Conversations) != 1 {
		t.Fatalf("konuşma yok: %d", len(d.Conversations))
	}
	resp := d.Conversations[0].Exchanges[0].Response
	if resp.Error != `HTTP 429 | {"error":{"message":"quota exceeded"}}` {
		t.Fatalf("hata birebir korunmalı, alındı: %s", resp.Error)
	}
	if resp.StatusCode != 429 {
		t.Fatalf("durum kodu 429 olmalı, alındı: %d", resp.StatusCode)
	}
}

// TestTranscript_ZamanDamgasiTSI kayıtların +03:00 offset'li zaman damgalı
// olduğunu doğrular (UI'daki canlı sayaç bunlara dayanır).
func TestTranscript_ZamanDamgasiTSI(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)
	req := BuildTranscriptRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"selam"}]}`), "evren", time.Now())
	st.Record(req, TranscriptResponse{StatusCode: 200})

	d := dumpConvs(t, st)
	ts := d.Conversations[0].LastSeenAt
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Fatalf("last_seen_at RFC3339Nano olmalı: %v", err)
	}
}

// TestTranscript_NilGuvenli nil depo ve nil istek kaydı çökmemeli.
func TestTranscript_NilGuvenli(t *testing.T) {
	var st *TranscriptStore
	st.Record(nil, TranscriptResponse{}) // nil receiver, no-op
	if len(st.DumpBytes()) == 0 {
		t.Fatal("nil dump boş dönmeli")
	}
	old := globalTranscripts
	globalTranscripts = nil
	defer func() { globalTranscripts = old }()
	req := BuildTranscriptRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`), "evren", time.Now())
	globalTranscripts.Record(req, TranscriptResponse{}) // nil global, no-op
}

// TestTranscript_SubagentSoyIpucu, X-Parent-Session-Id başlığının okunduğunu
// ve Record'un alt oturumun KARTINI AYNI TUTARKEN soy bağını kayıt
// defterine yazdığını kanıtlar (evren ve zen yolları ortak akış).
func TestTranscript_SubagentSoyIpucu(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	// Başlık okuması: trim + nil güvenli
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Parent-Session-Id", "  sess-parent  ")
	if got := HeaderParentSessionHint(r); got != "sess-parent" {
		t.Fatalf("trim'li okunmalı: %q", got)
	}
	if HeaderParentSessionHint(nil) != "" {
		t.Fatal("nil istek boş dönmeli")
	}

	// Record: child kartı oluşur, bağ deftere yazılır
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"subagent gorev mesaji"}]}`)
	req := BuildTranscriptRequest(body, "evren", time.Now())
	req.ParentSessionID = "sess-parent"
	st.Record(req, TranscriptResponse{StatusCode: 200})

	d := dumpConvs(t, st)
	if len(d.Conversations) != 1 {
		t.Fatalf("tek kart bekleniyordu: %d", len(d.Conversations))
	}
	child := d.Conversations[0].ConversationID
	if child == "sess-parent" {
		t.Fatal("çocuk kart, parent'ın kartıyla birleşmemeli")
	}
	if got := chatSessions.Root(child); got != "sess-parent" {
		t.Fatalf("kayıt defterinde kök sess-parent olmalı: %q", got)
	}

	// Aynı child'ın ikinci turu AYNI kartta kalmaya devam eder
	req2 := BuildTranscriptRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"subagent gorev mesaji"},{"role":"assistant","content":"tamam"},{"role":"user","content":"ikinci tur"}]}`), "evren", time.Now())
	req2.ParentSessionID = "sess-parent"
	st.Record(req2, TranscriptResponse{StatusCode: 200})
	if d2 := dumpConvs(t, st); len(d2.Conversations) != 1 || d2.Conversations[0].RequestCount != 2 {
		t.Fatalf("child ikinci turda aynı kartta kalmalı: kart=%d istek=%d", len(d2.Conversations), d2.Conversations[0].RequestCount)
	}
}
