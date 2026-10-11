package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// maxDisplayNameRunes, oturuma verilebilecek özel adın karakter sınırı.
const maxDisplayNameRunes = 80

type EvrenServer struct {
	km    *KeyManager
	proxy *ProxyHandler
	mux   *http.ServeMux
}

func NewEvrenServer(km *KeyManager, proxy *ProxyHandler) *EvrenServer {
	s := &EvrenServer{
		km:    km,
		proxy: proxy,
		mux:   http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *EvrenServer) registerRoutes() {
	// Arayüz
	s.mux.HandleFunc("/", s.handleRoot)
	s.mux.HandleFunc("/dashboard", s.handleDashboard)
	s.mux.HandleFunc("/ui/evren", s.handleDashboard)
	// Sürükle-bırak ses test sayfası (8110'da da aynı adla sunulur)
	s.mux.HandleFunc("/asr-test", handleASRTestPage)
	s.mux.HandleFunc("/ses-test", handleASRTestPage)

	// Dashboard API'leri
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/evren/status", s.handleStatus)
	s.mux.HandleFunc("/api/keys", s.handleKeys)
	s.mux.HandleFunc("/api/evren/keys", s.handleKeys)
	s.mux.HandleFunc("/api/keys/", s.handleKeyAction)
	s.mux.HandleFunc("/api/evren/keys/", s.handleKeyAction)
	s.mux.HandleFunc("/api/reset-now", s.handleResetNow)
	s.mux.HandleFunc("/api/evren/reset-now", s.handleResetNow)
	s.mux.HandleFunc("/api/unexhaust-all", s.handleUnexhaustAll)
	s.mux.HandleFunc("/api/evren/unexhaust-all", s.handleUnexhaustAll)
	s.mux.HandleFunc("/api/logs", s.handleLogs)
	s.mux.HandleFunc("/api/evren/logs", s.handleLogs)
	s.mux.HandleFunc("/api/logs/clear-session", s.handleClearSessionLogs)
	s.mux.HandleFunc("/api/evren/logs/clear-session", s.handleClearSessionLogs)
	s.mux.HandleFunc("/api/logs/clear-all-sessions", s.handleClearAllSessions)
	s.mux.HandleFunc("/api/evren/logs/clear-all-sessions", s.handleClearAllSessions)
	s.mux.HandleFunc("/api/logs/embeddings", s.handleEmbeddingLogs)
	s.mux.HandleFunc("/api/evren/logs/embeddings", s.handleEmbeddingLogs)
	// Ses (ASR) istek günlüğü: chat tablosundan ayrı, embedding gibi kendi bölümü.
	s.mux.HandleFunc("/api/logs/audio", s.handleAudioLogs)
	s.mux.HandleFunc("/api/evren/logs/audio", s.handleAudioLogs)
	// Karar (decision) istek günlüğü: /v1/systemone — clef/laya.
	s.mux.HandleFunc("/api/logs/decisions", s.handleDecisionLogs)
	s.mux.HandleFunc("/api/evren/logs/decisions", s.handleDecisionLogs)

	// Transcript dökümü + oto geçiş ana anahtarı
	s.mux.HandleFunc("/api/logs/transcript", s.handleTranscriptDump)
	s.mux.HandleFunc("/api/evren/logs/transcript", s.handleTranscriptDump)
	// Oturum bazlı TEK oturum logu: /logs/transcript/<oturum_id>
	s.mux.HandleFunc("/api/logs/transcript/", s.handleTranscriptSession)
	s.mux.HandleFunc("/api/evren/logs/transcript/", s.handleTranscriptSession)
	// Oturum listesi + oturuma özel üretilmiş endpoint URL'leri
	// (alt ağacın ÜSTÜNE çıkar: daha uzun eşleşme Go'da kazanır)
	s.mux.HandleFunc("/api/logs/transcript/sessions", s.handleTranscriptSessions)
	s.mux.HandleFunc("/api/evren/logs/transcript/sessions", s.handleTranscriptSessions)
	// Karta ✏️ ile verilen özel ad (RAM)
	s.mux.HandleFunc("/api/logs/transcript/rename", s.handleTranscriptRename)
	s.mux.HandleFunc("/api/evren/logs/transcript/rename", s.handleTranscriptRename)
	s.mux.HandleFunc("/api/auto-switch", s.handleAutoSwitch)
	s.mux.HandleFunc("/api/evren/auto-switch", s.handleAutoSwitch)

	// Model bazlı görsel sınırı okuma/kaydetme (ayar paneli)
	s.mux.HandleFunc("/api/image-limits", s.handleImageLimits)
	s.mux.HandleFunc("/api/evren/image-limits", s.handleImageLimits)

	// EVREN / OpenAI API Yönlendirmeleri
	s.mux.HandleFunc("/v1/", s.proxy.ServeHTTP)
	s.mux.HandleFunc("/v1/chat/completions", s.proxy.ServeHTTP)
	s.mux.HandleFunc("/v1/embeddings", s.proxy.ServeHTTP)
	// Ses (ASR): konuşma tanıma/çeviri — multipart gövde ham geçer.
	s.mux.HandleFunc("/v1/audio/transcriptions", s.proxy.ServeHTTP)
	s.mux.HandleFunc("/v1/audio/translations", s.proxy.ServeHTTP)
	s.mux.HandleFunc("/v1/models", s.proxy.ServeHTTP)
	s.mux.HandleFunc("/v1/responses", s.proxy.ServeHTTP)

	// Prefixli /evren/v1/ rotası (8110 portundan veya 8088'den erişim)
	s.mux.HandleFunc("/evren/v1/", func(w http.ResponseWriter, r *http.Request) {
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/evren")
		s.proxy.ServeHTTP(w, r2)
	})
}

func (s *EvrenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS başlıkları
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Eğer istek /v1/ yoluna aitse doğrudan proxy'ye
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		s.proxy.ServeHTTP(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/evren/v1/") {
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/evren")
		s.proxy.ServeHTTP(w, r2)
		return
	}

	s.mux.ServeHTTP(w, r)
}

func (s *EvrenServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	s.handleDashboard(w, r)
}

func (s *EvrenServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(GetEmbeddedDashboardHTML()))
}

