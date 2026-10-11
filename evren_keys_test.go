package main

import (
	"os"
	"testing"
	"time"
)

func TestKeyManager_Rotation(t *testing.T) {
	tempFile := "test_keys.json"
	defer os.Remove(tempFile)

	km, err := NewKeyManager(tempFile)
	if err != nil {
		t.Fatalf("NewKeyManager hatası: %v", err)
	}

	// 2 adet key ekle
	k1, _ := km.AddKey("Key 1", "evren-key-1", 1000, 100) // 900'de devret
	k2, _ := km.AddKey("Key 2", "evren-key-2", 1000, 100)
	_ = km.SetActiveKey(k1.ID)

	active, err := km.GetActiveKey()
	if err != nil || active.ID != k1.ID {
		t.Fatalf("İlk aktif anahtar beklenen %s, alınan %v", k1.ID, active)
	}

	// 500 token düş
	usage1 := CalculateUsage(500, 0, 0)
	nextKey, rotated, err := km.DeductUsage(k1.ID, usage1, "net")
	if err != nil || rotated {
		t.Fatalf("500 token sonrası rotasyon olmamalıydı")
	}

	// 450 token daha düş (toplam 950 >= 1000-100=900 threshold)
	usage2 := CalculateUsage(450, 0, 0)
	nextKey, rotated, err = km.DeductUsage(k1.ID, usage2, "net")
	if err != nil {
		t.Fatalf("DeductUsage hatası: %v", err)
	}
	if !rotated {
		t.Fatalf("Limit eşiği aşıldığı için rotasyon bekleniyordu!")
	}
	if nextKey == nil || nextKey.ID != k2.ID {
		t.Fatalf("Rotasyon sonrası beklenen key %s, alınan %v", k2.ID, nextKey)
	}

	// Yeni GetActiveKey çağrıldığında k2 dönmeli
	activeNow, err := km.GetActiveKey()
	if err != nil || activeNow.ID != k2.ID {
		t.Fatalf("Yeni aktif anahtar %s olmalıydı, alınan %v", k2.ID, activeNow)
	}
}

func TestKeyManager_ResetDateCalculation(t *testing.T) {
	tempFile := "test_reset_keys.json"
	defer os.Remove(tempFile)

	km, err := NewKeyManager(tempFile)
	if err != nil {
		t.Fatalf("NewKeyManager hatası: %v", err)
	}

	loc := time.FixedZone("UTC+3", 3*60*60)

	// 2026-09-28 saat 02:30 (gece 03:00'ten önce)
	tBefore := time.Date(2026, 9, 28, 2, 30, 0, 0, loc)
	dayBefore := km.currentResetDayString(tBefore)
	if dayBefore != "2026-09-27" {
		t.Errorf("03:00 öncesi için dönem '2026-09-27' olmalıydı, alınan %s", dayBefore)
	}

	// 2026-09-28 saat 03:05 (gece 03:00'ten sonra)
	tAfter := time.Date(2026, 9, 28, 3, 5, 0, 0, loc)
	dayAfter := km.currentResetDayString(tAfter)
	if dayAfter != "2026-09-28" {
		t.Errorf("03:00 sonrası için dönem '2026-09-28' olmalıydı, alınan %s", dayAfter)
	}
}

// TestKeyManager_GetActiveKeySaltOkuma "kendiliğinden anahtar değişme" bug'ının
// regresyonu: GetActiveKey hiçbir koşulda (limit aşılmış, boş anahtar,
// çoklu sorgu) aktif anahtarı değiştirmez.
func TestKeyManager_GetActiveKeySaltOkuma(t *testing.T) {
	tempFile := "test_pure_keys.json"
	defer os.Remove(tempFile)

	km, err := NewKeyManager(tempFile)
	if err != nil {
		t.Fatalf("NewKeyManager hatası: %v", err)
	}

	k1, _ := km.AddKey("Key 1", "evren-key-1", 1000, 100)
	_ = km.SetActiveKey(k1.ID)
	km.SetAutoSwitch(false) // oto geçiş KAPALI: limit aşımı geçiş üretmemeli

	usage := CalculateUsage(950, 0, 0) // 950 >= 1000-100
	if _, rotated, err := km.DeductUsage(k1.ID, usage, "net"); err != nil || rotated {
		t.Fatalf("oto geçiş kapalıyken limit aşımı rotasyon üretmemeli (rotated=%v, err=%v)", rotated, err)
	}

	for i := range 5 {
		active, err := km.GetActiveKey()
		if err != nil || active.ID != k1.ID {
			t.Fatalf("sorgu %d: GetActiveKey aktif anahtarı değiştirmemeli (err=%v, active=%v)", i, err, active)
		}
	}

	// 429 artık hiçbir koşulda geçiş tetiklemez (tek tetik: geçiş eşiği);
	// yalnızca hata sayacı artar, aktif anahtar sabit kalır.
	km.RecordError(k1.ID)

	for _, k := range km.GetAllKeys() {
		if k.IsActive && k.ID != k1.ID {
			t.Fatalf("aktif anahtar izinsiz değişmiş: %s", k.ID)
		}
	}
}

