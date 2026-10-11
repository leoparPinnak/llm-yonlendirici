package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ActiveTunnel represents an in-flight live connection currently tunneling through a proxy.
type ActiveTunnel struct {
	ID         string    `json:"id"`
	StartTime  time.Time `json:"start_time"`
	ElapsedMs  int64     `json:"elapsed_ms"`
	Target     string    `json:"target"`
	ProxyHost  string    `json:"proxy_host"`
	ProxyType  string    `json:"proxy_type"`
	ClientPort int       `json:"client_port"`
	ClientPID  uint32    `json:"client_pid"`
	ClientProc string    `json:"client_proc"`

	clientConn   net.Conn `json:"-"`
	upstreamConn net.Conn `json:"-"`
	closed       bool     `json:"-"`
}

// TunnelRecord represents a completed tunnel exchange in recent history.
type TunnelRecord struct {
	Time       time.Time `json:"time"`
	Target     string    `json:"target"`
	ProxyHost  string    `json:"proxy_host"`
	ProxyType  string    `json:"proxy_type"`
	ClientPort int       `json:"client_port"`
	ClientPID  uint32    `json:"client_pid"`
	ClientProc string    `json:"client_proc"`
	DurationMs int64     `json:"duration_ms"`
	Status     string    `json:"status"`
	BytesUp    int64     `json:"bytes_up"`
	BytesDown  int64     `json:"bytes_down"`
}

// TunnelStats tracks aggregate counters for all connections since service start.
type TunnelStats struct {
	TotalTunnels   int64 `json:"total_tunnels"`
	ActiveTunnels  int   `json:"active_tunnels"`
	FailedTunnels  int64 `json:"failed_tunnels"`
	TotalBytesUp   int64 `json:"total_bytes_up"`
	TotalBytesDown int64 `json:"total_bytes_down"`
}

// ProxyServer manages proxy routing, CONNECT tunneling, and the proxy dashboard API.
type ProxyServer struct {
	listenAddr    string
	pool          *ProxyPool
	verbose       bool
	trackerMu     sync.RWMutex
	activeTunnels map[string]*ActiveTunnel
	recentTunnels []TunnelRecord
	stats         TunnelStats
}

// NewProxyServer creates a new proxy server bound to the pool.
func NewProxyServer(listenAddr string, pool *ProxyPool, verbose bool) *ProxyServer {
	return &ProxyServer{
		listenAddr:    listenAddr,
		pool:          pool,
		verbose:       verbose,
		activeTunnels: make(map[string]*ActiveTunnel),
		recentTunnels: make([]TunnelRecord, 0, 50),
	}
}

func (s *ProxyServer) startTunnel(target, proxyHost, proxyType string, clientPort int, clientPID uint32, clientProc string) string {
	now := time.Now()
	id := fmt.Sprintf("%d-%d", now.UnixNano(), clientPort)

	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()

	s.stats.TotalTunnels++
	s.activeTunnels[id] = &ActiveTunnel{
		ID:         id,
		StartTime:  now,
		Target:     target,
		ProxyHost:  proxyHost,
		ProxyType:  proxyType,
		ClientPort: clientPort,
		ClientPID:  clientPID,
		ClientProc: clientProc,
	}
	return id
}

func (s *ProxyServer) bindTunnelConns(id string, clientConn, upstreamConn net.Conn) {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()
	if at, ok := s.activeTunnels[id]; ok {
		at.clientConn = clientConn
		at.upstreamConn = upstreamConn
	}
}

// CloseActiveTunnels closes all currently active tunnel connections.
func (s *ProxyServer) CloseActiveTunnels() int {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()

	closed := 0
	for _, at := range s.activeTunnels {
		if !at.closed {
			at.closed = true
			if at.clientConn != nil {
				_ = at.clientConn.Close()
			}
			if at.upstreamConn != nil {
				_ = at.upstreamConn.Close()
			}
			closed++
		}
	}
	return closed
}