// ---- ANA METRİK KARTLARI GEÇMİŞİ (usage_history.jsonl) ----
//
// Ana metrik kartları (GÜNLÜK METRİKLER / TÜM ZAMANLAR) artık key kartlarının
// sayaçlarından DEĞİL, kalıcı log dosyasından beslenir. Böylece key kartları
// (keys.json) tek seferlik sıfırlansa bile ana metrik kartlarının geçmişi
// korunur; dosya silinirse/boşsa eski davranışa (key sayaçlarına) düşülür.

// historyUsage, log dosyasından hesaplanan metrik toplamları.
type historyUsage struct {
	Prompt          int64
	Cached          int64
	Completion      int64
	Net             int64
	Requests        int64
	EmbeddingTokens int64
	EmbeddingReqs   int64
	AudioTokens     int64
	AudioReqs       int64
	AudioSeconds    int64
	DecisionTokens  int64
	DecisionReqs    int64
}

// historyTotals, ana metrik kartları için günlük ve tüm-zamanlar toplamları.
type historyTotals struct {
	Daily historyUsage
	All   historyUsage
}

var (
	histTotalsMu  sync.Mutex
	histTotalsKey string
	histTotalsVal historyTotals
)

// evrenTodayDay, 03:00 TSİ sınırına göre bugünü YYYY-MM-DD verir.
func evrenTodayDay() string {
	if globalLogger != nil {
		return globalLogger.CurrentDayString()
	}
	return time.Now().UTC().Format("2006-01-02")
}

// loadHistoryTotals, ana metrik kartlarının geçmişini log dosyasından okur.
// modTime/size/gün değişmediyse önbellekten döner; kullanılabilir token kaydı
// yoksa ok=false döner (çağıran key sayaçlarına düşer).
func loadHistoryTotals(today string) (historyTotals, bool) {
	logFile := GetConfig().LogFile
	if logFile == "" {
		return historyTotals{}, false
	}
	st, err := os.Stat(logFile)
	if err != nil {
		return historyTotals{}, false
	}
	cacheKey := fmt.Sprintf("%d|%d|%s", st.ModTime().UnixNano(), st.Size(), today)

	histTotalsMu.Lock()
	defer histTotalsMu.Unlock()
	if cacheKey == histTotalsKey {
		return histTotalsVal, true
	}

	var out historyTotals
	f, err := os.Open(logFile)
	if err != nil {
		return historyTotals{}, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var e struct {
			Day         string      `json:"day"`
			IsEmbedding bool        `json:"is_embedding"`
			IsAudio     bool        `json:"is_audio"`
			IsDecision  bool        `json:"is_decision"`
			AudioSec    int64       `json:"audio_seconds"`
			Usage       UsageResult `json:"usage"`
		}
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		u := e.Usage

		accumulate := func(h *historyUsage) {
			if e.IsAudio {
				// SES (ASR): embedding gibi ayrı sayaçlar; token varsa ham düşülür.
				tokens := u.PromptTokens
				if tokens == 0 {
					tokens = u.TotalTokens
				}
				h.AudioTokens += tokens
				h.AudioReqs++
				h.AudioSeconds += e.AudioSec
				h.Net += tokens
				return
			}
			if e.IsEmbedding {
				tokens := u.PromptTokens
				if tokens == 0 {
					tokens = u.TotalTokens
				}
				h.EmbeddingTokens += tokens
				h.EmbeddingReqs++
				// EVREN mekanizması: embedding token'ları kotadan ham düşülür
				// (formül hesabına girmez) — key kartlarıyla tutarlı olsun.
				h.Net += tokens
				return
			}
			if e.IsDecision {
				tokens := u.PromptTokens
				if tokens == 0 {
					tokens = u.TotalTokens
				}
				h.DecisionTokens += tokens
				h.DecisionReqs++
				// Karar istekleri de kotadan ham düşülür (embedding/ses ile aynı).
				h.Net += tokens
				return
			}
			h.Prompt += u.PromptTokens
			h.Cached += u.CachedTokens
			h.Completion += u.CompletionTokens
			net := u.NetDeduction
			if net == 0 && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
				net = (u.PromptTokens - u.CachedTokens) + u.CompletionTokens + int64(math.Round(float64(u.CachedTokens)*0.03))
			}
			h.Net += net
			if u.PromptTokens > 0 || u.CompletionTokens > 0 {
				h.Requests++
			}
		}

		accumulate(&out.All)
		if today != "" && e.Day == today {
			accumulate(&out.Daily)
		}
	}

	// Kullanılabilir token kaydı yoksa dosya bir kaynak sayılmaz.
	if out.All.Prompt == 0 && out.All.Completion == 0 && out.All.EmbeddingReqs == 0 && out.All.AudioReqs == 0 {
		return historyTotals{}, false
	}

	histTotalsKey = cacheKey
	histTotalsVal = out
	return out, true
}

