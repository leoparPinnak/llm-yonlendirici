package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Bellek halka tamponu sınırları — RAM patlamasını önler:
// en fazla son 50 konuşma ve toplamda son 200 istek (exchange) bellekte tutulur.
// Kayıtlar diske YAZILMAZ; yalnızca RAM'de yaşar ve endpoint'lere dökülür.
const (
	maxTranscriptConversations = 50
	maxTranscriptExchanges     = 200
	maxTranscriptRawBody       = 64 << 10 // ham yanıt gövdesi üst sınırı (64 KiB)
	maxTranscriptMessageBytes  = 64 << 10 // tek mesaj içerik sınırı (64 KiB)
	maxTranscriptMessages      = 4000     // bir konuşmada tutulan en fazla mesaj
)

// TranscriptSessionInfo oturum listesi endpoint'inin bir satırı; her satır o
// oturuma özel üretilmiş endpoint URL'ini de taşır.
type TranscriptSessionInfo struct {
	ConversationID  string   `json:"conversation_id"`
	DisplayName     string   `json:"display_name,omitempty"` // kullanıcı verdiği özel ad
	Source          string   `json:"source"`
	FirstSeenAt     string   `json:"first_seen_at"`
	LastSeenAt      string   `json:"last_seen_at"`
	RequestCount    int      `json:"request_count"`    // oturumun toplam isteği
	StoredExchanges int      `json:"stored_exchanges"` // ring'de kalan tur sayısı
	MessageCount    int      `json:"message_count"`    // tutulan bağlam mesajı
	Models          []string `json:"models"`
	EndpointPath    string   `json:"endpoint_path"`           // göreli yol (origin'siz)
	EndpointURL     string   `json:"endpoint_url,omitempty"`  // handler'da üretilen tam URL
	APIKeyID        string   `json:"api_key_id,omitempty"`    // oturumun bağlı olduğu kök anahtar
	APIKeyName      string   `json:"api_key_name,omitempty"`
}

// transcriptFormat, dump çıktısının taşdığı şema: OpenAI Chat Completions
// mesaj şeması + OpenTelemetry GenAI semantic conventions alan adları.
// Yapay zekâ okuyucular için tek JSON gövdesinde tam sohbet transkripti üretir.
const transcriptFormat = "llm-transcript/1.0 (OpenAI Chat Completions messages + OpenTelemetry GenAI semconv)"

var transcriptLoc = time.FixedZone("UTC+3", 3*60*60)

func tsNow() string { return time.Now().In(transcriptLoc).Format(time.RFC3339Nano) }

// TranscriptMessage tek bir protokol mesajı; içerik ham (compact) JSON olarak
// saklanır ki string/array/tool parçaları bozulmasın. Her mesaj zaman damgalıdır.
type TranscriptMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Timestamp string          `json:"timestamp"`
}

// TranscriptRequest giden isteğin protokol-seviyesindeki dökümü.
type TranscriptRequest struct {
	ReceivedAt     string              `json:"received_at"`
	Model          string              `json:"model"`
	Stream         bool                `json:"stream"`
	Params         map[string]any      `json:"params,omitempty"`
	Messages       []TranscriptMessage `json:"messages"`     // isteğin getirdiği TAM bağlam (zaman damgalı)
	MessagesNew    []TranscriptMessage `json:"messages_new"` // bu turda bağlama YENİ eklenen mesajlar (delta)
	ConversationID string              `json:"conversation_id"`
	ParentSessionID string             `json:"parent_session_id,omitempty"` // subagent doğurucusu (X-Parent-Session-Id)
	Source         string              `json:"source"` // "evren" | "zen"
	APIKeyID       string              `json:"api_key_id,omitempty"`
	APIKeyName     string              `json:"api_key_name,omitempty"`
}

// TranscriptResponse dönen yanıtın protokol-seviyesindeki dökümü; hata varsa
// tam içeriği Error alanında birebir tutulur.
type TranscriptResponse struct {
	CompletedAt  string              `json:"completed_at"`
	StatusCode   int                 `json:"status_code"`
	Message      *TranscriptMessage  `json:"message,omitempty"` // role=assistant, zaman damgalı
	FinishReason string              `json:"finish_reason,omitempty"`
	Stream       bool                `json:"stream"`
	Usage        *UsageResult        `json:"usage,omitempty"`
	Error        string              `json:"error,omitempty"` // upstream ham hatası (HTTP kodu + gövde) veya stream kopması
	Raw          json.RawMessage     `json:"raw,omitempty"`   // ham yanıt gövdesi (en fazla 64 KiB)
	DurationMs   int64               `json:"duration_ms"`
}

