package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

type APIKey struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Key             string `json:"key"`
	DailyLimit      int64  `json:"daily_limit"`      // Varsayılan: 10.000.000 (10M)
	ThresholdTokens int64  `json:"threshold_tokens"` // Bu limite ulaşınca veya bu kadar kalınca devret

	// GÜNLÜK CHAT METRİKLERİ (Gece 03:00 TSİ'de sıfırlanır)
	UsedDaily       int64  `json:"used_daily"`       // Günlük kotadan düşen net token
	PromptDaily     int64  `json:"prompt_daily"`     // Günlük girdi token toplamı
	CachedDaily     int64  `json:"cached_daily"`     // Günlük önbellek token toplamı
	CompletionDaily int64  `json:"completion_daily"` // Günlük çıktı token toplamı
	ReasoningDaily  int64  `json:"reasoning_daily"`  // Günlük reasoning token (03:00 TSİ'de sıfırlanır)
	RequestsDaily   int64  `json:"requests_daily"`   // Günlük başarılı chat istek sayısı

	// GÜNLÜK EMBEDDING METRİKLERİ (03:00 TSİ'de sıfırlanır, 10M chat kotasından DÜŞMEZ)
	EmbeddingTokensDaily   int64 `json:"embedding_tokens_daily"`
	EmbeddingRequestsDaily int64 `json:"embedding_requests_daily"`

	// GÜNLÜK SES/ASR METRİKLERİ (03:00 TSİ'de sıfırlanır; chat net formülüne
	// girmez, embedding gibi ham düşülür). AudioSeconds, upstream ses süresi
	// bildirdiğinde dolar (yoksa 0 kalır).
	AudioTokensDaily   int64 `json:"audio_tokens_daily"`
	AudioRequestsDaily int64 `json:"audio_requests_daily"`
	AudioSecondsDaily  int64 `json:"audio_seconds_daily"`

	// GÜNLÜK KARAR (DECISION) METRİKLERİ — /v1/systemone (clef, laya).
	// Girdi token'ı modele giden state+sorular+görsellerdir (görseller karo
	// karo sayılır: 32×32 px = 1 token). Çıktı metni yoktur (output_tokens=0).
	DecisionTokensDaily   int64 `json:"decision_tokens_daily"`
	DecisionRequestsDaily int64 `json:"decision_requests_daily"`

	// TÜM ZAMANLAR CHAT (KÜMÜLATİF / ASLA SIFIRLANMAYAN TOTAL METRİKLER)
	UsedTotal       int64  `json:"used_total"`       // Toplam net harcanan token
	PromptTotal     int64  `json:"prompt_total"`     // Toplam girdi token
	CachedTotal     int64  `json:"cached_total"`     // Toplam önbellekten okunan token
	CompletionTotal int64  `json:"completion_total"` // Toplam çıktı token
	ReasoningTotal  int64  `json:"reasoning_total"`  // Toplam reasoning token (sıfırlanmaz)
	RequestsTotal   int64  `json:"requests_total"`   // Toplam başarılı istek sayısı
	ErrorsTotal     int64  `json:"errors_total"`     // Toplam hatalı istek sayısı

	// TÜM ZAMANLAR EMBEDDING (KÜMÜLATİF TOTAL)
	EmbeddingTokensTotal   int64 `json:"embedding_tokens_total"`
	EmbeddingRequestsTotal int64 `json:"embedding_requests_total"`

	// TÜM ZAMANLAR SES/ASR (KÜMÜLATİF TOTAL)
	AudioTokensTotal   int64 `json:"audio_tokens_total"`
	AudioRequestsTotal int64 `json:"audio_requests_total"`
	AudioSecondsTotal  int64 `json:"audio_seconds_total"`

	// TÜM ZAMANLAR KARAR (KÜMÜLATİF TOTAL)
	DecisionTokensTotal   int64 `json:"decision_tokens_total"`
	DecisionRequestsTotal int64 `json:"decision_requests_total"`

	IsActive        bool   `json:"is_active"`        // Şu an istekleri karşılayan aktif anahtar mı
	IsExhausted     bool   `json:"is_exhausted"`     // Günlük 10M kotası doldu mu
	AutoSwitch      bool   `json:"auto_switch"`      // Oto geçiş havuzu üyesi mi (roza çerçeveli kartlar)
	AutoSwitchOrder int    `json:"auto_switch_order"` // Oto geçiş rotasyon sırası (1..N; havuzda değilse 0)
	CreatedAt       string `json:"created_at"`
	LastUsedAt      string `json:"last_used_at"`
}

type KeysState struct {
	Keys          []*APIKey `json:"keys"`
	LastResetDate string    `json:"last_reset_date"` // YYYY-MM-DD formatında son sıfırlama günü (03:00 TSİ referansı)
	// nil = eski sürüm dosyası: tüm anahtarlar havuza dahil kabul edilir (geriye dönük uyumluluk).
	AutoSwitchEnabled *bool `json:"auto_switch_enabled,omitempty"`
	// SessionKeys, istek bazlı dinamik dağıtımın kalıcı bağları:
	// oturum kökü (Root(session)) → anahtar id.03:00 sıfırlamasında
	// anahtarlar yenilenir, bağlar KORUNUR (oturum yerinde kalır).
	SessionKeys map[string]string `json:"session_keys,omitempty"`
}

