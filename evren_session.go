package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// ==== OTURUM TESPİTİ ==========================================================
//
// EVREN upstream'i istekleri anahtar bazlı dakikalık pencereyle karşılar ve
// kullanıcı-lane'de "tek oturum" kavramı panelin tek gerçeğidir. Bu dosya
// ZEN ve EVREN yolunun ORTAK oturum çözümlerini taşır: transcript_store
// (her iki yolun da kullandığı depo) boş ConversationID'yi buraya çözer.
//
// Kademeler (üstteki kazanır):
//	1. body/header — istemcinin açık conversation_id / session_id'i
//	2. fingerprint — ilk kullanıcı mesajının sha256[:12]
//	3. bridge      — fp değiştiyse (compaction/checkpoint) ≥3 mesajlık
//	                 bitişik örtüşme → AYNI oturum (bölünme yok)
//	4. fp indeksi  — panode ÖNCEDEN var olan bölünmüş konuşmalar benimsenir
//	                 (aynı fp → ilk konuşma; yeni kart AÇILMAZ)
//	5. lcp         — anahtar yoksa eski içerik-prefix davranışı korunur
//
// Oturum durumu diskte tutulur (sessions-state.json): yeniden başlatma sonrası
// kimlikler değişmez ("son istekler sürekli değişiyor" kapanır).

// Oturum tespiti kademe adları — RequestLog.session_tier alanına yazılır.
const (
	tierBody        = "body"        // gövde/metadata explicit id
	tierHeader      = "header"      // X-Conversation-Id / X-Session-Id
	tierFingerprint = "fingerprint" // ilk kullanıcı mesajı parmak izi
	tierBridge      = "bridge"      // parmak izi değişimi → örtüşme köprüsü
	tierAdopt       = "adopt"       // panoda var olan konuşmayı benimseme
	tierLCP         = "lcp"         // anahtar yok: içerik-prefix'e düş
	tierNone        = "none"        // mesaj içermeyen gövde (embedding vb.)
)

const (
	// bridgeMinMessages: compaction köprüsü için gereken bitişik örtüşme.
	bridgeMinMessages = 3
	// sessionMaxMsgs: oturum başına saklanan son mesaj (digest) sayısı.
	sessionMaxMsgs = 60
	// sessionMaxSessions: kayıtlı oturum üst sınırı; aşılırsa en eski düşer.
	sessionMaxSessions = 500
	// sessionMaxHeaderChars: yakalanan başlık değeri kırpma sınırı.
	sessionMaxHeaderChars = 300
	// sessionStateFileName: kalıcı oturum durumu (yeniden başlatmada kimlik korunur).
	sessionStateFileName = "sessions-state.json"
	// persistedDigests: dosyaya yazılan son digest sayısı (köprü yeterlisi).
	persistedDigests = 12
)

// SessionInfo, bir isteğin çözümlenmiş oturum kimliğidir.
type SessionInfo struct {
	ID      string `json:"id,omitempty"`
	Tier    string `json:"tier"`
	FP      string `json:"fp,omitempty"`      // içerik parmak izi
	Preview string `json:"preview,omitempty"` // ilk kullanıcı mesajı ön izlemesi
}

// sessionDigest, bir mesajın (rol + içerik) parmak izidir. Köprü araması ve
// disk durumu bu digestler üzerinde çalışır: bellek küçük, dosya küçük,
// çakışma imkânsı derecede nadir (role + sha256[:16]).
type sessionDigest struct {
	Role string `json:"r"`
	Hash string `json:"h"`
}

// registrySession bellekteki tek bir oturumdur.
type registrySession struct {
	id       string
	fps      map[string]bool // bu oturumda görülen tüm parmak izileri
	msgs     []sessionDigest // son N mesaj (köprü hedefi)
	lastPID  int
	lastSeen time.Time
}

// SessionRegistry, oturum eşleme durumudur (RAM + isteğe bağlı disk).
type SessionRegistry struct {
	mu          sync.Mutex
	byAlias     map[string]string // explicit id / header / fp -> session id
	byID        map[string]*registrySession
	parents     map[string]string // alt oturum (subagent) → doğurucu oturum
	maxSessions int
	statePath   string
	loadOnce    sync.Once
	saveMu      sync.Mutex
	saveTimer   *time.Timer
	dirty       bool
}