// TranscriptExchange bir istek-yanıt çifti: mutlak zaman damgalı.
type TranscriptExchange struct {
	ExchangeID string             `json:"exchange_id"`
	Request    TranscriptRequest  `json:"request"`
	Response   TranscriptResponse `json:"response"`
}

// TranscriptConversation tek bir sohbet/oturum; Messages alanındaki bağlam
// gelen isteklerle dinamik olarak büyür, Exchanges tüm turları tutar.
type TranscriptConversation struct {
	ConversationID string                `json:"conversation_id"`
	Source         string                `json:"source"`
	FirstSeenAt    string                `json:"first_seen_at"`
	LastSeenAt     string                `json:"last_seen_at"`
	RequestCount   int                   `json:"request_count"` // UI canlı metriği: toplam istek
	Models         []string              `json:"models"`
	Messages       []TranscriptMessage   `json:"messages"` // dinamik büyüyen tam bağlam
	Exchanges      []*TranscriptExchange `json:"exchanges"`
	// DisplayName, kullanıcının panelde verdiği özel ad. Verilmediğinde UI
	// conversation_id'yi gösterir. Yalnızca RAM'de yaşar (transcript gibi).
	DisplayName string `json:"display_name,omitempty"`
	// Bu oturumun bağlı olduğu havuz anahtarı (kök oturum bağlamı — subagent
	// kartlarında kökün anahtarı görünür). Panelden okunur.
	APIKeyID   string `json:"api_key_id,omitempty"`
	APIKeyName string `json:"api_key_name,omitempty"`
}

// TranscriptDump GET /api/evren/logs/transcript yanıt gövdesi.
type TranscriptDump struct {
	Format        string                    `json:"format"`
	GeneratedAt   string                    `json:"generated_at"`
	Stats         map[string]int            `json:"stats"`
	Conversations []*TranscriptConversation `json:"conversations"`
}

// TranscriptStore bellekte tutulan, halka tamponu sınırlı transkript deposudur.
// Kayıtlar dosyaya yazılmaz; yalnızca RAM'de yaşar ve dump endpoint'ine dökülür.
type TranscriptStore struct {
	mu           sync.Mutex
	convs        []*TranscriptConversation // eski → yeni
	byID         map[string]*TranscriptConversation
	byFP         map[string]string // ilk kullanıcı mesajı fp → konuşma id (benimseme)
	exchanges    int               // toplam exchange (ring sayacı)
	loc          *time.Location
	maxConvs     int
	maxExchanges int
}

// globalTranscripts ana transkript deposudur; main() içinde InitTranscripts ile açılır.
var globalTranscripts *TranscriptStore

// InitTranscripts deposu hazırlar ve global'e bağlar (nil kapasiteler default'a düşer).
func InitTranscripts(maxConvs, maxExchanges int) *TranscriptStore {
	if maxConvs <= 0 {
		maxConvs = maxTranscriptConversations
	}
	if maxExchanges <= 0 {
		maxExchanges = maxTranscriptExchanges
	}
	globalTranscripts = &TranscriptStore{
		byID:         make(map[string]*TranscriptConversation),
		byFP:         make(map[string]string),
		loc:          transcriptLoc,
		maxConvs:     maxConvs,
		maxExchanges: maxExchanges,
	}
	return globalTranscripts
}

// compactJSON içeriği boşluksuz standart biçime indirir; böylece istekler
// arası biçim farkları (beyaz alan) yanlış bölünmeye yol açmaz.
func compactJSON(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.Bytes()
	}
	return raw
}

func clipRaw(raw json.RawMessage, limit int) json.RawMessage {
	if len(raw) <= limit {
		return raw
	}
	// Geçerli JSON string olarak kırp: içerik tırnakları string'i bozmasın.
	s, err := json.Marshal(fmt.Sprintf("%s… [kırpıldı: toplam %d bayt]", string(raw[:limit]), len(raw)))
	if err != nil {
		return json.RawMessage(`"[truncated]"`)
	}
	return s
}

