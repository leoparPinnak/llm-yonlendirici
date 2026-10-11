package main

import (
	"encoding/json"
	"testing"
)

// msg, transkript mesajı kurucu: içerik ham JSON (compact) olarak saklanır.
func msg(role, content string) TranscriptMessage {
	return TranscriptMessage{Role: role, Content: compactJSON(json.RawMessage(`"` + content + `"`))}
}

// msgsOf, role/content çiftlerinden dizi kurar.
func msgsOf(pairs ...string) []TranscriptMessage {
	out := make([]TranscriptMessage, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, msg(pairs[i], pairs[i+1]))
	}
	return out
}

func TestSessionExplicitBodyIDWins(t *testing.T) {
	reg := newSessionRegistry(50)
	m1 := msgsOf("user", "selam dünya")
	a := reg.Resolve("oturum-1", "", "aaaa11112222", m1, 100)
	if a.ID != "oturum-1" || a.Tier != tierBody {
		t.Fatalf("explicit id gövdeden gelmeli: %+v", a)
	}
	// Aynı id, farklı içerik → aynı oturum
	b := reg.Resolve("oturum-1", "", "bbbb33334444", msgsOf("user", "başka içerik"), 100)
	if b.ID != "oturum-1" || b.Tier != tierBody {
		t.Fatalf("explicit id sabit kalmalı: %+v", b)
	}
}

func TestSessionHeaderHint(t *testing.T) {
	reg := newSessionRegistry(50)
	a := reg.Resolve("", "hdr-9", "aaaa11112222", msgsOf("user", "selam"), 0)
	if a.ID != "hdr-9" || a.Tier != tierHeader {
		t.Fatalf("header ipucu kullanılmalı: %+v", a)
	}
	b := reg.Resolve("", "hdr-9", "cccc55556666", msgsOf("user", "farklı başlangıç"), 0)
	if b.ID != "hdr-9" {
		t.Fatalf("header oturumu bölünmemeli: %+v", b)
	}
}

func TestSessionFingerprintStableAcrossTurns(t *testing.T) {
	reg := newSessionRegistry(50)
	turn1 := msgsOf("system", "sys", "user", " ilk mesaj ")
	a := reg.Resolve("", "", sessionFingerprintMsgs(turn1), turn1, 1)
	// Tur 2: geçmiş büyür, ilk mesaj aynı → aynı oturum
	turn2 := msgsOf("system", "sys", "user", " ilk mesaj ", "assistant", "yanıt", "user", "ikinci mesaj")
	b := reg.Resolve("", "", sessionFingerprintMsgs(turn2), turn2, 1)
	if a.ID == "" || a.Tier != tierFingerprint {
		t.Fatalf("ilk tur parmak iziyle açılmalı: %+v", a)
	}
	if b.ID != a.ID || b.Tier != tierFingerprint {
		t.Fatalf("aynı ilk mesaj aynı oturumu vermeli: %+v != %+v", b, a)
	}
	// Farklı ilk mesaj → farklı oturum
	turnOther := msgsOf("system", "sys", "user", "bambaşka sohbet")
	c := reg.Resolve("", "", sessionFingerprintMsgs(turnOther), turnOther, 1)
	if c.ID == a.ID {
		t.Fatalf("farklı sohbet ayrı oturum olmalı: %+v", c)
	}
}

func TestSessionCompactionBridgeKeepsSession(t *testing.T) {
	reg := newSessionRegistry(50)
	// Uzun sohbet: ilk mesaj sabit, birçok tur
	long := msgsOf(
		"system", "sys",
		"user", "görev bir",
		"assistant", "yanıt bir",
		"user", "görev iki",
		"assistant", "yanıt iki",
		"user", "görev üç",
		"assistant", "yanıt üç",
		"user", "görev dört",
	)
	a := reg.Resolve("", "", sessionFingerprintMsgs(long), long, 7)
	if a.ID == "" {
		t.Fatalf("başlangıç oturumu açılmadı")
	}

	// COMPACTION: ilk mesaj özetle değişti, kuyruk mesajları korundu
	compacted := msgsOf(
		"user", "[özet] daha önce görev bir/iki/üç işlendi",
		"user", "görev üç",
		"assistant", "yanıt üç",
		"user", "görev dört",
		"user", "görev beş",
	)
	fp := sessionFingerprintMsgs(compacted)
	if fp == "" || fp == a.FP {
		t.Fatalf("compaction parmak izi farklı olmalı: %q vs %q", fp, a.FP)
	}
	b := reg.Resolve("", "", fp, compacted, 7)
	if b.ID != a.ID {
		t.Fatalf("köprü oturumu bölmemeli: %q != %q (tier=%s)", b.ID, a.ID, b.Tier)
	}
	if b.Tier != tierBridge {
		t.Fatalf("compaction köprüsü tier=bridge olmalı: %s", b.Tier)
	}

	// Aynı yeni parmak izinin ikinci gelişi: takma ad artık doğrudan eşler
	compacted2 := append([]TranscriptMessage{}, compacted...)
	compacted2 = append(compacted2, msg("assistant", "yanıt beş"), msg("user", "görev altı"))
	c := reg.Resolve("", "", sessionFingerprintMsgs(compacted2), compacted2, 7)
	if c.ID != a.ID || c.Tier != tierFingerprint {
		t.Fatalf("yeni fp takma adı oturuma bağlanmalı: %+v", c)
	}
}