type KeyManager struct {
	mu            sync.RWMutex
	filePath      string
	keys          []*APIKey
	lastResetDate string
	turkeyLoc     *time.Location
	onStateChange func()

	// bindings, istek bazlı dinamik dağıtım: oturum kökü → anahtar id.
	// Yeni oturum havuzdan anahtar alır; anahtarın kotası bitince oturum
	// HEMEN havuzdan yenisine taşınır; subagent'lar Root() ile kök oturumun
	// anahtarını paylaşır.
	bindings map[string]string

	// autoSwitch, API'ler arası otomatik geçişin ana anahtarıdır: kapalıyken
	// hiçbir kod yolu (eşik, 429, boş anahtar) bir sonraki anahtara atlamaz.
	autoSwitch bool
}

func NewKeyManager(filePath string) (*KeyManager, error) {
	loc := time.FixedZone("UTC+3", 3*60*60) // Türkiye Saati (TSİ)
	km := &KeyManager{
		filePath:   filePath,
		turkeyLoc:  loc,
		bindings:   make(map[string]string),
		autoSwitch: true, // varsayılan: oto geçiş açık (keys.json alanı yoksa bu kalır)
	}

	if err := km.load(); err != nil {
		return nil, err
	}

	// Başlangıçta 03:00 TSİ kontrolü yap ve sıfırlanması gerekiyorsa sıfırla
	km.checkAndResetDaily(time.Now().In(loc))

	// Arka plan zamanlayıcısını başlat
	km.startDailyResetWorker()

	return km, nil
}

func (km *KeyManager) load() error {
	km.mu.Lock()
	defer km.mu.Unlock()

	data, err := os.ReadFile(km.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			km.keys = []*APIKey{
				{
					ID:              "key-1",
					Name:            "EVREN API Anahtarı 1 (Birincil)",
					Key:             "",
					DailyLimit:      10000000,
					ThresholdTokens: 50000,
					IsActive:        true,
					CreatedAt:       time.Now().In(km.turkeyLoc).Format(time.RFC3339),
				},
				{
					ID:              "key-2",
					Name:            "EVREN API Anahtarı 2 (İkincil)",
					Key:             "",
					DailyLimit:      10000000,
					ThresholdTokens: 50000,
					IsActive:        false,
					CreatedAt:       time.Now().In(km.turkeyLoc).Format(time.RFC3339),
				},
				{
					ID:              "key-3",
					Name:            "EVREN API Anahtarı 3 (Üçüncül)",
					Key:             "",
					DailyLimit:      10000000,
					ThresholdTokens: 50000,
					IsActive:        false,
					CreatedAt:       time.Now().In(km.turkeyLoc).Format(time.RFC3339),
				},
				{
					ID:              "key-4",
					Name:            "EVREN API Anahtarı 4 (Dördüncül)",
					Key:             "",
					DailyLimit:      10000000,
					ThresholdTokens: 50000,
					IsActive:        false,
					CreatedAt:       time.Now().In(km.turkeyLoc).Format(time.RFC3339),
				},
			}
			km.lastResetDate = km.currentResetDayString(time.Now().In(km.turkeyLoc))
			for _, k := range km.keys {
				k.AutoSwitch = false // taze kurulumda boş tohum anahtarlar havuz dışı; kullanıcı doldurup seçer
			}
			return km.saveLocked()
		}
		return err
	}

	var state KeysState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	km.keys = state.Keys
	km.lastResetDate = state.LastResetDate

	// Geriye dönük uyumluluk: eski keys.json'larda oto geçiş alanı yoksa özellik
	// açık kabul edilir ve MEVCUT TÜM anahtarlar havuza dahil edilir; böylece
	// yükseltme sonrası mevcut otomatik rotasyon davranışı aynen sürer.
	if state.AutoSwitchEnabled == nil {
		km.autoSwitch = true
		for _, k := range km.keys {
			k.AutoSwitch = true
		}
	} else {
		km.autoSwitch = *state.AutoSwitchEnabled
	}

	// Oturum→anahtar bağları: silinmiş/ bilinmeyen anahtara giden bağ
	// atılır — oturum havuzdan yeniden dağıtılmak üzere sıfırdan kalır.
	km.bindings = make(map[string]string, len(state.SessionKeys))
	for root, keyID := range state.SessionKeys {
		if root == "" || keyID == "" {
			continue
		}
		valid := false
		for _, k := range km.keys {
			if k.ID == keyID {
				valid = true
				break
			}
		}
		if valid {
			km.bindings[root] = keyID
		}
	}

	// NOT: dosyada aktif anahtar yoksa keys[0] ZORLA AKTİF yapılmaz (eski
	// davranış kaldırıldı). Aktif anahtar yalnızca geçiş eşiği rotasyonu veya
	// manuel SetActiveKey ile değişir.
	return nil
}

func (km *KeyManager) saveLocked() error {
	autoSwitch := km.autoSwitch
	state := KeysState{
		Keys:              km.keys,
		LastResetDate:     km.lastResetDate,
		AutoSwitchEnabled: &autoSwitch,
		SessionKeys:       km.bindings,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(km.filePath, data, 0644)
}

func (km *KeyManager) Save() error {
	km.mu.Lock()
	defer km.mu.Unlock()
	return km.saveLocked()
}

func (km *KeyManager) currentResetDayString(t time.Time) string {
	shifted := t.Add(-3 * time.Hour)
	return shifted.Format("2006-01-02")
}

func (km *KeyManager) NextResetTime() time.Time {
	now := time.Now().In(km.turkeyLoc)
	today3AM := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, km.turkeyLoc)
	if now.Before(today3AM) {
		return today3AM
	}
	return today3AM.Add(24 * time.Hour)
}

