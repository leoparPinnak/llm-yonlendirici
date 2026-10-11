package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type RequestLog struct {
	ID           string      `json:"id"`
	Timestamp    string      `json:"timestamp"`
	Day          string      `json:"day"` // YYYY-MM-DD formatında gün (03:00 TSİ sınırına göre)
	KeyID        string      `json:"key_id"`
	KeyName      string      `json:"key_name"`
	Model        string      `json:"model"`
	Method       string      `json:"method"`
	Path         string      `json:"path"`
	StatusCode   int         `json:"status_code"`
	DurationMs   int64       `json:"duration_ms"`
	Stream       bool        `json:"stream"`
	Usage        UsageResult `json:"usage"`
	Rotated      bool        `json:"rotated"`
	NewKeyID     string      `json:"new_key_id,omitempty"`
	ErrorMessage string      `json:"error_message,omitempty"`
	IsEmbedding  bool        `json:"is_embedding"`
	// IsAudio, ses (ASR / konuşma tanıma) isteklerini işaretler: embedding gibi
	// chat tablosundan hariç tutulur, kendi günlük görünümünde listelenir ve
	// ayrı sayaçlara (AudioTokens/AudioRequests) işlenir.
	// IsDecision ise KARAR isteklerini (/v1/systemone — clef, laya) işaretler:
	// bu modeller metin üretmez, choice/score/noul sorularını olasılıklarıyla
	// yanıtlar; kendi bölümünde listelenir, chat tablosunda görünmez.
	// SourceFile ise isteğin paneldeki yükleme penceresinden geldiği dosya adıdır
	// (X-Source-File başlığı; upstream'e gönderilmez). Ses isteklerinde ad
	// multipart gövdeden de okunur; embedding gövdesi yalnızca metin olduğu için
	// "hangi dosya vektörlendi" bilgisi yalnızca buradan gelir.
	IsAudio      bool   `json:"is_audio"`
	IsDecision   bool   `json:"is_decision"`
	AudioSeconds int64  `json:"audio_seconds,omitempty"`
	SourceFile   string `json:"source_file,omitempty"`
	InputPreview string `json:"input_preview,omitempty"`
	// OTURUM TESPİTİ (evren_session.go): isteğin çözümlenmiş oturum anahtarı.
	// session_id mantıksal oturum (explicit > header > fp > bridge),
	// session_fp ilk kullanıcı mesajının parmak izi — upstream'in oturum
	// anahtarı adayı. client_headers yalnızca oturum taşıyabilecek
	// başlıkları (authorization/cookie hariç) tutar.
	SessionID      string            `json:"session_id,omitempty"`
	SessionTier    string            `json:"session_tier,omitempty"`
	SessionFP      string            `json:"session_fp,omitempty"`
	SessionPreview string            `json:"session_preview,omitempty"`
	ClientPID      int               `json:"client_pid,omitempty"`
	ClientProc     string            `json:"client_proc,omitempty"`
	ClientHeaders  map[string]string `json:"client_headers,omitempty"`
	// Detay gövdeleri (UI "detay göster" paneli için): yalnızca bellekte
	// tutulur, usage_history.jsonl dosyasına YAZILMAZ. 64 KiB üstü kırpılır.
	RequestBody  string `json:"request_body,omitempty"`
	ResponseBody string `json:"response_body,omitempty"`
}

type MemoryLogger struct {
	mu        sync.RWMutex
	filePath  string
	capacity  int
	entries   []RequestLog
	turkeyLoc *time.Location
}

var globalLogger *MemoryLogger

// maxRequestLogEntries, bellekteki RequestLog halka tamponu sınırıdır:
// en fazla son 1000 istek RAM'de tutulur (usage_history.jsonl dosyaya akar).
const maxRequestLogEntries = 1000

// maxDetailBody, istek/yanıt detay gövdesinin bellekte saklanacağı üst sınırdır.
const maxDetailBody = 64 << 10

