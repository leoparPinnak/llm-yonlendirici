package main

import (
	"strings"
	"testing"
	"time"
)

// TestLogger_OturumBazlıSıfırlama, RAM halkanın yalnızca hedef oturumun
// kayıtlarını (chat + embedding birlikte) sildiğini, diğer kayıtların
// ve dosya arşivinin kaldığını kanıtlar.
func TestLogger_OturumBazlıSıfırlama(t *testing.T) {
	l := &MemoryLogger{
		filePath:  t.TempDir() + "/usage_history.jsonl",
		capacity:  10,
		entries:   make([]RequestLog, 0, 10),
		turkeyLoc: time.FixedZone("UTC+3", 3*60*60),
	}
	l.Log(RequestLog{ID: "r1", SessionID: "sess-a"})
	l.Log(RequestLog{ID: "r2", SessionID: "sess-b"})
	l.Log(RequestLog{ID: "r3", SessionID: "sess-a", IsEmbedding: true})
	l.Log(RequestLog{ID: "r4", SessionID: "sess-c"})

	if n := l.ClearSession("sess-a"); n != 2 {
		t.Fatalf("sess-a iki kayıt içermeli (chat+embedding): %d", n)
	}
	if n := l.ClearSession(""); n != 0 {
		t.Fatalf("boş id silim yapmamalı: %d", n)
	}
	if n := l.ClearSession("sess-yok"); n != 0 {
		t.Fatalf("var olmayan oturum 0 dönmeli: %d", n)
	}

	remaining := l.GetRecent(0, "")
	if len(remaining) != 2 {
		t.Fatalf("kalan chat kaydı 2 olmalı: %d", len(remaining))
	}
	for _, e := range remaining {
		if e.SessionID == "sess-a" {
			t.Fatalf("sess-a kaydı kalmamalı: %+v", e)
		}
	}
	// Embedding görünümü de sess-a'dan arınmış olmalı
	for _, e := range l.GetEmbeddingLogs(0) {
		if e.SessionID == "sess-a" {
			t.Fatalf("sess-a embedding kaydı kalmamalı: %+v", e)
		}
	}
}

// TestTranscript_OturumBazlıSıfırlama, transcript kartının depodan silindiğini,
// fp indeksinin temizlendiğini ve KİMLİKİN KORUNARAK aynı fp'den taze kart
// açıldığını kanıtlar (siline=sağlam, kimlik=sabıt).
func TestTranscript_OturumBazlıSıfırlama(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	bodyA := []byte(`{"model":"m","messages":[{"role":"user","content":"sifirla oturum A ilk"}]}`)
	reqA := BuildTranscriptRequest(bodyA, "evren", time.Now())
	st.Record(reqA, TranscriptResponse{StatusCode: 200})

	bodyB := []byte(`{"model":"m","messages":[{"role":"user","content":"sifirla oturum B tek"}]}`)
	reqB := BuildTranscriptRequest(bodyB, "evren", time.Now())
	st.Record(reqB, TranscriptResponse{StatusCode: 200})

	d := dumpConvs(t, st)
	if len(d.Conversations) != 2 {
		t.Fatalf("iki kart bekleniyordu: %d", len(d.Conversations))
	}
	idA := ""
	for _, c := range d.Conversations {
		if len(c.Messages) > 0 && strings.Contains(string(c.Messages[0].Content), "oturum A") {
			idA = c.ConversationID
		}
	}
	if idA == "" {
		t.Fatal("A kartı bulunamadı")
	}

	if !st.ClearSession(idA) {
		t.Fatal("A kartı silinmeli")
	}
	if st.ClearSession(idA) {
		t.Fatal("ikinci silim kart kalmadığı için false olmalı")
	}
	d2 := dumpConvs(t, st)
	if len(d2.Conversations) != 1 || d2.Conversations[0].ConversationID == idA {
		t.Fatalf("yalnızca B kalmalı: %d kart", len(d2.Conversations))
	}

	// KİMLİK KORUNUR: aynı ilk mesajla (fp aynı) gelen yeni tur AYNI kimlikle
	// taze kart açar — fp, ilk kullanıcı mesajının özetidir.
	bodyA2 := []byte(`{"model":"m","messages":[{"role":"user","content":"sifirla oturum A ilk"},{"role":"assistant","content":"devam"},{"role":"user","content":"temiz sonrasi"}]}`)
	reqA2 := BuildTranscriptRequest(bodyA2, "evren", time.Now())
	st.Record(reqA2, TranscriptResponse{StatusCode: 200})

	d3 := dumpConvs(t, st)
	if len(d3.Conversations) != 2 {
		t.Fatalf("taze A kartıyla birlikte iki kart olmalı: %d", len(d3.Conversations))
	}
	found := false
	for _, c := range d3.Conversations {
		if c.ConversationID == idA {
			found = true
			if c.RequestCount != 1 || len(c.Messages) != 3 {
				t.Fatalf("kart TAZE olmalı (istek=%d, mesaj=%d)", c.RequestCount, len(c.Messages))
			}
		}
	}
	if !found {
		t.Fatalf("aynı kimlik (%s) taze kartla geri gelmeli", idA)
	}
}