func (s *EvrenServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	cfg := GetConfig()
	keys := s.poolKeys()

	var activeKeyID string
	// GÜNLÜK CHAT
	var totalPromptToday, totalCachedToday, totalCompletionToday, totalNetToday, totalRequestsToday int64
	// GÜNLÜK EMBEDDING
	var totalEmbeddingTokensToday, totalEmbeddingRequestsToday int64
	// GÜNLÜK SES (ASR)
	var totalAudioTokensToday, totalAudioRequestsToday, totalAudioSecondsToday int64
	// GÜNLÜK KARAR (DECISION — /v1/systemone)
	var totalDecisionTokensToday, totalDecisionRequestsToday int64

	// TÜM ZAMANLAR CHAT (TOTAL)
	var totalPromptAllTime, totalCachedAllTime, totalCompletionAllTime, totalNetAllTime, totalRequestsAllTime int64
	// TÜM ZAMANLAR EMBEDDING (TOTAL)
	var totalEmbeddingTokensAllTime, totalEmbeddingRequestsAllTime int64
	// TÜM ZAMANLAR SES (ASR) (TOTAL)
	var totalAudioTokensAllTime, totalAudioRequestsAllTime, totalAudioSecondsAllTime int64
	// TÜM ZAMANLAR KARAR (DECISION) (TOTAL)
	var totalDecisionTokensAllTime, totalDecisionRequestsAllTime int64

	var totalPoolLimit int64
	var totalPoolRemaining int64
	var configuredKeysCount int

	for _, k := range keys {
		if k.IsActive {
			activeKeyID = k.ID
		}
		if k.Key != "" {
			configuredKeysCount++
			totalPoolLimit += k.DailyLimit
			rem := k.DailyLimit - k.UsedDaily
			if rem > 0 {
				totalPoolRemaining += rem
			}
		}
		// Günlük
		totalNetToday += k.UsedDaily
		totalPromptToday += k.PromptDaily
		totalCachedToday += k.CachedDaily
		totalCompletionToday += k.CompletionDaily
		totalRequestsToday += k.RequestsDaily

		totalEmbeddingTokensToday += k.EmbeddingTokensDaily
		totalEmbeddingRequestsToday += k.EmbeddingRequestsDaily

		totalAudioTokensToday += k.AudioTokensDaily
		totalAudioRequestsToday += k.AudioRequestsDaily
		totalAudioSecondsToday += k.AudioSecondsDaily

		totalDecisionTokensToday += k.DecisionTokensDaily
		totalDecisionRequestsToday += k.DecisionRequestsDaily

		// Tüm zamanlar
		totalNetAllTime += k.UsedTotal
		totalPromptAllTime += k.PromptTotal
		totalCachedAllTime += k.CachedTotal
		totalCompletionAllTime += k.CompletionTotal
		totalRequestsAllTime += k.RequestsTotal

		totalEmbeddingTokensAllTime += k.EmbeddingTokensTotal
		totalEmbeddingRequestsAllTime += k.EmbeddingRequestsTotal

		totalAudioTokensAllTime += k.AudioTokensTotal
		totalAudioRequestsAllTime += k.AudioRequestsTotal
		totalAudioSecondsAllTime += k.AudioSecondsTotal

		totalDecisionTokensAllTime += k.DecisionTokensTotal
		totalDecisionRequestsAllTime += k.DecisionRequestsTotal
	}

	// ANA METRİK KARTLARI GEÇMİŞTEN BESLENİR: key kartlarındaki sayaçlar
	// tek seferlik sıfırlansa bile günlük/tüm-zamanlar kartları geçmişini
	// korur. Dosya yoksa/boşsa eski toplamlara (key sayaçlarına) düşülür.
	if hist, ok := loadHistoryTotals(evrenTodayDay()); ok {
		totalPromptToday = hist.Daily.Prompt
		totalCachedToday = hist.Daily.Cached
		totalCompletionToday = hist.Daily.Completion
		totalNetToday = hist.Daily.Net
		totalRequestsToday = hist.Daily.Requests
		totalEmbeddingTokensToday = hist.Daily.EmbeddingTokens
		totalEmbeddingRequestsToday = hist.Daily.EmbeddingReqs

		totalAudioTokensToday = hist.Daily.AudioTokens
		totalAudioRequestsToday = hist.Daily.AudioReqs
		totalAudioSecondsToday = hist.Daily.AudioSeconds

		totalDecisionTokensToday = hist.Daily.DecisionTokens
		totalDecisionRequestsToday = hist.Daily.DecisionReqs

		totalPromptAllTime = hist.All.Prompt
		totalCachedAllTime = hist.All.Cached
		totalCompletionAllTime = hist.All.Completion
		totalNetAllTime = hist.All.Net
		totalRequestsAllTime = hist.All.Requests
		totalEmbeddingTokensAllTime = hist.All.EmbeddingTokens
		totalEmbeddingRequestsAllTime = hist.All.EmbeddingReqs

		totalAudioTokensAllTime = hist.All.AudioTokens
		totalAudioRequestsAllTime = hist.All.AudioReqs
		totalAudioSecondsAllTime = hist.All.AudioSeconds

		totalDecisionTokensAllTime = hist.All.DecisionTokens
		totalDecisionRequestsAllTime = hist.All.DecisionReqs

		// KALAN/DÜŞÜLEN kartı tutarlı kalsın: kalan = havuz limiti - günlük net
		totalPoolRemaining = totalPoolLimit - totalNetToday
		if totalPoolRemaining < 0 {
			totalPoolRemaining = 0
		}
	}

	if configuredKeysCount == 0 {
		totalPoolLimit = cfg.DefaultDailyLimit
		rem := cfg.DefaultDailyLimit - totalNetToday
		if rem > 0 {
			totalPoolRemaining = rem
		}
	}

	nextReset := s.km.NextResetTime()
	secondsUntilReset := int64(time.Until(nextReset).Seconds())
	if secondsUntilReset < 0 {
		secondsUntilReset = 0
	}

	hours := secondsUntilReset / 3600
	mins := (secondsUntilReset % 3600) / 60
	secs := secondsUntilReset % 60
	countdownStr := fmt.Sprintf("%02d:%02d:%02d", hours, mins, secs)

	var dailyCacheRatio float64
	if totalPromptToday > 0 {
		dailyCacheRatio = (float64(totalCachedToday) / float64(totalPromptToday)) * 100.0
	}

	var allTimeCacheRatio float64
	if totalPromptAllTime > 0 {
		allTimeCacheRatio = (float64(totalCachedAllTime) / float64(totalPromptAllTime)) * 100.0
	}

	resp := map[string]interface{}{
		"active_key_id":         activeKeyID,
		"auto_switch_enabled":   s.km.AutoSwitchEnabled(),
		"keys":                  keys,
		"next_reset_in_seconds": secondsUntilReset,
		"next_reset_countdown":  countdownStr,
		"next_reset_time":       nextReset.Format("2006-01-02 15:04:05 MST"),
		"calculation_mode":      cfg.CalculationMode,
		"default_daily_limit":   cfg.DefaultDailyLimit,
		"total_pool_limit":      totalPoolLimit,
		"total_pool_remaining":  totalPoolRemaining,
		"configured_keys_count": configuredKeysCount,
		"upstream_base":         cfg.UpstreamBase,
		"listen_addr":           cfg.ListenAddr,

		// GÜNLÜK SAYACLAR (03:00 TSİ)
		"total_prompt_today":     totalPromptToday,
		"total_cached_today":     totalCachedToday,
		"total_completion_today": totalCompletionToday,
		"total_net_today":        totalNetToday,
		"total_requests_today":   totalRequestsToday,
		"daily_cache_ratio":      fmt.Sprintf("%.1f", dailyCacheRatio),

		// GÜNLÜK EMBEDDING SAYACLAR (Chat kotasından bağımsız)
		"total_embedding_tokens_today":   totalEmbeddingTokensToday,
		"total_embedding_requests_today": totalEmbeddingRequestsToday,

		// GÜNLÜK SES/ASR SAYACLAR (embedding gibi ayrı takip)
		"total_audio_tokens_today":   totalAudioTokensToday,
		"total_audio_requests_today": totalAudioRequestsToday,
		"total_audio_seconds_today":  totalAudioSecondsToday,

		// GÜNLÜK KARAR SAYAÇLAR (/v1/systemone — clef/laya; metin üretmez, girdi token'ı sayılır)
		"total_decision_tokens_today":   totalDecisionTokensToday,
		"total_decision_requests_today": totalDecisionRequestsToday,

		// TOTAL SAYACLAR (KÜMÜLATİF - ASLA SIFIRLANMAZ)
		"total_prompt_all_time":          totalPromptAllTime,
		"total_cached_all_time":          totalCachedAllTime,
		"total_completion_all_time":      totalCompletionAllTime,
		"total_net_all_time":             totalNetAllTime,
		"total_requests_all_time":        totalRequestsAllTime,
		"all_time_cache_ratio":           fmt.Sprintf("%.1f", allTimeCacheRatio),

		// TOTAL EMBEDDING SAYACLAR
		"total_embedding_tokens_all_time":   totalEmbeddingTokensAllTime,
		"total_embedding_requests_all_time": totalEmbeddingRequestsAllTime,

		// TOTAL SES/ASR SAYACLAR
		"total_audio_tokens_all_time":   totalAudioTokensAllTime,
		"total_audio_requests_all_time": totalAudioRequestsAllTime,
		"total_audio_seconds_all_time":  totalAudioSecondsAllTime,

		// TOTAL KARAR SAYAÇLAR
		"total_decision_tokens_all_time":   totalDecisionTokensAllTime,
		"total_decision_requests_all_time": totalDecisionRequestsAllTime,
	}

	_ = json.NewEncoder(w).Encode(resp)
}