func TestSessionNoFalseMergeOnSharedSystemPrompt(t *testing.T) {
	reg := newSessionRegistry(50)
	a := msgsOf("system", "aynı sistem promptu", "user", "sohbet a", "assistant", "c a", "user", "devam a")
	sa := reg.Resolve("", "", sessionFingerprintMsgs(a), a, 3)

	b := msgsOf("system", "aynı sistem promptu", "user", "sohbet b", "assistant", "c b", "user", "devam b")
	sb := reg.Resolve("", "", sessionFingerprintMsgs(b), b, 3)
	if sa.ID == sb.ID {
		t.Fatalf("farklı sohbetler birleşmemeli: %+v / %+v", sa, sb)
	}

	// Sohbet b'nin devamı: yalnız system ortak (1 mesaj) → köprü TETİKLENMEZ
	b2 := msgsOf("system", "aynı sistem promptu", "user", "sohbet b", "assistant", "c b", "user", "devam b", "assistant", "c b2", "user", "yeni tur")
	sb2 := reg.Resolve("", "", sessionFingerprintMsgs(b2), b2, 3)
	if sb2.ID != sb.ID {
		t.Fatalf("devam kendi oturumunda kalmalı: %+v", sb2)
	}

	// Yeni ve kısa bir sohbet: system tek ortak → bridge yok, yeni oturum
	c := msgsOf("system", "aynı sistem promptu", "user", "sohbet c")
	sc := reg.Resolve("", "", sessionFingerprintMsgs(c), c, 3)
	if sc.ID == sa.ID || sc.ID == sb.ID {
		t.Fatalf("system promptu tek başına birleştirmemeli: %+v", sc)
	}
}

func TestSessionNoUserMessageFallsBackToLCP(t *testing.T) {
	reg := newSessionRegistry(50)
	m := msgsOf("assistant", "karşılama")
	info := reg.Resolve("", "", sessionFingerprintMsgs(m), m, 1)
	if info.ID != "" || info.Tier != tierLCP {
		t.Fatalf("kullanıcı mesajsız istek lcp'ye düşmeli: %+v", info)
	}
}

func TestSessionFingerprintMatchesLegacyAlgorithm(t *testing.T) {
	raw := json.RawMessage(`[{"role":"user","content":"merhaba dünya"}]`)
	legacy, _ := sessionFingerprint(raw)
	mine := sessionFingerprintMsgs([]TranscriptMessage{msg("user", "merhaba dünya")})
	if legacy != mine {
		t.Fatalf("yeni parmak izi eski algoritmayla birebir aynı olmalı: %q != %q", mine, legacy)
	}
}

func TestCaptureSessionHeaders(t *testing.T) {
	h := map[string][]string{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer gizli"},
		"X-Api-Key":     {"gizli2"},
		"Cookie":        {"sid=1"},
		"User-Agent":    {"dsh/1.0"},
		"X-Session-Id":  {"kanc-42"},
		"X-Custom":      {"değer"},
	}
	out := captureSessionHeaders(h)
	if _, ok := out["authorization"]; ok {
		t.Fatalf("authorization sızmamalı")
	}
	if out["x-session-id"] != "kanc-42" || out["x-custom"] != "değer" || out["user-agent"] != "dsh/1.0" {
		t.Fatalf("oturum başlıkları aynen alınmalı: %+v", out)
	}
	if len(out) != 3 {
		t.Fatalf("standart/gizli başlıklar atlanmalı: %+v", out)
	}
}

func TestLongestCommonContiguous(t *testing.T) {
	a := msgsOf("user", "1", "assistant", "2", "user", "3", "assistant", "4", "user", "5")
	b := msgsOf("user", "0", "assistant", "2", "user", "3", "assistant", "4", "user", "9")
	if n := longestCommonContiguous(digestsOf(a), digestsOf(b)); n != 3 {
		t.Fatalf("en uzun bitişik eşleşme 3 olmalı, aldı: %d", n)
	}
	if n := longestCommonContiguous(digestsOf(a), nil); n != 0 {
		t.Fatalf("boş dizi 0 dönmeli: %d", n)
	}
}