// rawJSONOrString bir ham gövdeyi JSON RawMessage olarak saklar: geçerli JSON
// ise compact, değilse JSON string kodlar; uzun gövdeler kırpılır.
func rawJSONOrString(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(body)
	if json.Valid(trimmed) {
		if len(trimmed) > maxTranscriptRawBody {
			s, _ := json.Marshal(fmt.Sprintf("[kırpıldı: toplam %d bayt ham gövde]", len(trimmed)))
			return json.RawMessage(s)
		}
		return json.RawMessage(bytes.Clone(trimmed))
	}
	s := string(trimmed)
	if len(s) > maxTranscriptRawBody {
		s = s[:maxTranscriptRawBody] + fmt.Sprintf("… [kırpıldı: toplam %d bayt]", len(trimmed))
	}
	out, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`{"error":"encode edilemedi"}`)
	}
	return out
}

// parseTranscriptMessages "messages" dizisini protokol mesajlarına çevirir
// ve her mesajı verilen an damgasıyla işaretler.
func parseTranscriptMessages(raw json.RawMessage, ts string) []TranscriptMessage {
	var items []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]TranscriptMessage, 0, len(items))
	for _, it := range items {
		if it.Role == "" {
			continue
		}
		content := compactJSON(it.Content)
		if len(content) > maxTranscriptMessageBytes {
			clipped, _ := json.Marshal(fmt.Sprintf("[truncated: %d bayt içerik kırpıldı]", len(content)))
			content = clipped
		}
		out = append(out, TranscriptMessage{Role: it.Role, Content: content, Timestamp: ts})
	}
	return out
}

// BuildTranscriptRequest bir chat-completions gövdesini protokol-seviyesinde
// ayrıştırır; messages içermeyen gövdelerde (models vb.) nil döner.
func BuildTranscriptRequest(body []byte, source string, receivedAt time.Time) *TranscriptRequest {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil
	}
	rawMessages, ok := fields["messages"]
	if !ok {
		return nil
	}
	ts := receivedAt.In(transcriptLoc).Format(time.RFC3339Nano)
	messages := parseTranscriptMessages(rawMessages, ts)
	if len(messages) == 0 {
		return nil
	}

	req := &TranscriptRequest{
		ReceivedAt: ts,
		Messages:   messages,
		Source:     source,
	}

	if raw, ok := fields["model"]; ok {
		_ = json.Unmarshal(raw, &req.Model)
	}
	if raw, ok := fields["stream"]; ok {
		_ = json.Unmarshal(raw, &req.Stream)
	}

	// Üretim parametreleri (messages/model/stream/tools hariç)
	for name, raw := range fields {
		switch name {
		case "messages", "model", "stream", "tools":
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) == nil {
			if req.Params == nil {
				req.Params = make(map[string]any, 4)
			}
			req.Params[name] = v
		}
	}

	// Açık oturum ipuçları: conversation_id / session_id / metadata.*
	var hint string
	for _, key := range []string{"conversation_id", "session_id"} {
		if raw, ok := fields[key]; ok {
			_ = json.Unmarshal(raw, &hint)
			if hint != "" {
				break
			}
		}
	}
	if hint == "" {
		if raw, ok := fields["metadata"]; ok {
			var md map[string]json.RawMessage
			if json.Unmarshal(raw, &md) == nil {
				for _, key := range []string{"conversation_id", "session_id"} {
					if v, ok := md[key]; ok {
						_ = json.Unmarshal(v, &hint)
						if hint != "" {
							break
						}
					}
				}
			}
		}
	}
	req.ConversationID = hint
	return req
}