// poolKeyView, panel listelerine havuz bağlamı katan anahtar görünümü:
// APIKey alanları aynı gövdede kalır (geri uyumlu), yanına bağlı oturum
// sayısı ve kullanılabilirlik eklenir.
type poolKeyView struct {
	*APIKey
	BoundSessions int  `json:"bound_sessions"`
	Usable        bool `json:"usable"`
}

// poolKeys, tüm anahtarları havuz istatistikleriyle tek kilit altında
// zenginleştirir (status + /api/keys uçları aynı kaynağı paylaşır).
func (s *EvrenServer) poolKeys() []poolKeyView {
	stats := s.km.PoolStats()
	keys := s.km.GetAllKeys()
	out := make([]poolKeyView, 0, len(keys))
	for _, k := range keys {
		st := stats[k.ID]
		out = append(out, poolKeyView{APIKey: k, BoundSessions: st.BoundSessions, Usable: st.Usable})
	}
	return out
}

func (s *EvrenServer) handleKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.poolKeys())
		return
	}

	if r.Method == "POST" {
		var req struct {
			Name            string `json:"name"`
			Key             string `json:"key"`
			DailyLimit      int64  `json:"daily_limit"`
			ThresholdTokens int64  `json:"threshold_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"Geçersiz JSON"}`, http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			req.Name = "Yeni API Anahtarı"
		}

		newKey, err := s.km.AddKey(req.Name, req.Key, req.DailyLimit, req.ThresholdTokens)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(newKey)
		return
	}

	http.Error(w, "Metot desteklenmiyor", http.StatusMethodNotAllowed)
}