// chatSessions her iki yolun (zen + evren) paylaştığı ortak oturum deposudur.
// Kalıcılık main() içinde setStatePath ile açılır (testler dosyaya yazmaz);
// açılışta sessions-state.json yüklenir, değişimler 1sn geciktirilerek yazılır.
var chatSessions = newSessionRegistry(sessionMaxSessions)

// setStatePath kalıcı durum dosyasını etkinleştirir (sunucu başlangıcında,
// ilk istekten ÖNCE çağrılır).
func (reg *SessionRegistry) setStatePath(path string) {
	reg.mu.Lock()
	reg.statePath = path
	reg.mu.Unlock()
}

func newSessionRegistry(maxSessions int) *SessionRegistry {
	return &SessionRegistry{
		byAlias:     make(map[string]string),
		byID:        make(map[string]*registrySession),
		parents:     make(map[string]string),
		maxSessions: maxSessions,
	}
}

// digestOf, TranscriptMessage'ı (rol+sha256) biçimine çevirir.
func digestOf(m TranscriptMessage) sessionDigest {
	sum := sha256.Sum256(m.Content)
	return sessionDigest{Role: m.Role, Hash: hex.EncodeToString(sum[:])[:16]}
}

func digestsOf(msgs []TranscriptMessage) []sessionDigest {
	out := make([]sessionDigest, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, digestOf(m))
	}
	return out
}

// sessionFingerprintMsgs, stats.go'daki sessionFingerprint ile BİREBİR aynı
// algoritmayı transkript mesajlarına uygular: ilk kullanıcı mesajının
// (düzleştirilmiş) sha256'sının ilk 12 hanesi.
func sessionFingerprintMsgs(msgs []TranscriptMessage) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		text := contentText(m.Content)
		sum := sha256.Sum256([]byte(text))
		return hex.EncodeToString(sum[:])[:12]
	}
	return ""
}

// longestCommonContiguous iki digest serisindeki EN UZUN bitişik eşleşmedir.
// Compaction sonrası istek [özet, ...son mesajlar] biçiminde gelir; kayıtlı
// oturumun kuyruğuyla bu mesajlar örtüşür → köprü kurulur.
func longestCommonContiguous(a, b []sessionDigest) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	best := 0
	for i := range a {
		if len(a)-i <= best {
			break
		}
		for j := range b {
			k := 0
			for i+k < len(a) && j+k < len(b) && a[i+k] == b[j+k] {
				k++
			}
			if k > best {
				best = k
			}
		}
	}
	return best
}

// touch, oturumu güncelleyer ve takma adları bağlar.
func (reg *SessionRegistry) touch(s *registrySession, digests []sessionDigest, pid int, fp string) {
	s.lastSeen = time.Now()
	if pid > 0 {
		s.lastPID = pid
	}
	if fp != "" {
		s.fps[fp] = true
		reg.byAlias[fp] = s.id
	}
	if len(digests) > 0 {
		if len(digests) > sessionMaxMsgs {
			digests = digests[len(digests)-sessionMaxMsgs:]
		}
		s.msgs = digests
	}
	reg.scheduleSaveLocked()
}

// create yeni bir oturum açar; id çakışırsa sonuna sayaç ekler.
func (reg *SessionRegistry) create(id string, digests []sessionDigest, pid int, fp string) *registrySession {
	base := id
	for n := 2; ; n++ {
		if _, exists := reg.byID[id]; !exists {
			break
		}
		id = base + "-" + itoa(n)
	}
	s := &registrySession{
		id:       id,
		fps:      make(map[string]bool),
		lastSeen: time.Now(),
	}
	reg.byID[id] = s
	reg.touch(s, digests, pid, fp)
	reg.evictLocked()
	return s
}

// itoa, küçük sayaçlar için basit taban-10 çevrimi.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// bridge, verilen seriyi en güçlü bitişik örtüşmeye sahip kayıtlı oturuma
// bağlar (son konuşan önce); ≥bridgeMinMessages şartı aranır.
func (reg *SessionRegistry) bridge(digests []sessionDigest, pid int) *registrySession {
	type cand struct {
		s   *registrySession
		lcs int
	}
	var cands []cand
	for _, s := range reg.byID {
		lcs := longestCommonContiguous(digests, s.msgs)
		if lcs >= bridgeMinMessages {
			cands = append(cands, cand{s, lcs})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].lcs != cands[j].lcs {
			return cands[i].lcs > cands[j].lcs
		}
		if (cands[i].s.lastPID > 0 && cands[i].s.lastPID == pid) != (cands[j].s.lastPID > 0 && cands[j].s.lastPID == pid) {
			return cands[i].s.lastPID == pid
		}
		return cands[i].s.lastSeen.After(cands[j].s.lastSeen)
	})
	return cands[0].s
}

