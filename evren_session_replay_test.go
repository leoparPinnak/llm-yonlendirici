package main

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ==== GEÇMİŞ LOG YENİDEN OYNATMA ============================================
//
// Amaç (goal adımı 4): kayıtlı istek gövdeleri ve transkriptler taze bir
// oturum deposu üzerinden yeniden oynatılır; hiçbir istek farklı oturuma
// düşmez (bölünme=0) ve hiçbir farklı oturum birleştirilmez
// (yanlış birleşme=0). Kanıt dosyaları testdata/replay/ altındadır:
//   - logs.json        → /api/evren/logs?limit=1000 (ham istek günlüğü)
//   - transcript.json  → /api/evren/logs/transcript (tüm konuşmalar)

const (
	replayLogsPath       = "testdata/replay/logs.json"
	replayTranscriptPath = "testdata/replay/transcript.json"
)

// replaySortID, req-<nanos> kimliğini sayıya çevirir (sıra için).
func replaySortID(id string) int64 {
	if !strings.HasPrefix(id, "req-") {
		return 0
	}
	n, err := strconv.ParseInt(id[len("req-"):], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func firstUserContentOf(msgs []TranscriptMessage) string {
	for _, m := range msgs {
		if m.Role == "user" {
			return string(m.Content)
		}
	}
	return ""
}

// TestReplay_HamIstekGunluguAltınEsleme, ham istek günlüğündeki her kaydı
// taze registry üzerinden oynatır ve canlıda atanan session_id ile BİREBİR
// aynı sonucu üretmek zorunda olduğunu söyler (determinizm = sıfır sapma).
func TestReplay_HamIstekGunluguAltınEsleme(t *testing.T) {
	raw, err := os.ReadFile(replayLogsPath)
	if err != nil {
		t.Skipf("fixture yok (%s): gateway canlı API'sinden çekilmeli", replayLogsPath)
	}
	var entries []RequestLog
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("logs.json çözümlenemedi: %v", err)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return replaySortID(entries[i].ID) < replaySortID(entries[j].ID)
	})

	reg := newSessionRegistry(1000)
	compared := 0
	clipped := 0
	for _, e := range entries {
		if e.SessionID == "" || e.RequestBody == "" || !strings.Contains(e.RequestBody, `"messages"`) {
			continue
		}
		req := BuildTranscriptRequest([]byte(e.RequestBody), "evren", time.Now())
		if req == nil || len(req.Messages) == 0 {
			// Bilinen sınır: gövdeler 64 KiB'te kırpılır (kayıtta "[kırpıldı]"
			// izi var) — büyük checkpoint gövdeleri oynatılamaz, atlanır.
			clipped++
			continue
		}
		headerID := ""
		if e.ClientHeaders != nil {
			if v := e.ClientHeaders["x-session-id"]; v != "" {
				headerID = v
			} else if v := e.ClientHeaders["x-conversation-id"]; v != "" {
				headerID = v
			}
		}
		fp := sessionFingerprintMsgs(req.Messages)
		info := reg.Resolve(req.ConversationID, headerID, fp, req.Messages, e.ClientPID)
		if info.ID != e.SessionID {
			t.Errorf("ALTIN ESLEME SAPTI: id=%s ts=%s canlı=%q replay=%q (tier=%s)",
				e.ID, e.Timestamp, e.SessionID, info.ID, info.Tier)
			continue
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("hiç kayıt karşılaştırılamadı — fixture boş veya uyumsuz")
	}
	t.Logf("ALTIN: %d kayıt canlı atamayla birebir (bölünme=0, sapma=0); %d kayıt kırpma nedeniyle atlandı", compared, clipped)
}

// TestReplay_TranscriptTekOturum, transkript fixture'ını taze registry ile
// oynatır ve iki garantiyi sınar:
//
//	A) AYNI ilk kullanıcı mesajını taşıyan TÜM exchange'ler tek oturuma düşer
//	   (DİKEYDİZİLERİM'in b8cf/b038/fa43 üç parçaya bölünmesi sınıfı),
//	B) tek bir oturumda birden çok fp varsa arada KÖPRÜ (bridge) kaydı bulunur
//	   — fp değişimi dışında bir oturum birleştirmesi imkânsızdır.
func TestReplay_TranscriptTekOturum(t *testing.T) {
	raw, err := os.ReadFile(replayTranscriptPath)
	if err != nil {
		t.Skipf("fixture yok (%s)", replayTranscriptPath)
	}
	var dump TranscriptDump
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatalf("transcript.json çözümlenemedi: %v", err)
	}

	type rex struct {
		conv string
		msgs []TranscriptMessage
		ts   time.Time
	}
	var all []rex
	for _, c := range dump.Conversations {
		for _, e := range c.Exchanges {
			ts, _ := time.Parse(time.RFC3339Nano, e.Request.ReceivedAt)
			all = append(all, rex{conv: c.ConversationID, msgs: e.Request.Messages, ts: ts})
		}
	}
	if len(all) == 0 {
		t.Fatal("transcript fixture boş")
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ts.Before(all[j].ts) })

	reg := newSessionRegistry(1000)
	fpOwner := map[string]string{}  // ilk kullanıcı mesajı → sid
	sidFPS := map[string]map[string]bool{}
	sidTier := map[string]map[string]bool{}
	var missingUser []string
	for i, r := range all {
		fuc := firstUserContentOf(r.msgs)
		if fuc == "" {
			missingUser = append(missingUser, r.conv)
			continue
		}
		fp := sessionFingerprintMsgs(r.msgs)
		info := reg.Resolve("", "", fp, r.msgs, 0)
		if info.ID == "" {
			t.Errorf("[%d] (%s) oturum çözümlenemedi", i, r.conv)
			continue
		}
		// Garanti A: aynı ilk kullanıcı mesajı → tek oturum
		if prev, ok := fpOwner[fuc]; ok && prev != info.ID {
			t.Errorf("BÖLÜNME: aynı ilk mesaj iki oturuma düştü: %q vs %q (kaynak kart %s)", prev, info.ID, r.conv)
		}
		fpOwner[fuc] = info.ID
		if sidFPS[info.ID] == nil {
			sidFPS[info.ID] = map[string]bool{}
			sidTier[info.ID] = map[string]bool{}
		}
		sidFPS[info.ID][fp] = true
		sidTier[info.ID][info.Tier] = true
	}
	if len(missingUser) > 0 {
		t.Logf("kullanıcı mesajsız exchange atlandı: %d", len(missingUser))
	}

	// Garanti B: çok-fp'li oturum yalnızca köprü/explicit ile birleşebilir
	for sid, fps := range sidFPS {
		if len(fps) <= 1 {
			continue
		}
		if sidTier[sid][tierBridge] || sidTier[sid][tierBody] || sidTier[sid][tierHeader] {
			continue
		}
		t.Errorf("YANLIŞ BİRLEŞME: oturum %q %d farklı fp taşıyor ama köprü/explicit kayıt yok",
			sid, len(fps))
	}
	t.Logf("transcript oynatması: %d exchange, %d farklı fp, %d oturum", len(all), len(fpOwner), len(sidFPS))
}