func (km *KeyManager) checkAndResetDaily(now time.Time) bool {
	km.mu.Lock()
	defer km.mu.Unlock()

	currentDay := km.currentResetDayString(now)
	if km.lastResetDate == "" || km.lastResetDate != currentDay {
		log.Printf("[RESET] Günlük 03:00 TSİ sıfırlaması tetiklendi! Eski dönem: %s -> Yeni dönem: %s", km.lastResetDate, currentDay)
		for _, k := range km.keys {
			k.UsedDaily = 0
			k.PromptDaily = 0
			k.CachedDaily = 0
			k.CompletionDaily = 0
			k.ReasoningDaily = 0
			k.RequestsDaily = 0
			k.EmbeddingTokensDaily = 0
			k.EmbeddingRequestsDaily = 0
			k.AudioTokensDaily = 0
			k.AudioRequestsDaily = 0
			k.AudioSecondsDaily = 0
			k.DecisionTokensDaily = 0
			k.DecisionRequestsDaily = 0
			k.IsExhausted = false
		}
		// Günlük sıfırlama AKTİF ANAHTARI DEĞİŞTİRMEZ: eski sürümde bu döngü
		// (gece 03:00 worker'ı ve açılış kontrolü) aktif anahtarı zorla
		// ilk anahtara sıçratıyordu — isteksiz "kendiliğinden değişme"nin ikinci kaynağı.
		km.lastResetDate = currentDay
		_ = km.saveLocked()
		return true
	}
	return false
}

func (km *KeyManager) startDailyResetWorker() {
	go func() {
		for {
			nextReset := km.NextResetTime()
			sleepDuration := time.Until(nextReset)
			if sleepDuration <= 0 {
				sleepDuration = 1 * time.Second
			}

			log.Printf("[RESET WORKER] Bir sonraki 03:00 TSİ sıfırlamasına kalan süre: %v (Hedef: %s)",
				sleepDuration.Round(time.Second), nextReset.Format("2006-01-02 15:04:05 MST"))

			time.Sleep(sleepDuration + 500*time.Millisecond)
			km.checkAndResetDaily(time.Now().In(km.turkeyLoc))
		}
	}()
}

// GetActiveKey aktif anahtarı SALT-OKUNUR döndürür; içinde rotasyon yoktur.
// KURAL (tek tetik): aktif anahtarı DEĞİŞTİREN TEK otomatik yol, DeductUsage
// içindeki geçiş eşiği (marj) aşımıdır. 429 dahil hiçbir olay geçiş tetiklemez;
// manuel geçiş yalnızca SetActiveKey ile olur.
func (km *KeyManager) GetActiveKey() (*APIKey, error) {
	km.mu.Lock()
	defer km.mu.Unlock()
	return km.activeKeyLocked()
}

// activeKeyLocked, GetActiveKey'in gövdesi (kilit altında): KeyForSession de
// aynı tekil kaynağı kullanır ki iki yol asla farklı anahtar tanımasın.
func (km *KeyManager) activeKeyLocked() (*APIKey, error) {
	if len(km.keys) == 0 {
		return nil, errors.New("kayıtlı hiçbir API anahtarı bulunamadı")
	}

	var active *APIKey
	for _, k := range km.keys {
		if k.IsActive {
			active = k
			break
		}
	}

	if active == nil {
		// Onarım adı altında keys[0]'a ATLAMAK da izinsiz geçişti: aktif
		// anahtar hiçbir koşulda kendiliğinden değişmez.
		return nil, errors.New("aktif API anahtarı yok; arayüzden bir kartı aktif yapın")
	}

	if active.Key == "" {
		return nil, fmt.Errorf("aktif API anahtarının değeri boş ('%s'); arayüzden anahtar değerini girin veya başka bir anahtarı aktif yapın", active.Name)
	}

	return active, nil
}

// effectiveLimitLocked, anahtarın havuz sınırı: günlük limitten geçiş marjı
// (ThresholdTokens) düşülür — DeductUsage rotasyonuyla BİREBİR aynı hesap.
func (km *KeyManager) effectiveLimitLocked(k *APIKey) int64 {
	if k.ThresholdTokens > 0 {
		return k.DailyLimit - k.ThresholdTokens
	}
	return k.DailyLimit
}

// usableLocked, anahtarın oturumlara dağıtılabilir (havuzda) olup olmadığı:
// havuz üyesi, tükenmemiş, değeri dolu ve marj sınırının altında.
func (km *KeyManager) usableLocked(k *APIKey) bool {
	return k.AutoSwitch && !k.IsExhausted && k.Key != "" && k.UsedDaily < km.effectiveLimitLocked(k)
}