// TestKeyManager_BosAnahtarAtlamaz boş değerli aktif anahtarın "sıradakine
// atlama" yapmadığını, yalnızca açıklayıcı hata döndürdüğünü doğrular.
func TestKeyManager_BosAnahtarAtlamaz(t *testing.T) {
	tempFile := "test_empty_key.json"
	defer os.Remove(tempFile)

	km, err := NewKeyManager(tempFile)
	if err != nil {
		t.Fatalf("NewKeyManager hatası: %v", err)
	}
	kEmpty, _ := km.AddKey("Boş", "", 10000000, 50000)
	_, _ = km.AddKey("Dolu", "dolu-deger", 10000000, 50000)
	_ = km.SetActiveKey(kEmpty.ID)

	if _, err := km.GetActiveKey(); err == nil {
		t.Fatal("boş anahtar için açıklayıcı hata bekleniyordu")
	}
	for _, k := range km.GetAllKeys() {
		if k.IsActive && k.ID != kEmpty.ID {
			t.Fatalf("boş anahtar sorgusunda aktif değişmemeli: %s", k.ID)
		}
	}
}

// TestKeyManager_HavuzKisiti geçişlerin yalnızca oto geçiş havuzuna seçilmiş
// kartlar arasında yapıldığını kanıtlar.
func TestKeyManager_HavuzKisiti(t *testing.T) {
	tempFile := "test_pool_keys.json"
	defer os.Remove(tempFile)

	km, err := NewKeyManager(tempFile)
	if err != nil {
		t.Fatalf("NewKeyManager hatası: %v", err)
	}
	k1, _ := km.AddKey("K1", "key-value-1", 1000, 100)
	k2, _ := km.AddKey("K2", "key-value-2", 1000, 100)
	k3, _ := km.AddKey("K3", "key-value-3", 1000, 100)
	_ = km.SetActiveKey(k1.ID)

	km.SetAutoSwitch(true)
	// Havuz: k1 + k2 (k3 hariç)
	if err := km.SetAutoSwitchMember(k3.ID, false); err != nil {
		t.Fatalf("havuz üyeliği: %v", err)
	}

	usage := CalculateUsage(950, 0, 0)
	next, rotated, err := km.DeductUsage(k1.ID, usage, "net")
	if err != nil || !rotated {
		t.Fatalf("k1 limit aşımında havuz üyesi k2'ye geçiş bekleniyordu (rotated=%v, err=%v)", rotated, err)
	}
	if next == nil || next.ID != k2.ID {
		t.Fatalf("geçilecek anahtar k2 olmalı, alındı: %v", next)
	}

	// k2 aşıldığında havuzda uygun kimse kalmaz (k1 zaten limit üstü) → geçiş yok
	usage2 := CalculateUsage(950, 0, 0)
	_, rotated2, err := km.DeductUsage(k2.ID, usage2, "net")
	if err != nil {
		t.Fatalf("DeductUsage: %v", err)
	}
	if rotated2 {
		t.Fatal("havuzda uygun kart yokken geçiş yapılmamalı")
	}
	active, err := km.GetActiveKey()
	if err != nil || active.ID != k2.ID {
		t.Fatalf("aktif k2 kalmalı, alındı: %v (err=%v)", active, err)
	}

	// Sıralı numaralar havuz üyeliğine göre dinamik atanmalı
	all := km.GetAllKeys()
	orderSeen := map[int]string{}
	for _, k := range all {
		if k.AutoSwitch {
			if k.AutoSwitchOrder == 0 {
				t.Fatalf("havuz üyesinde sıra numarası 0 olamaz: %s", k.Name)
			}
			if prev, dup := orderSeen[k.AutoSwitchOrder]; dup {
				t.Fatalf("sıra numarası çakışması: %d hem %s hem %s", k.AutoSwitchOrder, prev, k.Name)
			}
			orderSeen[k.AutoSwitchOrder] = k.Name
		} else if k.AutoSwitchOrder != 0 {
			t.Fatalf("havuz dışı kartın sıra numarası 0 olmalı: %s -> %d", k.Name, k.AutoSwitchOrder)
		}
	}
	if len(orderSeen) != 2 {
		t.Fatalf("havuzda 2 kart olmalı (k1, k2), alındı: %d", len(orderSeen))
	}
	if orderSeen[1] != "K1" || orderSeen[2] != "K2" {
		t.Fatalf("dinamik sıra K1=1, K2=2 olmalı, alındı: %+v", orderSeen)
	}
}

