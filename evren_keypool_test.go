package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// poolEnabled, fixture'daki oto geçiş bayrağı için gösterge değeri üretir.
func poolEnabled(v bool) *bool { return &v }

// writePoolFixture, n havuz üyesi anahtarlı (10M limit, 50k marj, key-1
// aktif) geçerli bir keys.json yazar ve yolunu döndürür. Anahtarların değeri
// doludur — kullanılabilirlik denetimi gerçek akışla aynıdır.
func writePoolFixture(t *testing.T, n int, autoSwitch bool) string {
	t.Helper()
	loc := time.FixedZone("UTC+3", 3*60*60)
	keys := make([]*APIKey, 0, n)
	for i := 1; i <= n; i++ {
		keys = append(keys, &APIKey{
			ID:              fmt.Sprintf("key-%d", i),
			Name:            fmt.Sprintf("kart-%d", i),
			Key:             fmt.Sprintf("secret-%d", i),
			DailyLimit:      10000000,
			ThresholdTokens: 50000,
			IsActive:        i == 1,
			AutoSwitch:      true,
			AutoSwitchOrder: i,
			CreatedAt:       time.Now().In(loc).Format(time.RFC3339),
		})
	}
	state := KeysState{
		Keys:              keys,
		LastResetDate:     time.Now().In(loc).Add(-3 * time.Hour).Format("2006-01-02"),
		AutoSwitchEnabled: poolEnabled(autoSwitch),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestKeyPool_OturumBasindaDagitim, dokuz oturuma dokuz FARKLI anahtarın
// deterministik dağıtıldığını ve10. oturumun bir kartı paylaştığını kanıtlar
// ("9 api oturum süreçlerine dağıtılır").
func TestKeyPool_OturumBasindaDagitim(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 9, true))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]string) // keyID → root
	for i := 0; i < 9; i++ {
		root := fmt.Sprintf("sess-%d", i)
		k, err := km.KeyForSession(root)
		if err != nil {
			t.Fatalf("%s için anahtar alınamadı: %v", root, err)
		}
		if prev, dup := seen[k.ID]; dup {
			t.Fatalf("anahtar %s iki oturuma birden dağıtıldı (%s ve %s)", k.ID, prev, root)
		}
		seen[k.ID] = root
	}
	// Kararlılık: her oturum ikinci çağrıda AYNI anahtarı alır.
	for i := 0; i < 9; i++ {
		root := fmt.Sprintf("sess-%d", i)
		k, err := km.KeyForSession(root)
		if err != nil {
			t.Fatal(err)
		}
		if seen[k.ID] != root {
			t.Fatalf("oturum %s kararlı kalmalıydı; %s → %s", root, k.ID, seen[k.ID])
		}
	}
	// On birinci oturum: havuz doldu → bir kart paylaşılır (hata değil).
	if _, err := km.KeyForSession("sess-10"); err != nil {
		t.Fatalf("havuz dolunca kart paylaşımı hata üretmemeli: %v", err)
	}
}

// TestKeyPool_MarjTuketimdeHemenYenidenBag, bağlı anahtarın marj sınırını
// aştığı anda oturumun havuzdan YENİ anahtara taşındığını kanıtlar
// ("limiti biterse hemen havuzdan yenisi atanır").
func TestKeyPool_MarjTuketimdeHemenYenidenBag(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	first, err := km.KeyForSession("sess-x")
	if err != nil {
		t.Fatal(err)
	}
	// Bağlı anahtar marj sınırına gelir (kullanılabilirlik biter).
	for _, k := range km.keys {
		if k.ID == first.ID {
			k.UsedDaily = k.DailyLimit - k.ThresholdTokens // effectiveLimit'e eşit → artık < değil
		}
	}
	second, err := km.KeyForSession("sess-x")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("kotası biten anahtarla kalmamalı: %s", second.ID)
	}
	// Yeni bağ kararlı: üçüncü çağrı yeni anahtarda kalır.
	third, err := km.KeyForSession("sess-x")
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != second.ID {
		t.Fatalf("yeniden bağlama kararlı olmalı: %s → %s", second.ID, third.ID)
	}
}