// KeyForSession, istek bazlı dinamik API atamasının girişidir:
//  1. oturumun bağlı anahtarı kullanılabilirse → o anahtar (kararlı),
//  2. yeni oturum → havuza dağıt: en az oturumlu → en çok kota kalan →
//     rotasyon sırası → id (deterministik,9 kart dengeli aşınır),
//  3. bağlı anahtarın kotası bittiyse → oturum HEMEN havuzdan yenisine
//     yeniden bağlanır,
//  4. havuz boşsa → bağlı anahtar (var) ya da aktif anahtar; nihai hakem
//     upstream'in günlük 429'udur (havuz03:00 TSİ'de yenilenir).
// root=="" (gömülü/oturumsuz yollar) ve oto geçiş KAPALIYKEN davranış
// aktif anahtarla birebir eski düzendir.
func (km *KeyManager) KeyForSession(root string) (*APIKey, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if len(km.keys) == 0 {
		return nil, errors.New("kayıtlı hiçbir API anahtarı bulunamadı")
	}
	if !km.autoSwitch || root == "" {
		return km.activeKeyLocked()
	}

	var bound *APIKey
	if keyID, ok := km.bindings[root]; ok {
		for _, k := range km.keys {
			if k.ID == keyID {
				bound = k
				break
			}
		}
		if bound != nil && km.usableLocked(bound) {
			return bound, nil
		}
		// bağlı ama havuz dışı (marj/tükenmiş) → aşağıda yeniden bağla
	}

	boundCount := make(map[string]int, len(km.keys))
	for _, id := range km.bindings {
		boundCount[id]++
	}
	var best *APIKey
	for _, k := range km.keys {
		if !km.usableLocked(k) {
			continue
		}
		if best == nil {
			best = k
			continue
		}
		if boundCount[k.ID] != boundCount[best.ID] {
			if boundCount[k.ID] < boundCount[best.ID] {
				best = k
			}
			continue
		}
		remK := km.effectiveLimitLocked(k) - k.UsedDaily
		remB := km.effectiveLimitLocked(best) - best.UsedDaily
		if remK > remB {
			best = k
			continue
		}
		if remK == remB {
			if k.AutoSwitchOrder < best.AutoSwitchOrder ||
				(k.AutoSwitchOrder == best.AutoSwitchOrder && k.ID < best.ID) {
				best = k
			}
		}
	}

	if best != nil {
		km.bindings[root] = best.ID
		_ = km.saveLocked()
		if bound != nil {
			log.Printf("[OTURUM YENİDEN BAĞ] oturum %s: kotaya dayanan '%s' → havuzdan '%s' kartına taşındı.",
				root, bound.ID, best.ID)
		} else {
			log.Printf("[OTURUM ANAHTARI] yeni oturum %s → '%s' (%s) kartına bağlandı.", root, best.Name, best.ID)
		}
		return best, nil
	}

	// Havuz boş (tüm kartlar marj/tükenmiş): bağlı anahtarı koru; upstream'in
	// günlük429'u hatayı söyler,03:00 sıfırlaması havuzu yeniler.
	if bound != nil && bound.Key != "" {
		return bound, nil
	}
	return km.activeKeyLocked()
}

// rebindSessionsLocked, bir anahtara bağlı TÜM oturumları başka anahtara
// taşır (limit dolduğunda tek adımda). Çağıran kilit zaten km.mu'dadır.
// Taşınan oturum sayısı döner.
func (km *KeyManager) rebindSessionsLocked(fromKeyID, toKeyID string) int {
	moved := 0
	for root, keyID := range km.bindings {
		if keyID == fromKeyID {
			km.bindings[root] = toKeyID
			moved++
		}
	}
	return moved
}

// BoundKey, oturumun (kökün) bağlı anahtarını SADECE OKUYARAK döner —
// atama YAPMAZ (panel kartı ve oturum listesi görselleri içindir).
// ok=false: bu oturumun henüz bağı yok.
func (km *KeyManager) BoundKey(root string) (id string, name string, ok bool) {
	if root == "" {
		return "", "", false
	}
	km.mu.RLock()
	defer km.mu.RUnlock()
	keyID, has := km.bindings[root]
	if !has || keyID == "" {
		return "", "", false
	}
	for _, k := range km.keys {
		if k.ID == keyID {
			return k.ID, k.Name, true
		}
	}
	return "", "", false
}

// KeyPoolStats, anahtar bazlı havuz durumu: kaç oturum bağlı, havuzda mı,
// kullanılabilir mi, tükenmiş mi. Panelin "havuz" özeti bundan beslenir.
type KeyPoolStats struct {
	BoundSessions int  `json:"bound_sessions"`
	Usable        bool `json:"usable"`
	Exhausted     bool `json:"exhausted"`
	PoolMember    bool `json:"pool_member"`
}

// PoolStats, tüm anahtarlar için KeyPoolStats'ü TEK kilit altında hesaplar.
func (km *KeyManager) PoolStats() map[string]KeyPoolStats {
	km.mu.RLock()
	defer km.mu.RUnlock()
	counts := make(map[string]int, len(km.bindings))
	for _, keyID := range km.bindings {
		counts[keyID]++
	}
	out := make(map[string]KeyPoolStats, len(km.keys))
	for _, k := range km.keys {
		out[k.ID] = KeyPoolStats{
			BoundSessions: counts[k.ID],
			Usable:        km.usableLocked(k),
			Exhausted:     k.IsExhausted,
			PoolMember:    k.AutoSwitch,
		}
	}
	return out
}