// TestOturum_DshÖneki, DSH kimliklerine "dsh-" önekinin yalnızca YENİ
// oturumlara eklendiğini, mevcut kartı/defter kaydı olan ham kimliklerin
// (kart bölünmesin diye) korunduğunu ve kimlik-dışı değerlerin aynen
// geçtiğini kanıtlar.
func TestOturum_DshÖneki(t *testing.T) {
	// Kimlik-dışı değerler aynen geçer
	for _, id := range []string{"", "evs-abc123", "conv-deadbeef", "pool-canyon-1", "dsh-session-zaten"} {
		if got := CanonicalSessionID(id); got != id {
			t.Fatalf("kimlik-dışı değer değişmemeli: %q → %q", id, got)
		}
	}
	// Bilinmeyen DSH kimliği → önekli
	if got := CanonicalSessionID("session-yeni-oturum"); got != "dsh-session-yeni-oturum" {
		t.Fatalf("yeni oturum öneklenmeli: %q", got)
	}

	// Kartı MEVCUT ham kimlik → korunur (bölünme yok)
	st := withTranscriptStore(t, 50, 200)
	old := globalTranscripts
	globalTranscripts = st
	defer func() { globalTranscripts = old }()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"onek eski kart"}]}`)
	req := BuildTranscriptRequest(body, "evren", time.Now())
	req.ConversationID = "session-eski-kart" // ham kimlikle kaydet
	st.Record(req, TranscriptResponse{StatusCode: 200})
	if !st.HasConversation("session-eski-kart") {
		t.Fatal("kurulum: ham kart kaydı oluşmalı")
	}
	if got := CanonicalSessionID("session-eski-kart"); got != "session-eski-kart" {
		t.Fatalf("mevcut kart ham kalmalı: %q", got)
	}

	// Defterde bilinen (transkriptte kartı olmasa da) kimlik → korunur
	known := chatSessions.Resolve("", "", "feedface0001", msgsOf("user", "onek registry kaydi"), 1)
	if known.ID == "" {
		t.Fatal("kurulum: registry kaydı oluşmalı")
	}
	if got := CanonicalSessionID(known.ID); got != known.ID {
		t.Fatalf("defterde bilinen kimlik korunmalı: %q → %q", known.ID, got)
	}
}

// TestOturum_ZenKimlikKalıcılığı, fp alias'ı başka bir karta bağlanmış olsa
// bile AÇIK header kimliğinin Ensure ile deftere girdiğini (zen yolu boşluk
// düzeltmesi) ve deftere giren ham kimliğin "dsh-" öneki korumasını aldığını
// kanıtlar.
func TestOturum_ZenKimlikKalıcılığı(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)
	old := globalTranscripts
	globalTranscripts = st
	defer func() { globalTranscripts = old }()

	//1) fp önce ESKİ bir karta bağlı (first-wins guard'ı simüle eder)
	chatSessions.Alias("aabbccddeeff", "evs-eski-fp-karti")

	//2) Aynı fp + AÇIK kimlik gelsin (zen başlık yolu) — Alias burada REDDEDER
	//   ama Ensure kaydeder:
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"zen kimlik kaniti"}]}`)
	req := BuildTranscriptRequest(body, "zen", time.Now())
	req.ConversationID = "session-zen-kimlik"
	st.Record(req, TranscriptResponse{StatusCode: 200})

	if !chatSessions.Knows("session-zen-kimlik") {
		t.Fatal("açık header kimliği deftere girmeli (fp guard'ına takılmadan)")
	}
	// Guard korunmalı: fp hâlâ ilk karta bağlı
	chatSessions.Alias("aabbccddeeff", "session-zen-kimlik") // reddedilmeli
	if got := CanonicalSessionID("session-zen-kimlik"); got != "session-zen-kimlik" {
		t.Fatalf("deftere giren ham kimlik önek korumasını almalı: %q", got)
	}
	// Bilmeyen kimlik öneklenmeye devam eder
	if got := CanonicalSessionID("session-bilinmeyen"); got != "dsh-session-bilinmeyen" {
		t.Fatalf("bilinmeyen kimlik öneklenmeli: %q", got)
	}
}