func (s *EvrenServer) handleKeyAction(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/evren/keys/") {
		path = strings.TrimPrefix(path, "/api/evren/keys/")
	} else {
		path = strings.TrimPrefix(path, "/api/keys/")
	}
	parts := strings.Split(path, "/")
	keyID := parts[0]

	if keyID == "" {
		http.Error(w, "Key ID eksik", http.StatusBadRequest)
		return
	}

	if len(parts) == 2 && (parts[1] == "activate" || parts[1] == "unexhaust") && r.Method == "POST" {
		if err := s.km.SetActiveKey(keyID); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"Anahtar kilidi açıldı ve aktif yapıldı"}`))
		return
	}

	if len(parts) == 2 && parts[1] == "auto-switch" && r.Method == "POST" {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"Geçersiz JSON"}`, http.StatusBadRequest)
			return
		}
		if err := s.km.SetAutoSwitchMember(keyID, req.Enabled); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
		return
	}

	if r.Method == "DELETE" {
		if err := s.km.DeleteKey(keyID); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"Anahtar silindi"}`))
		return
	}

	if r.Method == "PUT" {
		var req struct {
			Name            string `json:"name"`
			Key             string `json:"key"`
			DailyLimit      int64  `json:"daily_limit"`
			ThresholdTokens int64  `json:"threshold_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"Geçersiz JSON"}`, http.StatusBadRequest)
			return
		}
		if err := s.km.UpdateKey(keyID, req.Name, req.Key, req.DailyLimit, req.ThresholdTokens); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"Anahtar güncellendi"}`))
		return
	}

	http.Error(w, "Bilinmeyen eylem", http.StatusNotFound)
}

func (s *EvrenServer) handleResetNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Yalnızca POST kabul edilir", http.StatusMethodNotAllowed)
		return
	}

	s.km.ResetDailyForce()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"success":true,"message":"Tüm kotalar 03:00 TSİ referansıyla sıfırlandı"}`))
}