// Unbind, bir oturumun anahtar bağını çözer — oturumun logları/kartı silinince
// kart havuza BOŞA döner. Bağsız kimlikte no-op (false). Kalıcıdır
// (keys.json → session_keys).
func (km *KeyManager) Unbind(root string) bool {
	if root == "" {
		return false
	}
	km.mu.Lock()
	defer km.mu.Unlock()
	if _, ok := km.bindings[root]; !ok {
		return false
	}
	delete(km.bindings, root)
	_ = km.saveLocked()
	log.Printf("[OTURUM BAĞI KOPARILDI] %s → kart havuza döndü", root)
	return true
}

// UnbindAll, TÜM oturum→anahtar bağlarını çözer: tüm oturumlar silinince
// havuz TAMAMEN boşa düşer ve yeni oturumlara sıfırdan dağıtılır.
// Kalıcıdır; dönen değer koparılan bağ sayısı.
func (km *KeyManager) UnbindAll() int {
	km.mu.Lock()
	defer km.mu.Unlock()
	n := len(km.bindings)
	if n == 0 {
		return 0
	}
	km.bindings = make(map[string]string)
	_ = km.saveLocked()
	log.Printf("[OTURUM BAĞLARI SIFIRLA] %d bağ koparıldı — havuz tamamen boşa düştü", n)
	return n
}

func (km *KeyManager) findNextAvailableKeyLocked(fromIdx int) (*APIKey, int) {
	if !km.autoSwitch {
		// Oto geçiş kapalıyken geçiş yasak: aday bile aranmaz.
		return nil, -1
	}
	n := len(km.keys)
	for i := 1; i <= n; i++ {
		idx := (fromIdx + i) % n
		if idx == fromIdx {
			continue // aynı anahtara geri dönülmez
		}
		candidate := km.keys[idx]
		if !candidate.AutoSwitch {
			continue // yalnızca oto geçiş havuzuna seçilmiş kartlar arasında geçiş
		}
		if candidate.IsExhausted {
			continue // 429 ile kotası dolan kart günlük sıfırmaya kadar aday değil
		}
		limit := candidate.DailyLimit
		if candidate.ThresholdTokens > 0 {
			limit = candidate.DailyLimit - candidate.ThresholdTokens
		}
		if candidate.Key != "" && candidate.UsedDaily < limit {
			return candidate, idx
		}
	}
	return nil, -1
}

// DeductUsage records both DAILY and TOTAL (cumulative all-time) token consumption for Chat
func (km *KeyManager) DeductUsage(keyID string, usage UsageResult, calcMode string) (*APIKey, bool, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	var targetKey *APIKey
	var targetIdx int
	for i, k := range km.keys {
		if k.ID == keyID {
			targetKey = k
			targetIdx = i
			break
		}
	}

	if targetKey == nil {
		return nil, false, fmt.Errorf("anahtar bulunamadı: %s", keyID)
	}

	deduction := usage.NetDeduction
	if calcMode == "gross" {
		deduction = usage.GrossDeduction
	}

	// 1. GÜNLÜK METRİKLER (03:00 TSİ'de sıfırlanır)
	targetKey.UsedDaily += deduction
	targetKey.PromptDaily += usage.PromptTokens
	targetKey.CachedDaily += usage.CachedTokens
	targetKey.CompletionDaily += usage.CompletionTokens
	targetKey.ReasoningDaily += usage.ReasoningTokens
	targetKey.RequestsDaily++

	// 2. TÜM ZAMANLAR (KÜMÜLATİF / TOTAL - ASLA SIFIRLANMAZ)
	targetKey.UsedTotal += deduction
	targetKey.PromptTotal += usage.PromptTokens
	targetKey.CachedTotal += usage.CachedTokens
	targetKey.CompletionTotal += usage.CompletionTokens
	targetKey.ReasoningTotal += usage.ReasoningTokens
	targetKey.RequestsTotal++
	targetKey.LastUsedAt = time.Now().In(km.turkeyLoc).Format(time.RFC3339)

	effectiveLimit := targetKey.DailyLimit
	if targetKey.ThresholdTokens > 0 {
		effectiveLimit = targetKey.DailyLimit - targetKey.ThresholdTokens
	}

	rotated := false
	var nextActive *APIKey
	if targetKey.UsedDaily >= effectiveLimit {
		log.Printf("[LİMİT BİLDİRİMİ] Anahtar '%s' (%s) slider limitine (%d / %d) ulaştı.",
			targetKey.Name, targetKey.ID, targetKey.UsedDaily, targetKey.DailyLimit)

		if !km.autoSwitch {
			// Oto geçiş kapalıyken geçiş YASAK: limit aşılsa bile aynı anahtarla devam.
			log.Printf("[OTO GEÇİŞ KAPALI] '%s' limiti aştı; oto geçiş kapalı olduğu için geçiş yapılmadı, anahtar değişmedi.", targetKey.Name)
		} else {
			nextKey, _ := km.findNextAvailableKeyLocked(targetIdx)
			if nextKey != nil {
				// Yeni aktif atanırken TÜM kartların aktifliği temizlenir:
				// eşzamanlı istek yarışında iki kart aynı anda "aktif" kalamaz.
				for _, k := range km.keys {
					k.IsActive = false
				}
				nextKey.IsActive = true
				nextActive = nextKey
				rotated = true
				if moved := km.rebindSessionsLocked(targetKey.ID, nextKey.ID); moved > 0 {
					log.Printf("[OTURUM YENİDEN BAĞ] '%s' marjı doldu: %d oturum → '%s' kartına taşındı.",
						targetKey.Name, moved, nextKey.Name)
				}
				log.Printf("[ROTASYON] Geçiş eşiği (marj) aşıldı; sıradaki müsait '%s' anahtarına geçildi.", nextKey.Name)
			} else {
				log.Printf("[LİMİT UYARISI] Oto geçiş havuzunda sınırı aşmamış başka anahtar yok.")
			}
		}
	}

	_ = km.saveLocked()
	return nextActive, rotated, nil
}