// HeaderConversationHint, istek başlığındaki oturum ipucunu okur — kademe 1'in
// header uzantısı: X-Conversation-Id veya X-Session-Id. İstemci gövdeye
// conversation_id yazamıyorsa bu başlığı göndermesi yeterlidir (öncelik:
// gövde id > header id > içerik prefix eşleşmesi).
func HeaderConversationHint(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, h := range []string{"X-Conversation-Id", "X-Session-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}

// HeaderParentSessionHint, subagent isteğinin doğurucusunu okur:
// X-Parent-Session-Id. Alt oturum transcript kartını korur; child→parent
// bağı kayıt defterine (sessions-state.json) yazılır ve anahtar ataması
// kök oturuma görelenir.
func HeaderParentSessionHint(r *http.Request) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Parent-Session-Id"))
}

// CanonicalSessionID, DSH'den gelen oturum kimliklerine panelde ayırt edici
// "dsh-" öneki katar (`session-…` → `dsh-session-…`).
//
// GERİ UYUM KURALI: altındaki kimliğin KENDİ KARTI transkriptte ya da kayıt
// defterinde zaten varsa (önceki sürümde açılmış) ham kimlik KORUNUR — bir
// oturum ikiye bölünmez, eski kart büyümeye devam eder. Yeni oturumlar önekli
// açılır. `session-` ile başlamayan kimlikler (`evs-…`, `conv-…`, test kartları)
// aynen geçer. Subagent'ın X-Parent-Session-Id değeri de AYNI kuraldan geçer
// ki kök, çocuğun gördüğü kimlikle birebir aynı olsun.
func CanonicalSessionID(id string) string {
	if id == "" || !strings.HasPrefix(id, "session-") {
		return id
	}
	if globalTranscripts != nil && globalTranscripts.HasConversation(id) {
		return id
	}
	if chatSessions != nil && chatSessions.Knows(id) {
		return id
	}
	return "dsh-" + id
}

// HasConversation, kimliğin transkriptte kartı olup olmadığını sorar
// (CanonicalSessionID geri-uyum kontrolü; kilit altında).
func (s *TranscriptStore) HasConversation(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.byID[id]
	return ok
}

// ClearAll, TÜM transcript kartlarını ve istek kayıtlarını RAM'den siler
// ("tüm oturum belleği"). Kimlik defteri (sessions-state) ve dosya arşivi
// DOKUNULMAZ — yeni istek aynı kimliklerle taze kartlar açar.
// Silinen (kart, exchange) sayılarını döner.
func (s *TranscriptStore) ClearAll() (int, int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	convs, exchanges := len(s.convs), s.exchanges
	s.convs = nil
	s.byID = make(map[string]*TranscriptConversation)
	s.byFP = make(map[string]string)
	s.exchanges = 0
	return convs, exchanges
}

// extractAssistantMessage bir chat yanıt gövdesinden asistan mesajını ve
// finish_reason'u ayrıştırır (OpenAI tam yanıt şeması).
func extractAssistantMessage(body []byte, at time.Time) (*TranscriptMessage, string) {
	var parsed struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
			Message      *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Choices) == 0 {
		return nil, ""
	}
	choice := parsed.Choices[0]
	finish := ""
	if choice.FinishReason != nil {
		finish = *choice.FinishReason
	}
	if choice.Message == nil || len(choice.Message.Content) == 0 {
		return nil, finish
	}
	msg := &TranscriptMessage{
		Role:      "assistant",
		Content:   clipRaw(compactJSON(choice.Message.Content), maxTranscriptMessageBytes),
		Timestamp: at.In(transcriptLoc).Format(time.RFC3339Nano),
	}
	return msg, finish
}

// lcpLen iki mesaj serisinde rol+içerik eşleşmesinin uzunluğunu verir.
func lcpLen(a, b []TranscriptMessage) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i].Role == b[i].Role && bytes.Equal(a[i].Content, b[i].Content) {
		i++
	}
	return i
}

// lookupByFP, fp'nin daha önce açılmış bir konuşmada olup olmadığını söyler.
// İlk eşleşen (en eski) konuşma benimsenir — "aynı fp = tek oturum" ve
// ÖNCEDEN bölünmüş kartlar (zen LCP dönemi) yeni kart AÇILMADAN birleşir.
// İndeks tembel kurulur: eski konuşmalar ilk aramada taranır.
func (s *TranscriptStore) lookupByFP(fp string) (string, bool) {
	if fp == "" {
		return "", false
	}
	if id, ok := s.byFP[fp]; ok {
		if _, exists := s.byID[id]; exists {
			return id, true
		}
		delete(s.byFP, fp) // konuşma ring'den düşmüş
	}
	for _, conv := range s.convs {
		cfp := sessionFingerprintMsgs(conv.Messages)
		if cfp == "" {
			continue
		}
		if _, done := s.byFP[cfp]; !done {
			s.byFP[cfp] = conv.ConversationID
		}
	}
	if id, ok := s.byFP[fp]; ok {
		if _, exists := s.byID[id]; exists {
			return id, true
		}
	}
	return "", false
}