// TestKeyManager_ReasoningAyrimiVeGunlukSifirlama reasoning sayacının günlük
// toplandığını, gün bitiminde (ResetDailyForce = 03:00 sıfırması ile aynı
// döngü) sıfırlandığını ve total sayacın korunduğunu doğrular.
func TestKeyManager_ReasoningAyrimiVeGunlukSifirlama(t *testing.T) {
	tempFile := "test_reasoning_keys.json"
	defer os.Remove(tempFile)

	km, err := NewKeyManager(tempFile)
	if err != nil {
		t.Fatalf("NewKeyManager hatası: %v", err)
	}
	k1, _ := km.AddKey("K", "deger-1", 10000000, 50000)

	usage := CalculateUsage(100, 0, 40)
	usage.ReasoningTokens = 15
	if _, _, err := km.DeductUsage(k1.ID, usage, "net"); err != nil {
		t.Fatalf("DeductUsage: %v", err)
	}

	var daily, total int64
	for _, k := range km.GetAllKeys() {
		if k.ID == k1.ID {
			daily, total = k.ReasoningDaily, k.ReasoningTotal
		}
	}
	if daily != 15 || total != 15 {
		t.Fatalf("reasoning günlük=%d total=%d, ikisi de 15 olmalı", daily, total)
	}

	km.ResetDailyForce() // gün bitimi sıfırlaması

	for _, k := range km.GetAllKeys() {
		if k.ID != k1.ID {
			continue
		}
		if k.ReasoningDaily != 0 {
			t.Fatalf("gün bitiminde reasoning_daily sıfırlanmalı, alındı: %d", k.ReasoningDaily)
		}
		if k.ReasoningTotal != 15 {
			t.Fatalf("reasoning_total sıfırlanmamalı, alındı: %d", k.ReasoningTotal)
		}
	}
}

// TestKeyManager_GunlukLimit429Gecisi yalnızca code=daily_token_limit_exceeded
// için çağrılan RotateAfterDailyLimit'in oto geçiş KAPALIYKEN çalışmadığını,
// AÇIKKEN hedefi exhausted yapıp aktifi temizleyerek sıradaki karta geçtiğini doğrular.
func TestKeyManager_GunlukLimit429Gecisi(t *testing.T) {
	tempFile := "test_daily429_keys.json"
	defer os.Remove(tempFile)

	km, err := NewKeyManager(tempFile)
	if err != nil {
		t.Fatalf("NewKeyManager hatası: %v", err)
	}
	k1, _ := km.AddKey("K1", "deger-1", 10000000, 50000)
	k2, _ := km.AddKey("K2", "deger-2", 10000000, 50000)
	_ = km.SetActiveKey(k1.ID)

	km.SetAutoSwitch(false) // kapalı: geçiş yasak
	if _, rotated := km.RotateAfterDailyLimit(k1.ID); rotated {
		t.Fatal("oto geçiş kapalıyken günlük 429 rotasyonu yapılmamalı")
	}
	if active, _ := km.GetActiveKey(); active.ID != k1.ID {
		t.Fatalf("aktif k1 kalmalı, alındı: %s", active.ID)
	}

	km.SetAutoSwitch(true) // açık: geçiş yapılmalı
	next, rotated := km.RotateAfterDailyLimit(k1.ID)
	if !rotated || next == nil || next.ID != k2.ID {
		t.Fatalf("k2'ye geçiş bekleniyordu (rotated=%v, next=%v)", rotated, next)
	}
	active, _ := km.GetActiveKey()
	if active.ID != k2.ID {
		t.Fatalf("aktif k2 olmalı, alındı: %s", active.ID)
	}
	for _, k := range km.GetAllKeys() {
		if k.ID == k1.ID && !k.IsExhausted {
			t.Fatal("429 yiyen kart IsExhausted işaretlenmeli (gün sıfırlamasına kadar aday dışı)")
		}
		if k.ID == k2.ID && !k.IsActive {
			t.Fatal("yeni aktif kart yalnızca k2 olmalı")
		}
	}
}