// RecordEmbeddingUsage tracks embedding tokens separately WITHOUT deducting from the 10M chat quota
func (km *KeyManager) RecordEmbeddingUsage(keyID string, tokens int64) {
	km.mu.Lock()
	defer km.mu.Unlock()

	for _, k := range km.keys {
		if k.ID == keyID {
			// Günlük embedding
			k.EmbeddingTokensDaily += tokens
			k.EmbeddingRequestsDaily++

			// Kümülatif embedding
			k.EmbeddingTokensTotal += tokens
			k.EmbeddingRequestsTotal++

			// EVREN MEKANİZMASI: embedding token'ları KOTADAN otomatik düşülür —
			// net hesap formülüne (marj/önbellek ağırlığı) girmez, ham olarak
			// günlük ve toplam kotaya yazılır. Sayaçlar 03:00 TSİ'de sıfırlanır.
			k.UsedDaily += tokens
			k.UsedTotal += tokens

			k.LastUsedAt = time.Now().In(km.turkeyLoc).Format(time.RFC3339)
			_ = km.saveLocked()
			log.Printf("[EMBEDDING USAGE] [%s] Harcanan Token: %d | Günlük Embedding: %d | Kotadan otomatik düşüldü (Günlük: %d / %d)",
				k.Name, tokens, k.EmbeddingTokensDaily, k.UsedDaily, k.DailyLimit)
			break
		}
	}
}

// RecordAudioUsage, ses (ASR / konuşma tanıma) kullanımını EMBEDDING ile aynı
// mantıkla ayrı sayaçlarda takip eder: günlük/toplam ses token'ları, istek
// sayısı ve (upstream bildirirse) ses süresi. Chat net formülüne (marj/önbellek
// ağırlığı) girmez; token varsa ham olarak günlük/toplam kotaya yazılır —
// böylece 10M kota muhasebesi embedding ile tutarlı kalır.
// @param keyID - isteği karşılayan anahtar kimliği.
// @param tokens - upstream'in bildirdiği ses token'ı (yoksa 0).
// @param seconds - upstream'in bildirdiği ses süresi sn (yoksa 0).
func (km *KeyManager) RecordAudioUsage(keyID string, tokens, seconds int64) {
	km.mu.Lock()
	defer km.mu.Unlock()

	for _, k := range km.keys {
		if k.ID == keyID {
			k.AudioTokensDaily += tokens
			k.AudioRequestsDaily++
			k.AudioSecondsDaily += seconds

			k.AudioTokensTotal += tokens
			k.AudioRequestsTotal++
			k.AudioSecondsTotal += seconds

			if tokens > 0 {
				k.UsedDaily += tokens
				k.UsedTotal += tokens
			}

			k.LastUsedAt = time.Now().In(km.turkeyLoc).Format(time.RFC3339)
			_ = km.saveLocked()
			log.Printf("[AUDIO USAGE] [%s] Ses İsteği: %d | Token: %d | Süre: %d sn | Günlük Ses: %d token / %d istek (Kotaya ham düşüldü: %d / %d)",
				k.Name, k.AudioRequestsDaily, tokens, seconds, k.AudioTokensDaily, k.AudioRequestsDaily, k.UsedDaily, k.DailyLimit)
			break
		}
	}
}

// RecordDecisionUsage, KARAR (decision) isteklerini — POST /v1/systemone,
// modeller: clef (görsel destekli) ve laya — kendi sayaçlarında takip eder.
// Bu modeller metin ÜRETMEZ: tek geçişte choice/score/noul sorularını
// olasılıklarıyla yanıtlar; bu yüzden output token'ı yoktur, yalnızca girdi
// token'ı (state + sorular + görseller) oluşur. Ölçüm: görsel token'ı karo
// karo sayılır (32×32 px = 1 token; 760×760 ≈ 579, 1600×1600 ≈ 2503).
// Embedding/ses ile aynı mantık: chat net formülüne girmez, token varsa ham
// olarak günlük/toplam kotaya yazılır; anahtar rotasyonu tetiklemez.
// @param keyID - isteği karşılayan anahtar kimliği.
// @param tokens - upstream'in bildirdiği girdi token'ı (usage.input_tokens).
func (km *KeyManager) RecordDecisionUsage(keyID string, tokens int64) {
	km.mu.Lock()
	defer km.mu.Unlock()

	for _, k := range km.keys {
		if k.ID == keyID {
			k.DecisionTokensDaily += tokens
			k.DecisionRequestsDaily++

			k.DecisionTokensTotal += tokens
			k.DecisionRequestsTotal++

			if tokens > 0 {
				k.UsedDaily += tokens
				k.UsedTotal += tokens
			}

			k.LastUsedAt = time.Now().In(km.turkeyLoc).Format(time.RFC3339)
			_ = km.saveLocked()
			log.Printf("[DECISION USAGE] [%s] Karar İsteği: %d | Girdi Token: %d | Günlük Karar: %d token / %d istek (Kotaya ham düşüldü: %d / %d)",
				k.Name, k.DecisionRequestsDaily, tokens, k.DecisionTokensDaily, k.DecisionRequestsDaily, k.UsedDaily, k.DailyLimit)
			break
		}
	}
}