func (s *EvrenServer) handleUnexhaustAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Yalnızca POST kabul edilir", http.StatusMethodNotAllowed)
		return
	}

	_ = s.km.UnexhaustAll()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"success":true,"message":"Tüm anahtarların kota kilidi kaldırıldı"}`))
}

// handleClearSessionLogs, bir OTURUMUN RAM loglarını sıfırlar: ham istek
// halkası (chat + embedding kayıtları) ve transcript kartı. Yalnız bellek —
// usage_history.jsonl dosyasına ve oturum kimlik defterine dokunulmaz; kart,
// istek geldiğinde aynı kimlikle taze olarak yeniden oluşur.
func (s *EvrenServer) handleClearSessionLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Yalnızca POST kabul edilir", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"Geçersiz JSON"}`, http.StatusBadRequest)
		return
	}
	sid := strings.TrimSpace(req.SessionID)
	if sid == "" {
		http.Error(w, `{"error":"session_id gerekli"}`, http.StatusBadRequest)
		return
	}
	clearedLogs := 0
	if globalLogger != nil {
		clearedLogs = globalLogger.ClearSession(sid)
	}
	clearedConv := false
	if globalTranscripts != nil {
		clearedConv = globalTranscripts.ClearSession(sid)
	}
	// Oturumun logları silinince bağlı anahtarı da HAVUZA GERİ VER
	// (bağsız kimlikte no-op; subagent kartında kök bağı durur).
	released := false
	if s.km != nil {
		released = s.km.Unbind(sid)
	}
	log.Printf("[OTURUM LOG SIFIRLA] %s: %d RAM istek kaydı silindi, transcript kartı=%v, anahtar bağ=%v", sid, clearedLogs, clearedConv, released)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":               true,
		"session_id":            sid,
		"cleared_request_logs":  clearedLogs,
		"cleared_conversation":  clearedConv,
		"released_key_binding":  released,
	})
}

// handleClearAllSessions, TÜM oturum belleğini siler: transcript kartları,
// ham istek halkası ve TÜM oturum→anahtar bağları — havuz TAMAMEN boşa
// düşer, yeni oturumlara sıfırdan dağıtılır. Kimlik defteri (sessions-state)
// ve dosya arşivi (usage_history.jsonl) korunur.
//
//	POST /api/evren/logs/clear-all-sessions
func (s *EvrenServer) handleClearAllSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Yalnızca POST kabul edilir", http.StatusMethodNotAllowed)
		return
	}
	convs, exchanges := 0, 0
	if globalTranscripts != nil {
		convs, exchanges = globalTranscripts.ClearAll()
	}
	logs := 0
	if globalLogger != nil {
		logs = globalLogger.ClearAll()
	}
	released := 0
	if s.km != nil {
		released = s.km.UnbindAll()
	}
	log.Printf("[TÜM OTURUMLAR SIFIRLA] %d kart, %d exchange, %d istek kaydı silindi; %d anahtar bağ koparıldı (havuz boşa düştü)",
		convs, exchanges, logs, released)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":               true,
		"cleared_conversations": convs,
		"cleared_exchanges":     exchanges,
		"cleared_request_logs":  logs,
		"released_key_bindings": released,
	})
}

// imageLimitModel, görsel sınırı ayar paneline sunulan bir model satırıdır.
type imageLimitModel struct {
	Model         string `json:"model"`
	DisplayName   string `json:"display_name"`
	SupportsImage bool   `json:"supports_image"`
	KnownSpec     bool   `json:"known_spec"`
	Seen          bool   `json:"seen"`
	Limit         int    `json:"limit"`
}

// collectImageLimitModels, panelde sunulacak model listesini üretir:
// YALNIZCA (1) isteklerde gerçekten yakalanan modeller (router'dan geçenler) ve
// (2) yapılandırmada zaten sınır tanımlı modeller. Gömülü sabit katalog
// (knownModelSpecs) listeye eklenmez — panel yalnızca gerçek isteklerle dolar;
// katalog yalnızca ad/görsel-rozeti zenginleştirmesinde aranır.
func collectImageLimitModels(cfg EvrenConfig) []imageLimitModel {
	seen := map[string]bool{}
	if globalLogger != nil {
		for _, entry := range globalLogger.GetRecent(1000, "") {
			if entry.Model != "" && entry.Model != "unknown" {
				seen[entry.Model] = true
			}
		}
	}

	all := map[string]bool{}
	for id := range cfg.ModelImageLimits {
		all[id] = true
	}
	for id := range seen {
		all[id] = true
	}

	models := make([]imageLimitModel, 0, len(all))
	for id := range all {
		spec, known := knownModelSpecs[id]
		supportsImage := false
		if known {
			for _, modality := range spec.InputModalities {
				if modality == "image" {
					supportsImage = true
					break
				}
			}
		}
		displayName := cleanModelName(id)
		if known && spec.Name != "" {
			displayName = spec.Name
		}
		limit := 0
		if value, ok := cfg.ModelImageLimits[id]; ok {
			limit = value
		}
		models = append(models, imageLimitModel{
			Model:         id,
			DisplayName:   displayName,
			SupportsImage: supportsImage,
			KnownSpec:     known,
			Seen:          seen[id],
			Limit:         limit,
		})
	}

	// Router'dan geçenler önce, sonra adıyla sırala — panel okunaklı olsun.
	sort.Slice(models, func(i, j int) bool {
		if models[i].Seen != models[j].Seen {
			return models[i].Seen
		}
		if models[i].Limit > 0 != (models[j].Limit > 0) {
			return models[i].Limit > 0
		}
		return models[i].Model < models[j].Model
	})
	return models
}