// resolveConversation gelen isteği mevcut bir konuşmaya bağlar:
//  1. İstekte açık conversation_id/session_id varsa o oturum (KAYNAKTAN
//     BAĞIMSIZ — aynı oturum hem zen hem evren yolundan aynı karta düşer).
//  2. Yoksa, gelen mesaj serisi bir uzantı olduğu (prefix-match) konuşma.
// Eşleşme yoksa yeni konuşma açılır — böylece dallanan bağlam bozulmaz.
func (s *TranscriptStore) resolveConversation(req *TranscriptRequest) *TranscriptConversation {
	if req.ConversationID != "" {
		if conv, ok := s.byID[req.ConversationID]; ok {
			return conv
		}
		return nil // yeni oturum: bilinen id yok
	}
	var best *TranscriptConversation
	bestLCP := 0
	for _, conv := range s.convs {
		if conv.Source != req.Source {
			continue
		}
		n := lcpLen(conv.Messages, req.Messages)
		if n == 0 {
			continue
		}
		if n < len(conv.Messages) && n < len(req.Messages) {
			// Kısmi örtüşme = dallanma/düzenleme: bu bağlam BOZULMAZ,
			// yeni bir konuşma açılır.
			continue
		}
		if n > bestLCP {
			bestLCP = n
			best = conv
		}
	}
	if bestLCP == 0 {
		return nil
	}
	return best
}

// extendConversation konuşmayı gelen bağlamla hizalar: uzantı ise arkaya
// ekler, dallanma/düzenleme ise ortak noktadan devam eder. Delta hesaplanır.
func (s *TranscriptStore) extendConversation(conv *TranscriptConversation, req *TranscriptRequest) {
	n := lcpLen(conv.Messages, req.Messages)
	if n < len(conv.Messages) {
		// Dalanma/düzenleme: bağlam gelen isteğin sürümünden devam eder.
		conv.Messages = conv.Messages[:n]
	}
	for i := n; i < len(req.Messages); i++ {
		conv.Messages = append(conv.Messages, req.Messages[i])
	}
	req.MessagesNew = req.Messages[n:]

	if len(conv.Messages) > maxTranscriptMessages {
		conv.Messages = conv.Messages[len(conv.Messages)-maxTranscriptMessages:]
	}
}

func (s *TranscriptStore) newConversationID(req *TranscriptRequest, now string) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%s|%d", req.Source, now, string(req.Messages[0].Content), len(s.convs))))
	return "conv-" + hex.EncodeToString(h[:])[:12]
}