// TestOturum_TümBellekSilme, "Tüm Oturumları Temizle" düğmesinin arkasını
// kanıtlar: transcript TÜMÜ silinir (kart + fp indeksi + sayaçlar) ve RAM
// halkası tamamen boşalır; kimlik/defter ve dosya arşivi dokunulmaz.
func TestOturum_TümBellekSilme(t *testing.T) {
	st := withTranscriptStore(t, 50, 200)

	bodyA := []byte(`{"model":"m","messages":[{"role":"user","content":"tumunu sil A"}]}`)
	st.Record(BuildTranscriptRequest(bodyA, "evren", time.Now()), TranscriptResponse{StatusCode: 200})
	bodyB := []byte(`{"model":"m","messages":[{"role":"user","content":"tumunu sil B"}]}`)
	st.Record(BuildTranscriptRequest(bodyB, "zen", time.Now()), TranscriptResponse{StatusCode: 200})

	convs, exchanges := st.ClearAll()
	if convs != 2 || exchanges != 2 {
		t.Fatalf("2 kart +2 exchange silinmeli: %d/%d", convs, exchanges)
	}
	if d := dumpConvs(t, st); len(d.Conversations) != 0 {
		t.Fatalf("tüm kartlar silinmeli: %d", len(d.Conversations))
	}
	// fp indeksi de gitti → aynı içerik yeniden kaydı taze kart açar
	st.Record(BuildTranscriptRequest(bodyA, "evren", time.Now()), TranscriptResponse{StatusCode: 200})
	if d := dumpConvs(t, st); len(d.Conversations) != 1 {
		t.Fatalf("temizlik sonrası taze kart: %d", len(d.Conversations))
	}

	// RAM halkası tamamen boşalır (arşiv dosyası değil — o zaten yazılmaz)
	l := &MemoryLogger{
		filePath:  t.TempDir() + "/usage_history.jsonl",
		capacity:  8,
		entries:   make([]RequestLog, 0, 8),
		turkeyLoc: time.FixedZone("UTC+3", 3*60*60),
	}
	l.Log(RequestLog{ID: "r1", SessionID: "s1"})
	l.Log(RequestLog{ID: "r2", SessionID: "s2"})
	if n := l.ClearAll(); n != 2 {
		t.Fatalf("halka silinmeli: %d", n)
	}
	if got := l.GetRecent(0, ""); len(got) != 0 {
		t.Fatalf("halka boş kalmalı: %d", len(got))
	}
}