// NOT: upstream 429 artık geçiş TETİKLEMEZ — MarkExhaustedAndRotate tamamen
// kaldırıldı. Tek otomatik geçiş tetikleyicisi DeductUsage'taki geçiş eşiğidir.
// RotateAfterDailyLimit, upstream'in GÜNLÜK kota 429'unu (code=daily_token_limit_exceeded)
// yakaladığında çalışan ikinci geçiş tetikleyicisidir: yalnızca bu code ile çağrılır,
// dakikalık rate_limit 429'lar bu yola girmez. Oto geçiş AÇIKKEN sıradaki havuz
// kartına geçer ve hedef kartı IsExhausted işaretleyerek günlük sıfırmaya kadar
// adaylıktan çıkarır; KAPALIYKEN hiçbir şey değiştirmez.
func (km *KeyManager) RotateAfterDailyLimit(keyID string) (*APIKey, bool) {
	km.mu.Lock()
	defer km.mu.Unlock()

	var targetKey *APIKey
	var targetIdx int
	for i, k := range km.keys {
		if k.ID == keyID {
			targetKey = k
			targetIdx = i
			break
		}
	}
	if targetKey == nil {
		return nil, false
	}

	if !km.autoSwitch {
		log.Printf("[OTO GEÇİŞ KAPALI] '%s' için günlük limit 429 alındı; oto geçiş kapalı olduğu için geçiş yapılmadı.", targetKey.Name)
		_ = km.saveLocked()
		return nil, false
	}

	targetKey.IsExhausted = true
	nextKey, _ := km.findNextAvailableKeyLocked(targetIdx)
	if nextKey == nil {
		log.Printf("[OTO GEÇİŞ] Günlük limit 429 sonrası havuzda uygun kart yok ('%s' adaylıktan çıkarıldı).", targetKey.Name)
		_ = km.saveLocked()
		return nil, false
	}

	for _, k := range km.keys {
		k.IsActive = false
	}
	nextKey.IsActive = true
	if moved := km.rebindSessionsLocked(targetKey.ID, nextKey.ID); moved > 0 {
		log.Printf("[OTURUM YENİDEN BAĞ] '%s' kotası doldu: %d oturum → '%s' kartına taşındı.",
			targetKey.Name, moved, nextKey.Name)
	}
	_ = km.saveLocked()
	log.Printf("[OTO GEÇİŞ] Günlük limit 429 (daily_token_limit_exceeded): '%s' → '%s' kartına geçildi.", targetKey.Name, nextKey.Name)
	return nextKey, true
}

func (km *KeyManager) RecordError(keyID string) {
	km.mu.Lock()
	defer km.mu.Unlock()

	for _, k := range km.keys {
		if k.ID == keyID {
			k.ErrorsTotal++
			_ = km.saveLocked()
			break
		}
	}
}

func (km *KeyManager) GetAllKeys() []*APIKey {
	km.mu.RLock()
	defer km.mu.RUnlock()

	result := make([]*APIKey, len(km.keys))
	order := 0
	for i, k := range km.keys {
		cp := *k
		if cp.AutoSwitch {
			order++
			cp.AutoSwitchOrder = order
		} else {
			cp.AutoSwitchOrder = 0
		}
		if len(cp.Key) > 10 {
			cp.Key = cp.Key[:4] + "..." + cp.Key[len(cp.Key)-4:]
		}
		result[i] = &cp
	}
	return result
}

func (km *KeyManager) SetActiveKey(id string) error {
	km.mu.Lock()
	defer km.mu.Unlock()

	found := false
	for _, k := range km.keys {
		if k.ID == id {
			k.IsActive = true
			k.IsExhausted = false
			found = true
		} else {
			k.IsActive = false
		}
	}
	if !found {
		return fmt.Errorf("anahtar bulunamadı: %s", id)
	}
	return km.saveLocked()
}

// SetAutoSwitch API'ler arası otomatik geçişin ana anahtarını değiştirir.
// Kapalıyken hiçbir kod yolu (eşik, 429, boş anahtar) geçiş yapamaz.
func (km *KeyManager) SetAutoSwitch(enabled bool) {
	km.mu.Lock()
	defer km.mu.Unlock()
	if km.autoSwitch == enabled {
		return
	}
	km.autoSwitch = enabled
	_ = km.saveLocked()
	if enabled {
		log.Printf("[OTO GEÇİŞ] Otomatik API geçişi AÇILDI — yalnızca havuz kartları arasında geçiş yapılacak.")
	} else {
		log.Printf("[OTO GEÇİŞ] Otomatik API geçişi KAPATILDI — API'ler arası geçiş yasak.")
	}
}