// Record istek+yanıt çiftini konuşmaya işler; mutlak zaman damgalıdır.
func (s *TranscriptStore) Record(req *TranscriptRequest, resp TranscriptResponse) {
	if s == nil || req == nil || len(req.Messages) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// ORTAK OTURUM TESPİTİ (evren_session.go): boş kimlik önce panoda var
	// olan konuşmayı benimser (fp indeksi — bölünmüş kartlar birleşir),
	// sonra registry çözer (fp takma adı + compaction köprüsü). Kimlik hiç
	// bulunamazsa boş kalır ve eski LCP akışı devreye girer.
	fp := sessionFingerprintMsgs(req.Messages)
	if req.ConversationID == "" && fp != "" {
		if id, ok := s.lookupByFP(fp); ok {
			req.ConversationID = id
			chatSessions.Alias(fp, id)
		} else if id, _ := chatSessions.Lookup(fp, req.Messages); id != "" {
			req.ConversationID = id
		} else {
			req.ConversationID = chatSessions.Resolve("", "", fp, req.Messages, 0).ID
		}
	}
	// Açık/üretilen her kimlik ÖNCE deftere yazılır (evren+zen ortak): fp
	// alias'ı "ilk bağlanan kazanır" gardına takılsa bile kimlik kalıcı olsun
	// — CanonicalSessionID'in geri-uyum hafızası da buradan beslenir.
	if req.ConversationID != "" {
		chatSessions.Ensure(req.ConversationID)
	}
	// Sonra fp'ye bağlanır: sonraki id'siz istek (hangi yoldan gelirse
	// gelsin) aynı karta düşer.
	if fp != "" && req.ConversationID != "" {
		chatSessions.Alias(fp, req.ConversationID)
	}

	conv := s.resolveConversation(req)
	if conv == nil {
		// Yeni konuşma: ilk bağlam kuru olarak kurulur (delta = tüm mesajlar).
		firstSeen := req.ReceivedAt
		conv = &TranscriptConversation{
			ConversationID: req.ConversationID,
			Source:         req.Source,
			FirstSeenAt:    firstSeen,
		}
		if conv.ConversationID == "" {
			conv.ConversationID = s.newConversationID(req, firstSeen)
		}
		s.convs = append(s.convs, conv)
		s.byID[conv.ConversationID] = conv
		// fp indeksine yaz: sonraki aynı-fp istek bu kartı benimsesin.
		if fp != "" {
			if _, ok := s.byFP[fp]; !ok {
				s.byFP[fp] = conv.ConversationID
			}
		}
		// İlk bağlamı kuru olarak kur: delta = tüm mesajlar.
		conv.Messages = append(conv.Messages, req.Messages...)
		req.MessagesNew = req.Messages
	} else {
		s.extendConversation(conv, req)
	}

	// Subagent soy kaydı: alt oturum KARTINI KORUR, child→parent bağı
	// yalnızca kayıt defterine yazılır (kimlik/atama katmanı kök oturumu
	// görecektir).
	if req.ParentSessionID != "" && conv.ConversationID != req.ParentSessionID {
		chatSessions.SetParent(conv.ConversationID, req.ParentSessionID)
	}

	conv.LastSeenAt = resp.CompletedAt
	if conv.LastSeenAt == "" {
		conv.LastSeenAt = tsNow()
	}
	conv.RequestCount++

	if req.Model != "" {
		found := false
		for _, m := range conv.Models {
			if m == req.Model {
				found = true
				break
			}
		}
		if !found {
			conv.Models = append(conv.Models, req.Model)
		}
	}

	// Asistan yanıtı da bağlama katılır: bir sonraki istek bu sırayı getirir.
	if resp.Message != nil {
		conv.Messages = append(conv.Messages, *resp.Message)
	}

	ex := &TranscriptExchange{
		ExchangeID: fmt.Sprintf("req-%d", time.Now().UnixNano()),
		Request:    *req,
		Response:   resp,
	}
	conv.Exchanges = append(conv.Exchanges, ex)
	s.exchanges++
	s.evict()
}

// ClearSession, bir oturumun transcript KARTINI RAM deposundan siler (mesajlar
// + exchange'ler). Kimlik defterine (sessions-state) DOKUNMAZ: oturum yeniden
// başladığında aynı kimliğiyle taze kart açılır. fp indeksi ve sayaçlar
// temizlenir. Kart bulunamazsa false döner.
func (s *TranscriptStore) ClearSession(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	conv, ok := s.byID[id]
	if !ok {
		return false
	}
	delete(s.byID, id)
	for i, c := range s.convs {
		if c == conv {
			s.convs = append(s.convs[:i], s.convs[i+1:]...)
			break
		}
	}
	for fp, target := range s.byFP {
		if target == id {
			delete(s.byFP, fp)
		}
	}
	s.exchanges -= len(conv.Exchanges)
	if s.exchanges < 0 {
		s.exchanges = 0
	}
	return true
}

// evict halka tamponu sınırlarını uygular: önce toplam exchange (200),
// sonra konuşma sayısı (50).
func (s *TranscriptStore) evict() {
	for s.exchanges > s.maxExchanges && len(s.convs) > 0 {
		conv := s.convs[0]
		if len(conv.Exchanges) == 0 {
			// Boş konuşmayı tamamen düşür
			delete(s.byID, conv.ConversationID)
			s.convs = s.convs[1:]
			continue
		}
		conv.Exchanges = conv.Exchanges[1:]
		s.exchanges--
	}
	for len(s.convs) > s.maxConvs {
		oldest := s.convs[0]
		s.exchanges -= len(oldest.Exchanges)
		if s.exchanges < 0 {
			s.exchanges = 0
		}
		delete(s.byID, oldest.ConversationID)
		s.convs = s.convs[1:]
	}
}