// writeImageLimitsResponse, güncel ayarları ve model listesini yazar.
func writeImageLimitsResponse(w http.ResponseWriter) {
	cfg := GetConfig()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_ = json.NewEncoder(w).Encode(struct {
		MaxImagesPerRequest int               `json:"max_images_per_request"`
		ModelImageLimits    map[string]int    `json:"model_image_limits"`
		Models              []imageLimitModel `json:"models"`
	}{
		MaxImagesPerRequest: cfg.MaxImagesPerRequest,
		ModelImageLimits:    cfg.ModelImageLimits,
		Models:              collectImageLimitModels(cfg),
	})
}

// handleImageLimits, model bazlı görsel sınırı tablosunu okur (GET) ve kaydeder
// (POST). Kaydetme config.json'a yazılır ve bellekteki yapılandırmayı anında
// günceller — yeniden başlatma gerekmez.
func (s *EvrenServer) handleImageLimits(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeImageLimitsResponse(w)
	case "POST":
		var req struct {
			ModelImageLimits    map[string]int `json:"model_image_limits"`
			MaxImagesPerRequest int            `json:"max_images_per_request"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"Geçersiz JSON"}`, http.StatusBadRequest)
			return
		}
		if err := UpdateImageLimits(evrenConfigFile, req.ModelImageLimits, req.MaxImagesPerRequest); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%s}`, jsonString(err.Error())), http.StatusBadRequest)
			return
		}
		writeImageLimitsResponse(w)
	default:
		http.Error(w, "Metot desteklenmiyor", http.StatusMethodNotAllowed)
	}
}

func (s *EvrenServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	filter := r.URL.Query().Get("filter") // "today" or "all"
	filterDay := ""
	if filter == "today" {
		filterDay = globalLogger.CurrentDayString()
	}

	logs := globalLogger.GetRecent(limit, filterDay)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_ = json.NewEncoder(w).Encode(logs)
}

func (s *EvrenServer) handleEmbeddingLogs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	logs := globalLogger.GetEmbeddingLogs(limit)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_ = json.NewEncoder(w).Encode(logs)
}

// handleAudioLogs, ses (ASR) isteklerini JSON olarak döner — embedding
// günlüğünün birebir eşi; chat tablosunda görünmezler.
func (s *EvrenServer) handleAudioLogs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	logs := globalLogger.GetAudioLogs(limit)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_ = json.NewEncoder(w).Encode(logs)
}

// handleDecisionLogs, KARAR (/v1/systemone — clef, laya) isteklerini JSON
// olarak döner: ses/embedding günlüklerinin eşi. Bu istekler chat tablosunda
// görünmez (metin üretmezler, choice/score/noul yanıtı dönerler).
func (s *EvrenServer) handleDecisionLogs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	logs := globalLogger.GetDecisionLogs(limit)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_ = json.NewEncoder(w).Encode(logs)
}

// handleAutoSwitch API'ler arası otomatik geçişin ana anahtarını yönetir.
// Kapalıyken hiçbir kod yolu (eşik, 429, boş anahtar) apiler arası geçiş yapamaz.
func (s *EvrenServer) handleAutoSwitch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": s.km.AutoSwitchEnabled()})
	case http.MethodPost:
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
			http.Error(w, `{"error":"Geçersiz JSON: 'enabled' boolean alanı gerekli"}`, http.StatusBadRequest)
			return
		}
		s.km.SetAutoSwitch(*req.Enabled)
		message := "Otomatik API geçişi kapatıldı — apiler arası geçiş yasak"
		if *req.Enabled {
			message = "Otomatik API geçişi açıldı — yalnızca havuz kartları arasında geçiş yapılacak"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"success":true,"enabled":%t,"message":%q}`, *req.Enabled, message)))
	default:
		http.Error(w, "Metot desteklenmiyor", http.StatusMethodNotAllowed)
	}
}

// requestBaseURL, isteğin geldiği origin'i (scheme://host) üretir; panelin
// kopyalanabilir tam endpoint URL'leri için kullanılır.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:8088"
	}
	return scheme + "://" + host
}