// AutoSwitchEnabled oto geçişin açık olup olmadığını döndürür.
func (km *KeyManager) AutoSwitchEnabled() bool {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return km.autoSwitch
}

// SetAutoSwitchMember bir anahtarı oto geçiş havuzuna ekler/çıkarır (kart üyeliği).
func (km *KeyManager) SetAutoSwitchMember(id string, enabled bool) error {
	km.mu.Lock()
	defer km.mu.Unlock()
	for _, k := range km.keys {
		if k.ID == id {
			k.AutoSwitch = enabled
			if enabled {
				log.Printf("[OTO GEÇİŞ] '%s' anahtarı oto geçiş havuzuna eklendi.", k.Name)
			} else {
				log.Printf("[OTO GEÇİŞ] '%s' anahtarı oto geçiş havuzundan çıkarıldı.", k.Name)
			}
			return km.saveLocked()
		}
	}
	return fmt.Errorf("anahtar bulunamadı: %s", id)
}

func (km *KeyManager) UnexhaustKey(id string) error {
	km.mu.Lock()
	defer km.mu.Unlock()

	found := false
	for _, k := range km.keys {
		if k.ID == id {
			k.IsExhausted = false
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("anahtar bulunamadı: %s", id)
	}
	return km.saveLocked()
}

func (km *KeyManager) UnexhaustAll() error {
	km.mu.Lock()
	defer km.mu.Unlock()

	for _, k := range km.keys {
		k.IsExhausted = false
	}
	return km.saveLocked()
}

func (km *KeyManager) AddKey(name, keyVal string, limit, threshold int64) (*APIKey, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if limit <= 0 {
		limit = 10000000
	}
	if threshold < 0 {
		threshold = 50000
	}

	// Benzersiz ID: silmeden sonra "key-N" çakışırsa (eski bug) kullanım
	// yanlış karta yazılırdı; çakışan ID bir üst numaraya artar.
	n := len(km.keys) + 1
	id := fmt.Sprintf("key-%d", n)
	for {
		dup := false
		for _, k := range km.keys {
			if k.ID == id {
				dup = true
				break
			}
		}
		if !dup {
			break
		}
		n++
		id = fmt.Sprintf("key-%d", n)
	}
	newKey := &APIKey{
		ID:              id,
		Name:            name,
		Key:             keyVal,
		DailyLimit:      limit,
		ThresholdTokens: threshold,
		IsActive:        len(km.keys) == 0,
		AutoSwitch:      km.autoSwitch, // oto geçiş açıkken yeni anahtar havuza katılır (arayüzden çıkarılabilir)
		CreatedAt:       time.Now().In(km.turkeyLoc).Format(time.RFC3339),
	}

	km.keys = append(km.keys, newKey)
	return newKey, km.saveLocked()
}

func (km *KeyManager) UpdateKey(id, name, keyVal string, limit, threshold int64) error {
	km.mu.Lock()
	defer km.mu.Unlock()

	for _, k := range km.keys {
		if k.ID == id {
			if name != "" {
				k.Name = name
			}
			if keyVal != "" {
				k.Key = keyVal
			}
			if limit > 0 {
				k.DailyLimit = limit
				effectiveLimit := limit
				if k.ThresholdTokens > 0 {
					effectiveLimit = limit - k.ThresholdTokens
				}
				if k.UsedDaily < effectiveLimit {
					k.IsExhausted = false
				}
			}
			if threshold >= 0 {
				k.ThresholdTokens = threshold
				effectiveLimit := k.DailyLimit
				if threshold > 0 {
					effectiveLimit = k.DailyLimit - threshold
				}
				if k.UsedDaily < effectiveLimit {
					k.IsExhausted = false
				}
			}
			return km.saveLocked()
		}
	}
	return fmt.Errorf("anahtar bulunamadı: %s", id)
}

func (km *KeyManager) DeleteKey(id string) error {
	km.mu.Lock()
	defer km.mu.Unlock()

	idx := -1
	for i, k := range km.keys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("anahtar bulunamadı: %s", id)
	}

	// Aktif kart silindiğinde keys[0] ZORLA AKTİF yapılmaz: aktif anahtar
	// kendiliğinden değişmez. İstekler, arayüzden yeni bir kart seçilene kadar
	// açıklayıcı "aktif anahtar yok" hatası alır.
	km.keys = append(km.keys[:idx], km.keys[idx+1:]...)

	return km.saveLocked()
}

func (km *KeyManager) ResetDailyForce() {
	km.mu.Lock()
	defer km.mu.Unlock()

	for _, k := range km.keys {
		k.UsedDaily = 0
		k.PromptDaily = 0
		k.CachedDaily = 0
		k.CompletionDaily = 0
		k.ReasoningDaily = 0
		k.RequestsDaily = 0
		k.EmbeddingTokensDaily = 0
		k.EmbeddingRequestsDaily = 0
		k.AudioTokensDaily = 0
		k.AudioRequestsDaily = 0
		k.AudioSecondsDaily = 0
		k.DecisionTokensDaily = 0
		k.DecisionRequestsDaily = 0
		k.IsExhausted = false
	}
	// Manuel sıfırlama da aktif anahtarı değiştirmez (yalnızca sayaçlar sıfırlanır).
	km.lastResetDate = km.currentResetDayString(time.Now().In(km.turkeyLoc))
	_ = km.saveLocked()
}