// evictLocked oturum sayacı aşılsa en eskisini düşürür.
func (reg *SessionRegistry) evictLocked() {
	for len(reg.byID) > reg.maxSessions {
		var oldest *registrySession
		for _, s := range reg.byID {
			if oldest == nil || s.lastSeen.Before(oldest.lastSeen) {
				oldest = s
			}
		}
		if oldest == nil {
			return
		}
		delete(reg.byID, oldest.id)
		delete(reg.parents, oldest.id) // düşen çocuğun soy bağı da düşer
		for a, id := range reg.byAlias {
			if id == oldest.id {
				delete(reg.byAlias, a)
			}
		}
	}
}

// Ensure, açık kimlikli bir oturumu deftere kaydeder — Alias'ın "ilk bağlanan
// kazanır" fp gardına TAKILMAZAN. Zen yolu header kimliklerini de kalıcı
// kılar: RAM yıkımından sonra ve "dsh-" öneki geçişinde kimlik tanınsın diye.
// Zaten kayıtlıysa no-op.
func (reg *SessionRegistry) Ensure(sid string) {
	if sid == "" {
		return
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.ensureLoadedLocked()
	if _, ok := reg.byID[sid]; ok {
		return
	}
	reg.byID[sid] = &registrySession{
		id:       sid,
		fps:      map[string]bool{},
		lastSeen: time.Now(),
	}
	reg.scheduleSaveLocked()
}

// Alias, fp → sid bağını harici bir konuşmadan (transcript deposu) kaydeder.
// Registry'de gövdesi olmayan bir kimlik (panoda önceden bölünmüş konuşma)
// için oturum gövdesi de açılır ki sonraki istekler fp'den doğrudan o
// kimliğe düşsün.
func (reg *SessionRegistry) Alias(fp, sid string) {
	if fp == "" || sid == "" {
		return
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.ensureLoadedLocked()
	// İlk bağlama kazanır: fp indeksi (store) ile hizasız ikinci bir bağ,
	// kartların tekrar ayrılmasına yol açmasın.
	if existing, ok := reg.byAlias[fp]; ok && existing != sid {
		return
	}
	if s, ok := reg.byID[sid]; ok {
		s.fps[fp] = true
		reg.byAlias[fp] = sid
		reg.scheduleSaveLocked()
		return
	}
	s := &registrySession{
		id:       sid,
		fps:      map[string]bool{fp: true},
		lastSeen: time.Now(),
	}
	reg.byID[sid] = s
	reg.byAlias[fp] = sid
	reg.scheduleSaveLocked()
}

// SetParent, alt oturumu (subagent) doğuran oturuma bağlar. Alt oturumun
// TRANSCRIPT KARTI AYNI KALIR — soy bağı yalnızca kimlik/atama katmanını
// ilgilendirir (anahtar ataması kök oturuma görelenir). Döngü koruması:
// parent'ın zinciri child'a çıkıyorsa ekleme yok sayılır.
func (reg *SessionRegistry) SetParent(child, parent string) {
	if child == "" || parent == "" || child == parent {
		return
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.ensureLoadedLocked()
	walk := parent
	for i := 0; i < 64 && walk != ""; i++ {
		if walk == child {
			return // döngü: parent zaten child'ın altında
		}
		next, ok := reg.parents[walk]
		if !ok {
			break
		}
		walk = next
	}
	reg.parents[child] = parent
	reg.scheduleSaveLocked()
}

// Root, oturumun kökünü döndürür: subagent ise doğuran oturumu, kök oturum
// ise kendini. Zincir 64 adımda kırılır (bozuk/çok uzun veriye karşı).
// sessions-state.json'da kalıcıdır.
func (reg *SessionRegistry) Root(id string) string {
	if id == "" {
		return ""
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.ensureLoadedLocked()
	seen := map[string]bool{id: true}
	for i := 0; i < 64; i++ {
		next, ok := reg.parents[id]
		if !ok || next == "" || seen[next] {
			return id
		}
		seen[next] = true
		id = next
	}
	return id
}

// Knows, kimliğin kayıt defterinde bilinen bir oturum olup olmadığını sorar
// (CanonicalSessionID geri-uyum kontrolü: eski, öneksiz kimlik yaşatılır).
func (reg *SessionRegistry) Knows(id string) bool {
	if id == "" {
		return false
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.ensureLoadedLocked()
	if _, ok := reg.byID[id]; ok {
		return true
	}
	for _, sid := range reg.byAlias {
		if sid == id {
			return true
		}
	}
	return false
}

// Lookup, oturumu OLUŞTURMADAN arar: fp takma adı → bridge → yok.
// transcript deposunun "önce var olanı benimse" akışı için okunur.
func (reg *SessionRegistry) Lookup(fp string, msgs []TranscriptMessage) (string, string) {
	if fp == "" {
		return "", ""
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.ensureLoadedLocked()
	if sid, ok := reg.byAlias[fp]; ok {
		if s, ok2 := reg.byID[sid]; ok2 {
			s.lastSeen = time.Now()
			return s.id, tierFingerprint
		}
	}
	if s := reg.bridge(digestsOf(msgs), 0); s != nil {
		reg.byAlias[fp] = s.id
		s.fps[fp] = true
		s.lastSeen = time.Now()
		reg.scheduleSaveLocked()
		return s.id, tierBridge
	}
	return "", ""
}

// Resolve, bir isteğin oturumunu kademeli olarak çözer; güvenilir anahtar
// varsa ID döner (yoksa "" + tierLCP → eski LCP akışına düşülür).
func (reg *SessionRegistry) Resolve(explicitID, headerID, fp string, msgs []TranscriptMessage, pid int) SessionInfo {
	info := SessionInfo{Tier: tierLCP, FP: fp}
	if fp == "" || fp == "bilinmeyen" {
		info.FP = ""
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.ensureLoadedLocked()

	digests := digestsOf(msgs)

	find := func(id string) *registrySession {
		if s, ok := reg.byID[id]; ok {
			return s
		}
		if aliased, ok := reg.byAlias[id]; ok {
			if s, ok2 := reg.byID[aliased]; ok2 {
				return s
			}
		}
		return nil
	}

	// Kademe 1: gövde explicit id
	if explicitID != "" {
		s := find(explicitID)
		if s == nil {
			s = reg.create(explicitID, digests, pid, fp)
		} else {
			reg.touch(s, digests, pid, fp)
		}
		reg.byAlias[explicitID] = s.id
		info.ID, info.Tier = s.id, tierBody
		return info
	}

	// Kademe 2: header ipucu
	if headerID != "" {
		s := find(headerID)
		if s == nil {
			s = reg.create(headerID, digests, pid, fp)
		} else {
			reg.touch(s, digests, pid, fp)
		}
		reg.byAlias[headerID] = s.id
		info.ID, info.Tier = s.id, tierHeader
		return info
	}

	// Kademe 3: bilinen parmak izi
	if info.FP != "" {
		if sid, ok := reg.byAlias[info.FP]; ok {
			if s := reg.byID[sid]; s != nil {
				reg.touch(s, digests, pid, info.FP)
				info.ID, info.Tier = s.id, tierFingerprint
				return info
			}
		}
	}

	// Kademe 4: köprü (fp değişmiş olabilir — compaction/checkpoint)
	if s := reg.bridge(digests, pid); s != nil {
		reg.touch(s, digests, pid, info.FP)
		info.ID, info.Tier = s.id, tierBridge
		return info
	}

	// Kademe 5: fp varsa yeni oturum
	if info.FP != "" {
		s := reg.create("evs-"+info.FP, digests, pid, info.FP)
		info.ID, info.Tier = s.id, tierFingerprint
		return info
	}

	// Kademe 6: anahtar yok → transkript eski prefix eşleşmesine düşer
	return info
}

// ---- KALICILIK: sessions-state.json ---------------------------------------

type sessionStateFile struct {
	SavedAt  string                      `json:"saved_at"`
	Aliases  map[string]string           `json:"aliases"`
	Parents  map[string]string           `json:"parents,omitempty"` // subagent soyu: child → doğurucu
	Sessions []sessionStateEntry         `json:"sessions"`
}

type sessionStateEntry struct {
	ID       string          `json:"id"`
	FPS      []string        `json:"fps,omitempty"`
	Msgs     []sessionDigest `json:"msgs,omitempty"`
	LastPID  int             `json:"last_pid,omitempty"`
	LastSeen time.Time       `json:"last_seen"`
}

func (reg *SessionRegistry) stateEnabled() bool { return reg.statePath != "" }

func (reg *SessionRegistry) ensureLoadedLocked() {
	if !reg.stateEnabled() {
		return
	}
	reg.loadOnce.Do(func() {
		data, err := os.ReadFile(reg.statePath)
		if err != nil {
			return
		}
		var st sessionStateFile
		if json.Unmarshal(data, &st) != nil {
			return
		}
		for fp, sid := range st.Aliases {
			reg.byAlias[fp] = sid
		}
		for child, parent := range st.Parents {
			reg.parents[child] = parent
		}
		for _, e := range st.Sessions {
			if e.ID == "" {
				continue
			}
			s := &registrySession{
				id:       e.ID,
				fps:      make(map[string]bool, len(e.FPS)),
				msgs:     e.Msgs,
				lastPID:  e.LastPID,
				lastSeen: e.LastSeen,
			}
			for _, fp := range e.FPS {
				s.fps[fp] = true
				reg.byAlias[fp] = e.ID
			}
			if _, exists := reg.byID[e.ID]; !exists {
				reg.byID[e.ID] = s
			}
		}
	})
}

// scheduleSaveLocked, yazımı 1sn geciktirir (yığışık değişim tek dosyaya).
func (reg *SessionRegistry) scheduleSaveLocked() {
	if !reg.stateEnabled() {
		return
	}
	reg.dirty = true
	if reg.saveTimer != nil {
		reg.saveTimer.Stop()
	}
	reg.saveTimer = time.AfterFunc(time.Second, func() { reg.saveState() })
}

// saveState anlık durumu diske yazar (atomik: tmp + rename).
func (reg *SessionRegistry) saveState() {
	if !reg.stateEnabled() {
		return
	}
	reg.mu.Lock()
	if !reg.dirty {
		reg.mu.Unlock()
		return
	}
	reg.dirty = false
	st := sessionStateFile{
		SavedAt:  tsNow(),
		Aliases:  make(map[string]string, len(reg.byAlias)),
		Sessions: make([]sessionStateEntry, 0, len(reg.byID)),
	}
	for fp, sid := range reg.byAlias {
		st.Aliases[fp] = sid
	}
	if len(reg.parents) > 0 {
		st.Parents = make(map[string]string, len(reg.parents))
		for child, parent := range reg.parents {
			st.Parents[child] = parent
		}
	}
	for _, s := range reg.byID {
		fps := make([]string, 0, len(s.fps))
		for fp := range s.fps {
			fps = append(fps, fp)
		}
		msgs := s.msgs
		if len(msgs) > persistedDigests {
			msgs = msgs[len(msgs)-persistedDigests:]
		}
		st.Sessions = append(st.Sessions, sessionStateEntry{
			ID: s.id, FPS: fps, Msgs: msgs, LastPID: s.lastPID, LastSeen: s.lastSeen,
		})
	}
	reg.mu.Unlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	reg.saveMu.Lock()
	defer reg.saveMu.Unlock()
	tmp := reg.statePath + ".tmp"
	if os.WriteFile(tmp, data, 0644) != nil {
		return
	}
	_ = os.Rename(tmp, reg.statePath)
}

// ---- İSTEMCİ BAŞLIKLARI -----------------------------------------------------

// captureSessionHeaders, istemciden gelen ve oturum kimliği taşıyabilecek
// başlıkları toplar. Kimlik/parola başlıkları değeri alınmaz; standart
// içerik başlıkları atlanır (kullanıcı ajanı + özel başlıklar kalır).
func captureSessionHeaders(h map[string][]string) map[string]string {
	skip := map[string]bool{
		"content-type": true, "content-length": true, "accept": true,
		"accept-encoding": true, "connection": true, "host": true,
		"upgrade": true, "transfer-encoding": true, "te": true,
		"proxy-authorization": true, "x-api-key": true, "authorization": true,
		"cookie": true, "api-key": true,
	}
	out := make(map[string]string)
	for k, vals := range h {
		lk := toLowerASCII(k)
		if skip[lk] {
			continue
		}
		v := ""
		for i, one := range vals {
			if i > 0 {
				v += ", "
			}
			v += one
		}
		if len(v) > sessionMaxHeaderChars {
			v = v[:sessionMaxHeaderChars] + "…"
		}
		out[lk] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toLowerASCII, yalnızca ASCII küçük harfe indirger (başlık adları için).
func toLowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