// handleTranscriptSessions, RAM'deki oturumların listesini ve HER OTURUM İÇİN
// üretilmiş response-uyumlu endpoint URL'ini döner:
//
//	GET /api/evren/logs/transcript/sessions
//
// Liste halka tamponunun tamamını kapsar (son 200 istek / en fazla 50 oturum,
// yalnızca RAM'de; diske yazılmaz).
func (s *EvrenServer) handleTranscriptSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Yalnızca GET kabul edilir", http.StatusMethodNotAllowed)
		return
	}
	sessions, stats := globalTranscripts.ListSessions()
	base := requestBaseURL(r)
	for i := range sessions {
		sessions[i].EndpointURL = base + sessions[i].EndpointPath
		// Oturumun O AN bağlı olduğu havuz anahtarı (subagent kartında kökün
		// anahtarı görünür); bağ yoksa alan boş kalır.
		if s.km != nil {
			if id, name, ok := s.km.BoundKey(chatSessions.Root(sessions[i].ConversationID)); ok {
				sessions[i].APIKeyID = id
				sessions[i].APIKeyName = name
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_ = json.NewEncoder(w).Encode(struct {
		Format        string                  `json:"format"`
		GeneratedAt   string                  `json:"generated_at"`
		IndexURL      string                  `json:"index_url"`        // bu listenin kendi URL'i
		DumpURL       string                  `json:"dump_url"`         // tüm konuşmalar tek gövde
		RequestLogURL string                  `json:"request_log_url"`  // tüm istek günlüğü (karışık)
		Stats         map[string]int          `json:"stats"`
		Sessions      []TranscriptSessionInfo `json:"sessions"`
	}{
		Format:        transcriptFormat,
		GeneratedAt:   tsNow(),
		IndexURL:      base + "/api/evren/logs/transcript/sessions",
		DumpURL:       base + "/api/evren/logs/transcript",
		RequestLogURL: base + "/api/evren/logs?limit=1000",
		Stats:         stats,
		Sessions:      sessions,
	})
}

// handleTranscriptRename, bir oturuma panelden ✏️ ile verilen özel adı takar:
//
//	POST /api/evren/logs/transcript/rename
//	{"conversation_id":"conv-...","display_name":"reelshorts analizi"}
//
// display_name boş gönderiliyorsa ad kaldırılır ve kart conversation_id'ye
// döner. Ad yalnızca RAM'de tutulur (transcript gibi), 80 karakterle sınırlıdır.
func (s *EvrenServer) handleTranscriptRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Yalnızca POST kabul edilir", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ConversationID string `json:"conversation_id"`
		DisplayName    string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"Geçersiz JSON"}`, http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(req.ConversationID)
	if id == "" {
		http.Error(w, `{"error":"conversation_id gerekli"}`, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if utf8.RuneCountInString(name) > maxDisplayNameRunes {
		http.Error(w, fmt.Sprintf(`{"error":%s}`,
			jsonString(fmt.Sprintf("ad en fazla %d karakter olabilir (alındı %d)", maxDisplayNameRunes, utf8.RuneCountInString(name)))),
			http.StatusBadRequest)
		return
	}
	if !globalTranscripts.Rename(id, name) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":%s}`, jsonString("oturum bulunamadı: "+id))))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"conversation_id": id,
		"display_name":    name,
	})
}

// handleTranscriptDump bellekteki tüm yapılandırılmış konuşma transkriptlerini
// (giden istek messages + model yanıtı + hata detayları) tek JSON gövdesinde,
// her kayıt mutlak zaman damgalı olarak döker. Ring buffer: 50 oturum / 200 istek.
func (s *EvrenServer) handleTranscriptDump(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Yalnızca GET kabul edilir", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_, _ = w.Write(globalTranscripts.DumpBytes())
}

// handleTranscriptSession, OTURUM ID'si ile tek bir oturumun logunu döker:
//
//	GET /api/evren/logs/transcript/<oturum_id>
//
// Aynı response-uyumlu (llm-transcript/1.0) şema; oturum bellek ring'inden
// düşmüşse dürüst 404 JSON döner.
func (s *EvrenServer) handleTranscriptSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Yalnızca GET kabul edilir", http.StatusMethodNotAllowed)
		return
	}
	const marker = "transcript/"
	idx := strings.LastIndex(r.URL.Path, marker)
	if idx < 0 || idx+len(marker) >= len(r.URL.Path) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"oturum id'si eksik"}`))
		return
	}
	id := r.URL.Path[idx+len(marker):]
	if decoded, err := url.PathUnescape(id); err == nil {
		id = decoded
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	data, ok := globalTranscripts.DumpConversationBytes(id)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":%s}`, jsonString("oturum bulunamadı: "+id))))
		return
	}
	_, _ = w.Write(data)
}