// TestSession_KalıcılıkYenidenBaslatma, oturum kimliklerinin ve compaction
// köprüsünün yeniden başlatma (taze registry + aynı dosya) sonrası da
// çalıştığını kanıtlar: "son istekler sürekli değişiyor" kapanır.
func TestSession_KalıcılıkYenidenBaslatma(t *testing.T) {
	path := t.TempDir() + "/sessions-state.json"

	reg1 := newSessionRegistry(50)
	reg1.setStatePath(path)

	long := msgsOf(
		"system", "sys",
		"user", "görev bir",
		"assistant", "yanıt bir",
		"user", "görev iki",
		"assistant", "yanıt iki",
		"user", "görev üç",
	)
	a := reg1.Resolve("", "", sessionFingerprintMsgs(long), long, 3)
	if a.ID == "" {
		t.Fatal("ilk oturum açılmadı")
	}
	reg1.saveState()

	// Yeniden başlatma: taze registry, aynı durum dosyası
	reg2 := newSessionRegistry(50)
	reg2.setStatePath(path)
	if id, tier := reg2.Lookup(sessionFingerprintMsgs(long), long); id != a.ID || tier != tierFingerprint {
		t.Fatalf("fp kimliği yeniden başlatmada korunmalı: %q/%s != %q", id, tier, a.ID)
	}

	// Compaction: fp değişti ama kuyruk ≥3 mesaj kayıtlı digest'lerle eşleşiyor
	compacted := msgsOf(
		"user", "[özet] önceki span özetlendi",
		"assistant", "yanıt bir",
		"user", "görev iki",
		"assistant", "yanıt iki",
		"user", "görev üç",
		"user", "yeni tur",
	)
	b := reg2.Resolve("", "", sessionFingerprintMsgs(compacted), compacted, 3)
	if b.ID != a.ID || b.Tier != tierBridge {
		t.Fatalf("köprü yeniden başlatmada da çalışmalı: %+v != %q", b, a.ID)
	}
}

// TestSession_SoyBagiSubagent, subagent kartının AYRI kalıp kimlik/atama
// katmanında doğuran oturuma bağlandığını; zincirin köke çıktığını ve
// döngülerin/kendine bağlamaların yok sayıldığını kanıtlar.
func TestSession_SoyBagiSubagent(t *testing.T) {
	reg := newSessionRegistry(50)
	parent := reg.Resolve("", "", "aaaaaaaaaaaa", msgsOf("user", "ana oturum mesaji"), 1)
	child := reg.Resolve("", "", "bbbbbbbbbbbb", msgsOf("user", "subagent gorev mesaji"), 1)
	if parent.ID == "" || child.ID == "" || parent.ID == child.ID {
		t.Fatalf("iki ayrı oturum bekleniyordu: %+v / %+v", parent, child)
	}

	reg.SetParent(child.ID, parent.ID)
	if got := reg.Root(child.ID); got != parent.ID {
		t.Fatalf("subagent kökü ana oturum olmalı: %q", got)
	}
	if got := reg.Root(parent.ID); got != parent.ID {
		t.Fatalf("kök oturum kendisini döndürmeli: %q", got)
	}

	// Torun: child → parent zinciri köke çıkar
	grand := reg.Resolve("", "", "cccccccccccc", msgsOf("user", "torun subagent mesaji"), 1)
	reg.SetParent(grand.ID, child.ID)
	if got := reg.Root(grand.ID); got != parent.ID {
		t.Fatalf("zincir kök oturuma çıkmalı: %q", got)
	}

	// Döngü: parent'ı toruna bağlamak yok sayılır
	reg.SetParent(parent.ID, grand.ID)
	if got := reg.Root(parent.ID); got != parent.ID {
		t.Fatalf("döngü kurulmamalı: %q", got)
	}
	// Kendine bağlama ve boş kimlikler yok sayılır
	reg.SetParent(child.ID, child.ID)
	reg.SetParent(child.ID, "")
	if reg.Root("") != "" {
		t.Fatal("boş id boş kök döndürmeli")
	}
	// Farklı kökler birbirine karışmaz
	other := reg.Resolve("", "", "dddddddddddd", msgsOf("user", "baska ana oturum"), 1)
	if got := reg.Root(other.ID); got != other.ID {
		t.Fatalf("soyu olmayan oturum kendi kökü olmalı: %q", got)
	}
}

// TestSession_SoyKalıcılık, child→parent bağının sessions-state.json'da
// yaşayıp yeniden başlatma (taze registry + aynı dosya) sonra da Root'a
// dönüştüğünü kanıtlar.
func TestSession_SoyKalıcılık(t *testing.T) {
	path := t.TempDir() + "/sessions-state.json"
	reg1 := newSessionRegistry(50)
	reg1.setStatePath(path)
	a := reg1.Resolve("", "", "aaaaaaaaaaaa", msgsOf("user", "kalici ana"), 1)
	b := reg1.Resolve("", "", "bbbbbbbbbbbb", msgsOf("user", "kalici alt"), 1)
	reg1.SetParent(b.ID, a.ID)
	reg1.saveState()

	reg2 := newSessionRegistry(50)
	reg2.setStatePath(path)
	if got := reg2.Root(b.ID); got != a.ID {
		t.Fatalf("soy bağ restart'ta korunmalı: %q != %q", got, a.ID)
	}
	if got := reg2.Root(a.ID); got != a.ID {
		t.Fatalf("kök restart'ta kendisi kalmalı: %q", got)
	}
}