// TestKeyPool_Gunluk429TekAdimdaTasir, upstream günlük429 rotasyonunun
// o anahtara bağlı TÜM oturumları yeni karta taşıdığını kanıtlar.
func TestKeyPool_Gunluk429TekAdimdaTasir(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := km.KeyForSession("sess-a"); err != nil {
		t.Fatal(err)
	}
	next, ok := km.RotateAfterDailyLimit("key-1")
	if !ok || next == nil {
		t.Fatalf("rotasyon tetiklenmeli: ok=%v", ok)
	}
	k, err := km.KeyForSession("sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != next.ID {
		t.Fatalf("oturum rotasyonu izlemeli: %s ≠ %s", k.ID, next.ID)
	}
	for _, key := range km.keys {
		if key.ID == "key-1" && !key.IsExhausted {
			t.Fatal("429 alan anahtar havuzdan çıkarılmalı (IsExhausted)")
		}
	}
}

// TestKeyPool_MarjRotasyonuOturumlariTasir, DeductUsage marj aşımının hem
// aktifliği hem o anahtarın oturumlarını taşıdığını kanıtlar.
func TestKeyPool_MarjRotasyonuOturumlariTasir(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := km.KeyForSession("sess-b"); err != nil {
		t.Fatal(err)
	}
	usage := UsageResult{NetDeduction: 9950000, GrossDeduction: 9950000}
	nextActive, rotated, err := km.DeductUsage("key-1", usage, "net")
	if err != nil {
		t.Fatal(err)
	}
	if !rotated || nextActive == nil {
		t.Fatalf("marj aşımı rotasyonu tetiklemeli: rotated=%v", rotated)
	}
	k, err := km.KeyForSession("sess-b")
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != nextActive.ID {
		t.Fatalf("oturum marj rotasyonunu izlemeli: %s ≠ %s", k.ID, nextActive.ID)
	}
}

// TestKeyPool_HavuzBoskenUpstream429Hakem, havuz tamamen tükenince oturumun
// bağlı anahtarında KALDIĞINI (yedek yoksa upstream'in günlük429'u hatayı
// söyleyeceğini) kanıtlar.
func TestKeyPool_HavuzBoskenUpstream429Hakem(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	first, err := km.KeyForSession("sess-c")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range km.keys {
		k.IsExhausted = true // dokuz kart da bitti
	}
	// Bağlı oturum: bağlı anahtara düşer (hata değil, upstream yargılar)
	k, err := km.KeyForSession("sess-c")
	if err != nil {
		t.Fatalf("havuz boşken hata değil upstream429 beklenir: %v", err)
	}
	if k.ID != first.ID {
		t.Fatalf("bağ korunmalı: %s ≠ %s", k.ID, first.ID)
	}
	// Bağsız yeni oturum: aktif anahtar (havuz yenilene kadar)
	fresh, err := km.KeyForSession("sess-yeni")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.IsActive {
		t.Fatalf("bağsız oturum aktif anahtarı almalı: %s", fresh.ID)
	}
}

// TestKeyPool_Kalıcılık, oturum→anahtar bağının keys.json'da yaşayıp
// yeniden başlatma (aynı dosya) sonra da sürdüğünü kanıtlar.
func TestKeyPool_Kalıcılık(t *testing.T) {
	path := writePoolFixture(t, 4, true)
	km, err := NewKeyManager(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := km.KeyForSession("sess-k")
	if err != nil {
		t.Fatal(err)
	}
	km2, err := NewKeyManager(path) // yeniden başlatma
	if err != nil {
		t.Fatal(err)
	}
	again, err := km2.KeyForSession("sess-k")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("bağ restart'ta korunmalı: %s ≠ %s", again.ID, first.ID)
	}
}