// clipDetailBody gövdeyi 64 KiB'e kadar tutar; aşırsa kırpma izi bırakır.
func clipDetailBody(s string) string {
	if len(s) <= maxDetailBody {
		return s
	}
	return s[:maxDetailBody] + fmt.Sprintf("\n… [kırpıldı: toplam %d bayt]", len(s))
}

func InitLogger(filePath string, capacity int) *MemoryLogger {
	if capacity <= 0 {
		capacity = maxRequestLogEntries
	}
	loc := time.FixedZone("UTC+3", 3*60*60)
	globalLogger = &MemoryLogger{
		filePath:  filePath,
		capacity:  capacity,
		entries:   make([]RequestLog, 0, capacity),
		turkeyLoc: loc,
	}
	return globalLogger
}

func (l *MemoryLogger) Log(entry RequestLog) {
	now := time.Now().In(l.turkeyLoc)
	if entry.Timestamp == "" {
		entry.Timestamp = now.Format("2006-01-02 15:04:05")
	}
	if entry.Day == "" {
		shifted := now.Add(-3 * time.Hour)
		entry.Day = shifted.Format("2006-01-02")
	}

	l.mu.Lock()
	if len(l.entries) >= l.capacity {
		l.entries = l.entries[1:]
	}
	l.entries = append(l.entries, entry)
	l.mu.Unlock()

	// Append to JSONL file asynchronously (gövdeler dosyaya yazılmaz: dosya ince kalır)
	go func(e RequestLog) {
		e.RequestBody = ""
		e.ResponseBody = ""
		data, err := json.Marshal(e)
		if err != nil {
			return
		}
		f, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write(append(data, '\n'))
	}(entry)
}

func (l *MemoryLogger) GetRecent(limit int, filterDay string) []RequestLog {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var filtered []RequestLog
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if e.IsEmbedding || e.IsAudio || e.IsDecision {
			continue // Chat tablosunda embedding, ses (ASR) ve karar istekleri hariç tutulur
		}
		if filterDay != "" && e.Day != filterDay {
			continue
		}
		filtered = append(filtered, e)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
}

func (l *MemoryLogger) GetEmbeddingLogs(limit int) []RequestLog {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var filtered []RequestLog
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if !e.IsEmbedding {
			continue
		}
		filtered = append(filtered, e)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
}

// GetAudioLogs, ses (ASR) isteklerini en yeniden eskiye döner; chat
// tablosunda görünmezler, kendi "Ses (ASR) İstek Günlüğü" bölümünde listelenir.
func (l *MemoryLogger) GetAudioLogs(limit int) []RequestLog {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var filtered []RequestLog
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if !e.IsAudio {
			continue
		}
		filtered = append(filtered, e)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
}

// GetDecisionLogs, KARAR (/v1/systemone — clef, laya) isteklerini en yeniden
// eskiye döner; chat tablosunda görünmezler, kendi "Karar" bölümünde listelenir.
func (l *MemoryLogger) GetDecisionLogs(limit int) []RequestLog {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var filtered []RequestLog
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if !e.IsDecision {
			continue
		}
		filtered = append(filtered, e)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
}

func (l *MemoryLogger) CurrentDayString() string {
	shifted := time.Now().In(l.turkeyLoc).Add(-3 * time.Hour)
	return shifted.Format("2006-01-02")
}

// ClearSession, bir oturuma ait TÜM RAM halka kayıtlarını (chat + embedding +
// ses/ASR) siler; silinen kayıt sayısı döner. Yalnız bellek:
// usage_history.jsonl dosyası arşiv olarak akmaya devam eder, oturum
// kimliğine (registry) dokunulmaz.
func (l *MemoryLogger) ClearSession(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.entries[:0]
	cleared := 0
	for _, e := range l.entries {
		if e.SessionID == sessionID {
			cleared++
			continue
		}
		kept = append(kept, e)
	}
	l.entries = kept
	return cleared
}

// ClearAll, RAM halkasının TAMAMİNİ siler (tüm oturum belleği temizliği).
// usage_history.jsonl dosyası arşiv olarak kalır. Silinen kayıt sayısı döner.
func (l *MemoryLogger) ClearAll() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.entries)
	l.entries = l.entries[:0]
	return n
}