// ListSessions RAM'deki oturumları en yeniden eskiye sırayla özetler ve
// halka tamponu istatistiklerini döndürür. Her satır, o oturumun
// response-uyumlu tekil endpoint yolunu da taşır (URL handler'da tamamlanır).
// Nil güvenlidir.
func (s *TranscriptStore) ListSessions() ([]TranscriptSessionInfo, map[string]int) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]TranscriptSessionInfo, 0, len(s.convs))
	for i := len(s.convs) - 1; i >= 0; i-- {
		conv := s.convs[i]
		models := make([]string, len(conv.Models))
		copy(models, conv.Models)
		out = append(out, TranscriptSessionInfo{
			ConversationID:  conv.ConversationID,
			DisplayName:     conv.DisplayName,
			Source:          conv.Source,
			FirstSeenAt:     conv.FirstSeenAt,
			LastSeenAt:      conv.LastSeenAt,
			RequestCount:    conv.RequestCount,
			StoredExchanges: len(conv.Exchanges),
			MessageCount:    len(conv.Messages),
			Models:          models,
			EndpointPath:    "/api/evren/logs/transcript/" + url.PathEscape(conv.ConversationID),
		})
	}
	stats := map[string]int{
		"conversations":     len(s.convs),
		"exchanges":         s.exchanges,
		"max_conversations": s.maxConvs,
		"max_exchanges":     s.maxExchanges,
	}
	return out, stats
}

// Rename, bir oturuma panelden verilen özel adı takar (veya boş adla kaldırır).
// Belli bir conversation_id'nin tekilleştirilmiş olması garantisine dayanır.
// Bulunamazsa ok=false döner; kimlik doğrulaması handler'da yapılır.
// Yalnızca RAM: süreç yeniden başlayınca ad da sıfırlanır (transcript gibi).
func (s *TranscriptStore) Rename(id, name string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	conv, ok := s.byID[id]
	if !ok || conv == nil {
		return false
	}
	conv.DisplayName = strings.TrimSpace(name)
	return true
}

// DumpBytes tüm yapılandırılmış transkriptleri tek JSON gövdesinde, en yeniden
// eskiye sırayla döndürür. Nil güvenlidir.
func (s *TranscriptStore) DumpBytes() []byte {
	if s == nil {
		return []byte(`{}` + "\n")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	convs := make([]*TranscriptConversation, 0, len(s.convs))
	for i := len(s.convs) - 1; i >= 0; i-- {
		convs = append(convs, s.convs[i])
	}
	dump := TranscriptDump{
		Format:      transcriptFormat,
		GeneratedAt: tsNow(),
		Stats: map[string]int{
			"conversations":    len(s.convs),
			"exchanges":        s.exchanges,
			"max_conversations": s.maxConvs,
			"max_exchanges":     s.maxExchanges,
		},
		Conversations: convs,
	}
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return []byte(`{"error":"transcript dump encode edilemedi"}` + "\n")
	}
	return append(data, '\n')
}

// DumpConversationBytes, TEK bir oturumun (conversation) transkriptini aynı
// response-uyumlu şemada (llm-transcript/1.0) döker — oturum bazlı backend
// endpoint'i için: /api/evren/logs/transcript/<oturum_id>. Bulunamazsa ok=false.
func (s *TranscriptStore) DumpConversationBytes(id string) ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	conv, ok := s.byID[id]
	if !ok || conv == nil {
		return nil, false
	}

	dump := TranscriptDump{
		Format:      transcriptFormat,
		GeneratedAt: tsNow(),
		Stats: map[string]int{
			"conversations":    1,
			"exchanges":        len(conv.Exchanges),
			"max_conversations": s.maxConvs,
			"max_exchanges":     s.maxExchanges,
		},
		Conversations: []*TranscriptConversation{conv},
	}
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return []byte(`{"error":"transcript dump encode edilemedi"}` + "\n"), true
	}
	return append(data, '\n'), true
}