// TestKeyManager_EmbeddingKotadanDusur EVREN mekanizmasını kanıtlar: embedding
// token'ları KOTADAN ham düşülür (net formülüne girmez), günlük/toplam kotaya
// yazılır, geçiş TETİKLEMEZ ve gün bitiminde günlük sayaçlar sıfırlanır.
func TestKeyManager_EmbeddingKotadanDusur(t *testing.T) {
	tempFile := "test_embedding_kota.json"
	defer os.Remove(tempFile)

	km, err := NewKeyManager(tempFile)
	if err != nil {
		t.Fatalf("NewKeyManager hatası: %v", err)
	}
	k1, _ := km.AddKey("K", "deger-1", 10000000, 50000)
	_ = km.SetActiveKey(k1.ID)
	km.SetAutoSwitch(true)

	km.RecordEmbeddingUsage(k1.ID, 1234)

	if active, _ := km.GetActiveKey(); active == nil || active.ID != k1.ID {
		t.Fatal("embedding kullanımı geçiş TETİKLEMEMELİ (hesaplamaya girmez)")
	}
	for _, k := range km.GetAllKeys() {
		if k.ID != k1.ID {
			continue
		}
		if k.UsedDaily != 1234 {
			t.Fatalf("embedding kotadan ham düşmeli: UsedDaily=1234 beklenen, alındı %d", k.UsedDaily)
		}
		if k.UsedTotal != 1234 {
			t.Fatalf("embedding toplam kotaya ham düşmeli: UsedTotal=1234 beklenen, alındı %d", k.UsedTotal)
		}
		if k.EmbeddingTokensDaily != 1234 || k.EmbeddingTokensTotal != 1234 {
			t.Fatalf("embedding sayaçları yazılmalı: daily=%d total=%d", k.EmbeddingTokensDaily, k.EmbeddingTokensTotal)
		}
	}

	km.ResetDailyForce() // gün bitimi

	for _, k := range km.GetAllKeys() {
		if k.ID != k1.ID {
			continue
		}
		if k.UsedDaily != 0 || k.EmbeddingTokensDaily != 0 {
			t.Fatalf("gün bitiminde günlük sayaçlar sıfırlanmalı: kota=%d emb=%d", k.UsedDaily, k.EmbeddingTokensDaily)
		}
		if k.UsedTotal != 1234 {
			t.Fatalf("toplam kota sıfırlanmamalı: %d", k.UsedTotal)
		}
	}
}

// TestKeyManager_GunlukSifirlamaAktifiDegistirmez 03:00 sıfırlamasının (ve
// dolayısıyla açılış kontrolünün) aktif anahtarı keys[0]'a sıçratmamasını doğrular.
func TestKeyManager_GunlukSifirlamaAktifiDegistirmez(t *testing.T) {
	tempFile := "test_reset_active.json"
	defer os.Remove(tempFile)

	km, err := NewKeyManager(tempFile)
	if err != nil {
		t.Fatalf("NewKeyManager hatası: %v", err)
	}
	k3, _ := km.AddKey("K3", "deger-3", 1000, 100)
	_ = km.SetActiveKey(k3.ID)

	km.ResetDailyForce()

	active, err := km.GetActiveKey()
	if err != nil || active.ID != k3.ID {
		t.Fatalf("sıfırlama sonrası aktif k3 kalmalı, alındı: %v (err=%v)", active, err)
	}
}