// TestKeyPool_OturusKapaliykenDagitimYok, oto geçiş KAPALIYKEN tüm
// oturumların aktif anahtarda toplandığını (eski davranışın birebir korunduğunu)
// kanıtlar.
func TestKeyPool_OturusKapaliykenDagitimYok(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 5, false))
	if err != nil {
		t.Fatal(err)
	}
	a, err := km.KeyForSession("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := km.KeyForSession("sess-2")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "key-1" || b.ID != "key-1" {
		t.Fatalf("oto geçiş kapalıyken dağıtım olmamalı: %s / %s", a.ID, b.ID)
	}
}

// TestKeyPool_GunlukSifirlamaBaglariKorur, havuz tamamen tükendikten sonra
// 03:00 TSİ sıfırlamasının kartları YENİDEN kullanılabilir yaptığını ve
// oturum→anahtar bağını EZMEDİĞİNİ kanıtlar ("limitler bitikçe havuz azalır
// yenilene kadar").
func TestKeyPool_GunlukSifirlamaBaglariKorur(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	first, err := km.KeyForSession("sess-r")
	if err != nil {
		t.Fatal(err)
	}
	// Havuzun TAMAMI tükendi (bağlı kart dahil)
	for _, k := range km.keys {
		k.IsExhausted = true
	}
	// Tükenmiş hâlde oturum bağlı kartında kalır (upstream429 hakem)
	stuck, err := km.KeyForSession("sess-r")
	if err != nil || stuck.ID != first.ID {
		t.Fatalf("havuz boşken bağ korunmalı: %v / %s", err, stuck.ID)
	}
	// Yeni gün: 03:00 sıfırlaması (bugün değil, yarın)
	loc := km.turkeyLoc
	now := time.Now().In(loc)
	tomorrow := now.Add(30 * time.Hour)
	if !km.checkAndResetDaily(tomorrow) {
		t.Fatal("yeni güne geçişte sıfırlama tetiklenmeli")
	}
	// Kartlar yeniden havuzda + oturum AYNI anahtarda kalmış (bağ ezilmedi)
	after, err := km.KeyForSession("sess-r")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != first.ID {
		t.Fatalf("sıfırlama bağı ezmemeli: %s ≠ %s", after.ID, first.ID)
	}
	for _, k := range km.keys {
		if k.IsExhausted || k.UsedDaily != 0 {
			t.Fatalf("sıfırlama sonrası kartlar temiz olmalı: %s exh=%v used=%d", k.ID, k.IsExhausted, k.UsedDaily)
		}
	}
	// Kalıcılık: sıfırlamadan SONRAKİ kayıt da doğru (yeni güne ait last_reset_date)
	km2, err := NewKeyManager(writePoolFixtureReuse(t, km))
	if err != nil {
		t.Fatal(err)
	}
	if k, err := km2.KeyForSession("sess-r"); err != nil || k.ID != first.ID {
		t.Fatalf("sıfırlama sonrası restart'ta da bağ sürmeli: %v / %s", err, k.ID)
	}
}