// CloseTunnel closes a single active tunnel by ID.
func (s *ProxyServer) CloseTunnel(id string) bool {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()

	if at, ok := s.activeTunnels[id]; ok {
		if !at.closed {
			at.closed = true
			if at.clientConn != nil {
				_ = at.clientConn.Close()
			}
			if at.upstreamConn != nil {
				_ = at.upstreamConn.Close()
			}
			return true
		}
	}
	return false
}

func (s *ProxyServer) finishTunnel(id, status string, start time.Time, bytesUp, bytesDown int64,
	target, proxyHost, proxyType string, clientPort int, clientPID uint32, clientProc string) {
	dur := time.Since(start).Milliseconds()

	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()

	delete(s.activeTunnels, id)
	if status != "OK" {
		s.stats.FailedTunnels++
	}
	s.stats.TotalBytesUp += bytesUp
	s.stats.TotalBytesDown += bytesDown

	rec := TunnelRecord{
		Time:       time.Now(),
		Target:     target,
		ProxyHost:  proxyHost,
		ProxyType:  proxyType,
		ClientPort: clientPort,
		ClientPID:  clientPID,
		ClientProc: clientProc,
		DurationMs: dur,
		Status:     status,
		BytesUp:    bytesUp,
		BytesDown:  bytesDown,
	}

	s.recentTunnels = append([]TunnelRecord{rec}, s.recentTunnels...)
	if len(s.recentTunnels) > 50 {
		s.recentTunnels = s.recentTunnels[:50]
	}
}

func (s *ProxyServer) getTunnelSnapshot() (TunnelStats, []ActiveTunnel, []TunnelRecord) {
	s.trackerMu.RLock()
	defer s.trackerMu.RUnlock()

	active := make([]ActiveTunnel, 0, len(s.activeTunnels))
	now := time.Now()
	for _, at := range s.activeTunnels {
		cp := *at
		cp.ElapsedMs = now.Sub(at.StartTime).Milliseconds()
		active = append(active, cp)
	}
	sort.Slice(active, func(i, j int) bool {
		return active[i].StartTime.Before(active[j].StartTime)
	})

	stats := s.stats
	stats.ActiveTunnels = len(active)

	recent := make([]TunnelRecord, len(s.recentTunnels))
	copy(recent, s.recentTunnels)

	return stats, active, recent
}