// writePoolFixtureReuse, çalışan bir KeyManager'ın O ANKİ durumunu yeni bir
// geçici dosyaya yazar (yeniden başlatma benzetimi).
func writePoolFixtureReuse(t *testing.T, km *KeyManager) string {
	t.Helper()
	km.mu.Lock()
	state := KeysState{
		Keys:              km.keys,
		LastResetDate:     km.lastResetDate,
		AutoSwitchEnabled: poolEnabled(km.autoSwitch),
		SessionKeys:       km.bindings,
	}
	km.mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestKeyPool_SilinmisAnahtaraBagAtılır, bir kart silinip dosya yeniden
// yüklendiğinde o karta giden bağın ATILDIĞINI ve oturumun havuzdan
// yeniden dağıtıldığını kanıtlar.
func TestKeyPool_SilinmisAnahtaraBagAtılır(t *testing.T) {
	path := writePoolFixture(t, 3, true)
	km, err := NewKeyManager(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := km.KeyForSession("sess-del")
	if err != nil {
		t.Fatal(err)
	}
	// Dosyadan KART sil (bağ dosyada kalsın): keys.json'u elle yeniden yaz
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state KeysState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	kept := state.Keys[:0]
	for _, k := range state.Keys {
		if k.ID != first.ID {
			kept = append(kept, k)
		}
	}
	state.Keys = kept
	data, _ = json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	km2, err := NewKeyManager(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := km2.KeyForSession("sess-del")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID == first.ID {
		t.Fatalf("silinmiş anahtara bağlı kalmamalı: %s", again.ID)
	}
	// Geçersiz bağ atıldığı için yeni dağıtım kalıcıdır
	if got := km2.bindings["sess-del"]; got != again.ID {
		t.Fatalf("bağ temizlenip yeniden yazılmalı: %s ≠ %s", got, again.ID)
	}
}

// TestKeyPool_SifirRootAktif, gövdesiz/oturumsuz yolların (models, gömülü)
// her koşulda aktif anahtarı aldığını kanıtlar.
func TestKeyPool_SifirRootAktif(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 2, true))
	if err != nil {
		t.Fatal(err)
	}
	// Dağıtım yapılmadan ÖNCE boş root aktifi vermeli
	k, err := km.KeyForSession("")
	if err != nil {
		t.Fatal(err)
	}
	if !k.IsActive || k.ID != "key-1" {
		t.Fatalf("boş root aktif anahtarı almalı: %s active=%v", k.ID, k.IsActive)
	}
	// Dağıtım YAPILDIKTAN SONRA da boş root aktifi verir (bağı bozmaz)
	if _, err := km.KeyForSession("sess-z"); err != nil {
		t.Fatal(err)
	}
	k2, err := km.KeyForSession("")
	if err != nil {
		t.Fatal(err)
	}
	if k2.ID != "key-1" {
		t.Fatalf("boş root dağıtımdan etkilenmemeli: %s", k2.ID)
	}
}

// TestKeyPool_BoundKeyVeHavuzOzeti, panel görselleri için okuma-only
// BoundKey'nin atama YAPMADIĞINI ve PoolStats'un bağlı oturum sayısını +
// kullanılabilirliği doğru verdiğini kanıtlar (havuz widget'ının kaynağı).
func TestKeyPool_BoundKeyVeHavuzOzeti(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	// Bağsız oturum: ok=false, atama yok
	if _, _, ok := km.BoundKey("sess-yok"); ok {
		t.Fatal("bağsız oturum ok=false vermeli")
	}
	if _, _, ok := km.BoundKey(""); ok {
		t.Fatal("boş kök ok=false vermeli")
	}

	k1, err := km.KeyForSession("sess-x")
	if err != nil {
		t.Fatal(err)
	}
	id, name, ok := km.BoundKey("sess-x")
	if !ok || id != k1.ID || name == "" {
		t.Fatalf("BoundKey bağlı anahtarı dönmeli: id=%s name=%q ok=%v", id, name, ok)
	}
	// Okuma çağrısı ATAMA YAPMAZ
	if _, _, ok := km.BoundKey("sess-okuma-atama"); ok {
		t.Fatal("BoundKey atama yapmamalı")
	}

	stats := km.PoolStats()
	if stats[k1.ID].BoundSessions != 1 {
		t.Fatalf("karta1 oturum bağlı olmalı: %+v", stats[k1.ID])
	}
	if !stats[k1.ID].Usable || !stats[k1.ID].PoolMember {
		t.Fatalf("bağlı kart hâlâ kullanılabilir olmalı: %+v", stats[k1.ID])
	}
	empty := 0
	for _, st := range stats {
		if st.BoundSessions == 0 && st.Usable {
			empty++
		}
	}
	if empty != 2 {
		t.Fatalf("boşta2 kart olmalı: %d", empty)
	}

	// Tüketim → kart kullanılamaz hâle gelir (özette de görünür)
	for _, k := range km.keys {
		if k.ID == k1.ID {
			k.IsExhausted = true
		}
	}
	stats2 := km.PoolStats()
	if stats2[k1.ID].Usable || !stats2[k1.ID].Exhausted {
		t.Fatalf("tükenmiş kart usable=false olmalı: %+v", stats2[k1.ID])
	}
}

// TestKeyPool_OturumSilininceAnahtarBoşaDüşer, oturum logları silinince
// (Unbind) kartın havuza GERİ verildiğini ve UnbindAll ile havuzun TAMAMEN
// boşa düştüğünü kanıtlar — panel "temizle" düğmelerinin arkasındaki mekanizma.
func TestKeyPool_OturumSilininceAnahtarBoşaDüşer(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	a, err := km.KeyForSession("sess-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := km.KeyForSession("sess-b")
	if err != nil {
		t.Fatal(err)
	}
	stats := km.PoolStats()
	if stats[a.ID].BoundSessions != 1 || stats[b.ID].BoundSessions != 1 {
		t.Fatalf("ikişer bağ beklenir: a=%d b=%d", stats[a.ID].BoundSessions, stats[b.ID].BoundSessions)
	}

	// Tek oturum silindi → o bağ koptu, diğeri duruyor
	if !km.Unbind("sess-a") {
		t.Fatal("bağlı oturum kopmalı")
	}
	if km.Unbind("sess-a") {
		t.Fatal("ikinci koparma false olmalı")
	}
	if _, _, ok := km.BoundKey("sess-a"); ok {
		t.Fatal("kopan bağ görünmemeli")
	}
	total := 0
	for _, st := range km.PoolStats() {
		total += st.BoundSessions
	}
	if total != 1 {
		t.Fatalf("tek bağ kalmalı: %d", total)
	}

	// Tüm oturumlar silindi → havuz tamamen boşa
	if n := km.UnbindAll(); n != 1 {
		t.Fatalf("kopan bağ1 olmalı: %d", n)
	}
	for _, st := range km.PoolStats() {
		if st.BoundSessions != 0 {
			t.Fatalf("havuz tamamen boşa düşmeli: %+v", st)
		}
	}

	// Kalıcılık: boş bağ hâli keys.json'a yazıldı
	km2, err := NewKeyManager(writePoolFixtureReuse(t, km))
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range km2.PoolStats() {
		if st.BoundSessions != 0 {
			t.Fatalf("restart'ta da havuz boş kalmalı: %+v", st)
		}
	}
}

// TestKeyPool_SubagentİlkTurdanKökAnahtarınıAlır, proxy'nin ERKEN soy bağını
// kanıtlar: parent başlığı geldiğinde bağ anahtar ATAMADAN ÖNCE kurulur →
// subagent'ın ilk isteği de kökün KARTINI alır, yetim bağ oluşmaz.
func TestKeyPool_SubagentİlkTurdanKökAnahtarınıAlır(t *testing.T) {
	km, err := NewKeyManager(writePoolFixture(t, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	rootID := "kok-oturum-test"
	rootKey, err := km.KeyForSession(rootID)
	if err != nil {
		t.Fatal(err)
	}

	// Proxy erken bağ kurar (subagent'ın ilk isteğinden hemen önce)
	childID := "cocuk-oturum-test"
	chatSessions.SetParent(childID, rootID)
	if got := chatSessions.Root(childID); got != rootID {
		t.Fatalf("çocuğun kökü kok olmalı: %q", got)
	}
	// İlk istek BİLE kökün kartını alır
	k, err := km.KeyForSession(chatSessions.Root(childID))
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != rootKey.ID {
		t.Fatalf("subagent ilk turdan kök kartını almalı: %s ≠ %s", k.ID, rootKey.ID)
	}
	// Yetim bağ yok: anahtar hâlâ YALNIZCA köke bağlı
	st := km.PoolStats()
	if st[rootKey.ID].BoundSessions != 1 {
		t.Fatalf("yalnızca kök bağlanmalı (çocuk yetim değil): %+v", st[rootKey.ID])
	}
}