// handleConnect establishes an HTTPS tunnel via the chosen upstream proxy.
func (s *ProxyServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

	_, portStr, _ := net.SplitHostPort(r.RemoteAddr)
	clientPort, _ := strconv.Atoi(portStr)
	clientPID, clientProc := resolveClientInfo(clientPort)

	proxyItem, err := s.pool.SelectProxy()
	if err != nil {
		s.finishTunnel("", "Havuz Boş", time.Now(), 0, 0, target, "-", "-", clientPort, clientPID, clientProc)
		http.Error(w, "Proxy error: no upstream proxy available ("+err.Error()+")", http.StatusBadGateway)
		return
	}

	dialer, err := NewProxyDialer(proxyItem.RawURL, 10*time.Second)
	if err != nil {
		s.pool.RecordResult(proxyItem.ID, false)
		s.finishTunnel("", "Ayar Hatası", time.Now(), 0, 0, target, proxyItem.Host, proxyItem.Scheme, clientPort, clientPID, clientProc)
		http.Error(w, "Proxy configuration error: "+err.Error(), http.StatusBadGateway)
		return
	}

	start := time.Now()
	tunnelID := s.startTunnel(target, proxyItem.Host, proxyItem.Scheme, clientPort, clientPID, clientProc)

	upstreamConn, err := dialer.DialContext(r.Context(), "tcp", target)
	if err != nil {
		s.pool.RecordResult(proxyItem.ID, false)
		s.finishTunnel(tunnelID, err.Error(), start, 0, 0, target, proxyItem.Host, proxyItem.Scheme, clientPort, clientPID, clientProc)
		if s.verbose {
			log.Printf("[CONNECT FAIL] %s via %s: %v", target, proxyItem.Host, err)
		}
		http.Error(w, fmt.Sprintf("Tunnel error dialing %s via %s: %v", target, proxyItem.Host, err), http.StatusBadGateway)
		return
	}
	defer upstreamConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		s.finishTunnel(tunnelID, "Hijack Desteklenmiyor", start, 0, 0, target, proxyItem.Host, proxyItem.Scheme, clientPort, clientPID, clientProc)
		http.Error(w, "Webserver doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		s.finishTunnel(tunnelID, "Hijack Hatası: "+err.Error(), start, 0, 0, target, proxyItem.Host, proxyItem.Scheme, clientPort, clientPID, clientProc)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	s.bindTunnelConns(tunnelID, clientConn, upstreamConn)

	// Notify client that the tunnel is ready
	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	s.pool.RecordResult(proxyItem.ID, true)
	if s.verbose {
		procInfo := clientProc
		if clientPID != 0 {
			procInfo = fmt.Sprintf("%s [%d]", clientProc, clientPID)
		}
		log.Printf("[TUNNEL OPEN] %s via %s (%s) from %s:%d", target, proxyItem.Host, time.Since(start), procInfo, clientPort)
	}

	var bytesUp, bytesDown int64
	var wg sync.WaitGroup
	wg.Add(2)

	// Pipe client -> upstream
	go func() {
		defer wg.Done()
		var buf [32 * 1024]byte
		for {
			nr, er := clientConn.Read(buf[:])
			if nr > 0 {
				nw, ew := upstreamConn.Write(buf[:nr])
				if nw > 0 {
					atomic.AddInt64(&bytesUp, int64(nw))
				}
				if ew != nil {
					break
				}
			}
			if er != nil {
				break
			}
		}
		if tc, ok := upstreamConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	// Pipe upstream -> client
	go func() {
		defer wg.Done()
		var buf [32 * 1024]byte
		for {
			nr, er := upstreamConn.Read(buf[:])
			if nr > 0 {
				nw, ew := clientConn.Write(buf[:nr])
				if nw > 0 {
					atomic.AddInt64(&bytesDown, int64(nw))
				}
				if ew != nil {
					break
				}
			}
			if er != nil {
				break
			}
		}
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	wg.Wait()
	s.finishTunnel(tunnelID, "OK", start, atomic.LoadInt64(&bytesUp), atomic.LoadInt64(&bytesDown),
		target, proxyItem.Host, proxyItem.Scheme, clientPort, clientPID, clientProc)
}

// handleHTTPForward proxies plain HTTP requests through the chosen upstream proxy.
func (s *ProxyServer) handleHTTPForward(w http.ResponseWriter, r *http.Request) {
	proxyItem, err := s.pool.SelectProxy()
	if err != nil {
		http.Error(w, "Proxy error: "+err.Error(), http.StatusBadGateway)
		return
	}

	dialer, err := NewProxyDialer(proxyItem.RawURL, 10*time.Second)
	if err != nil {
		s.pool.RecordResult(proxyItem.ID, false)
		http.Error(w, "Proxy error: "+err.Error(), http.StatusBadGateway)
		return
	}

	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	defer transport.CloseIdleConnections()

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, "Request error: "+err.Error(), http.StatusBadRequest)
		return
	}

	for k, v := range r.Header {
		if !strings.EqualFold(k, "Proxy-Connection") && !strings.EqualFold(k, "Proxy-Authorization") {
			outReq.Header[k] = v
		}
	}

	start := time.Now()
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		s.pool.RecordResult(proxyItem.ID, false)
		http.Error(w, "Forward error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	s.pool.RecordResult(proxyItem.ID, true)
	if s.verbose {
		log.Printf("[FORWARD] %s %s via %s (%s)", r.Method, r.URL.String(), proxyItem.Host, time.Since(start))
	}

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleAPI routes REST API requests for managing the pool.
func (s *ProxyServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/proxy")
	path = strings.TrimPrefix(path, "/api")

	switch path {
	case "/status":
		s.apiGetStatus(w, r)
	case "/proxies":
		if r.Method == http.MethodPost {
			s.apiAddProxies(w, r)
		} else {
			s.apiGetStatus(w, r)
		}
	case "/proxies/delete":
		if r.Method == http.MethodPost || r.Method == http.MethodDelete {
			s.apiDeleteProxy(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	case "/proxies/clear":
		if r.Method == http.MethodPost {
			s.apiClearProxies(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	case "/mode":
		if r.Method == http.MethodPost {
			s.apiSetMode(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	case "/tunnels/close":
		if r.Method == http.MethodPost {
			s.apiCloseTunnels(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	case "/check":
		if r.Method == http.MethodPost {
			s.apiCheck(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *ProxyServer) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	mode, selectedID, list := s.pool.GetStatus()
	stats, activeTunnels, recentTunnels := s.getTunnelSnapshot()

	aliveCount := 0
	for _, p := range list {
		if p.Alive {
			aliveCount++
		}
	}

	resp := map[string]any{
		"listen_addr":    s.listenAddr,
		"mode":           mode,
		"selected_id":    selectedID,
		"proxies":        list,
		"total_proxies":  len(list),
		"alive_proxies":  aliveCount,
		"stats":          stats,
		"active_tunnels": activeTunnels,
		"recent_tunnels": recentTunnels,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *ProxyServer) apiAddProxies(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	toAdd := body.Text
	if toAdd == "" && body.URL != "" {
		toAdd = body.URL
	}
	if strings.TrimSpace(toAdd) == "" {
		http.Error(w, "No proxy URL provided", http.StatusBadRequest)
		return
	}

	added, errs := s.pool.AddBulk(toAdd)
	resp := map[string]any{
		"success": added > 0 || len(errs) == 0,
		"added":   added,
		"errors":  errs,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *ProxyServer) apiDeleteProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "Missing or invalid ID", http.StatusBadRequest)
		return
	}

	_, selID, _ := s.pool.GetStatus()
	deleted := s.pool.RemoveProxy(body.ID)
	if deleted && selID == body.ID {
		s.CloseActiveTunnels()
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": deleted})
}

func (s *ProxyServer) apiClearProxies(w http.ResponseWriter, r *http.Request) {
	s.pool.Clear()
	s.CloseActiveTunnels()
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *ProxyServer) apiSetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode         string `json:"mode"`
		SelectedID   string `json:"selected_id,omitempty"`
		ResetTunnels bool   `json:"reset_tunnels,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	modeChanged := false
	if body.Mode != "" {
		curMode, _, _ := s.pool.GetStatus()
		if curMode != body.Mode {
			if err := s.pool.SetMode(body.Mode); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			modeChanged = true
		}
	}

	selectedChanged := false
	if body.SelectedID != "" {
		_, curSelected, _ := s.pool.GetStatus()
		if curSelected != body.SelectedID {
			if err := s.pool.SetSelected(body.SelectedID); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			selectedChanged = true
		} else {
			_ = s.pool.SetSelected(body.SelectedID)
		}
	}

	closedCount := 0
	if selectedChanged || modeChanged || body.ResetTunnels || body.SelectedID != "" {
		closedCount = s.CloseActiveTunnels()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"tunnels_closed": closedCount,
	})
}

func (s *ProxyServer) apiCloseTunnels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	closedCount := 0
	if body.ID != "" {
		if s.CloseTunnel(body.ID) {
			closedCount = 1
		}
	} else {
		closedCount = s.CloseActiveTunnels()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"tunnels_closed": closedCount,
	})
}

func (s *ProxyServer) apiCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if body.ID != "" {
			_, _ = s.pool.CheckProxy(ctx, body.ID)
		} else {
			s.pool.CheckAll(ctx)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{"message": "Check started in background"})
}
