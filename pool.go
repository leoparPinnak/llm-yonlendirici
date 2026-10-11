package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ModeFailover   = "failover"    // Use first alive proxy; switch if error
	ModeRoundRobin = "round-robin" // Rotate per request across alive proxies
	ModeManual     = "manual"      // Strictly use selected proxy
)

// ProxyItem represents one managed upstream proxy.
type ProxyItem struct {
	ID         string    `json:"id"`
	RawURL     string    `json:"raw_url"`
	Scheme     string    `json:"scheme"`
	Host       string    `json:"host"`
	User       string    `json:"user,omitempty"`
	Alive      bool      `json:"alive"`
	LatencyMs  int64     `json:"latency_ms"`
	EgressIP   string    `json:"egress_ip"`
	LastCheck  time.Time `json:"last_check"`
	LastError  string    `json:"last_error,omitempty"`
	TotalReqs  int64     `json:"total_reqs"`
	FailedReqs int64     `json:"failed_reqs"`
}

// PoolConfig represents the persisted state of the pool.
type PoolConfig struct {
	Mode       string       `json:"mode"`
	SelectedID string       `json:"selected_id,omitempty"`
	Proxies    []*ProxyItem `json:"proxies"`
}

// ProxyPool manages the proxy collection, state, and routing.
type ProxyPool struct {
	mu          sync.RWMutex
	storagePath string
	mode        string
	selectedID  string
	proxies     []*ProxyItem
	rrIndex     uint64
	client      *http.Client
}

// NewProxyPool initializes a new proxy pool.
func NewProxyPool(storagePath string) *ProxyPool {
	pool := &ProxyPool{
		storagePath: storagePath,
		mode:        ModeFailover,
		proxies:     make([]*ProxyItem, 0),
		client:      &http.Client{Timeout: 8 * time.Second},
	}
	_ = pool.Load()
	return pool
}

func generateID(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if strings.EqualFold(rawURL, "direct") || strings.EqualFold(rawURL, "direct://") || strings.HasPrefix(strings.ToLower(rawURL), "direct://") {
		return "direct"
	}
	if strings.Contains(rawURL, "127.0.0.1:40000") {
		return "cloudflare"
	}
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])[:8]
}

// ParseProxyURL parses and normalizes a proxy string.
func ParseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty proxy URL")
	}
	if strings.EqualFold(raw, "direct") || strings.EqualFold(raw, "direct://") || strings.HasPrefix(strings.ToLower(raw), "direct://") {
		return &url.URL{
			Scheme: "direct",
			Host:   "direct",
		}, nil
	}
	// Default to socks5:// if scheme is missing
	if !strings.Contains(raw, "://") {
		raw = "socks5://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "socks5", "socks5h", "http", "https", "direct":
		parsed.Scheme = scheme
	default:
		return nil, fmt.Errorf("unsupported scheme %q (supported: socks5, socks5h, http, direct)", scheme)
	}
	if parsed.Scheme != "direct" && parsed.Host == "" {
		return nil, errors.New("missing host:port in proxy URL")
	}
	return parsed, nil
}

// AddProxy adds a single proxy to the pool.
func (p *ProxyPool) AddProxy(rawURL string) (*ProxyItem, error) {
	parsed, err := ParseProxyURL(rawURL)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	id := generateID(parsed.String())
	rawURLStr := parsed.String()
	hostStr := parsed.Host

	if parsed.Scheme == "direct" {
		id = "direct"
		rawURLStr = "direct://"
		hostStr = "Doğrudan Bağlantı (DIRECT)"
	}

	for _, item := range p.proxies {
		if item.ID == id || item.RawURL == rawURLStr {
			return item, nil // already exists
		}
	}

	user := ""
	if parsed.User != nil {
		user = parsed.User.Username()
	}

	item := &ProxyItem{
		ID:     id,
		RawURL: rawURLStr,
		Scheme: parsed.Scheme,
		Host:   hostStr,
		User:   user,
		Alive:  true, // assume alive until checked
	}
	p.proxies = append(p.proxies, item)
	if p.selectedID == "" {
		p.selectedID = id
	}
	_ = p.saveLocked()
	return item, nil
}

// AddBulk adds multiple proxies from multiline text.
func (p *ProxyPool) AddBulk(text string) (added int, errs []string) {
	lines := strings.Split(text, "\n")
	for lineNum, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, err := p.AddProxy(line)
		if err != nil {
			errs = append(errs, fmt.Sprintf("line %d (%s): %v", lineNum+1, line, err))
		} else {
			added++
		}
	}
	return added, errs
}

// RemoveProxy deletes a proxy by ID.
func (p *ProxyPool) RemoveProxy(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	found := false
	filtered := make([]*ProxyItem, 0, len(p.proxies))
	for _, item := range p.proxies {
		if item.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, item)
	}
	if found {
		p.proxies = filtered
		if p.selectedID == id {
			if len(p.proxies) > 0 {
				p.selectedID = p.proxies[0].ID
			} else {
				p.selectedID = ""
			}
		}
		_ = p.saveLocked()
	}
	return found
}

// Clear removes all proxies from the pool.
func (p *ProxyPool) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.proxies = make([]*ProxyItem, 0)
	p.selectedID = ""
	_ = p.saveLocked()
}

// GetStatus returns a snapshot of pool status and all proxies.
func (p *ProxyPool) GetStatus() (string, string, []*ProxyItem) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	copied := make([]*ProxyItem, len(p.proxies))
	for i, item := range p.proxies {
		cp := *item
		copied[i] = &cp
	}
	return p.mode, p.selectedID, copied
}

// SetMode updates the routing strategy.
func (p *ProxyPool) SetMode(mode string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch mode {
	case ModeFailover, ModeRoundRobin, ModeManual:
		p.mode = mode
		return p.saveLocked()
	default:
		return fmt.Errorf("invalid mode %q (must be failover, round-robin, or manual)", mode)
	}
}

// SetSelected picks a specific proxy for manual or failover routing.
func (p *ProxyPool) SetSelected(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, item := range p.proxies {
		if item.ID == id {
			p.selectedID = id
			// User explicitly chose this proxy; give it a fresh chance to connect
			item.Alive = true
			item.FailedReqs = 0
			return p.saveLocked()
		}
	}
	return fmt.Errorf("proxy with ID %q not found", id)
}

// GetSelectedID returns the currently selected proxy ID.
func (p *ProxyPool) GetSelectedID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.selectedID
}

// SelectProxy chooses a proxy based on the active mode.
func (p *ProxyPool) SelectProxy() (*ProxyItem, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.proxies) == 0 {
		return nil, errors.New("proxy pool is empty")
	}

	switch p.mode {
	case ModeManual:
		for _, item := range p.proxies {
			if item.ID == p.selectedID {
				return item, nil
			}
		}
		// Fallback to first if selected not found
		return p.proxies[0], nil

	case ModeRoundRobin:
		aliveList := make([]*ProxyItem, 0, len(p.proxies))
		for _, item := range p.proxies {
			if item.Alive {
				aliveList = append(aliveList, item)
			}
		}
		if len(aliveList) == 0 {
			// If all marked dead, try any proxy
			aliveList = p.proxies
		}
		idx := atomic.AddUint64(&p.rrIndex, 1) - 1
		return aliveList[idx%uint64(len(aliveList))], nil

	case ModeFailover:
		fallthrough
	default:
		// 1. If a selected proxy is set and alive, prioritize it!
		if p.selectedID != "" {
			for _, item := range p.proxies {
				if item.ID == p.selectedID && item.Alive {
					return item, nil
				}
			}
		}
		// 2. If selected is dead or not set, failover to the first alive proxy
		for _, item := range p.proxies {
			if item.Alive {
				return item, nil
			}
		}
		// 3. If none alive, fallback to selected proxy if available, else first proxy
		if p.selectedID != "" {
			for _, item := range p.proxies {
				if item.ID == p.selectedID {
					return item, nil
				}
			}
		}
		return p.proxies[0], nil
	}
}

// RecordResult tracks success/failure metrics for a proxy.
func (p *ProxyPool) RecordResult(id string, success bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, item := range p.proxies {
		if item.ID == id {
			item.TotalReqs++
			if !success {
				item.FailedReqs++
				// Mark as dead if failure rate is very high in failover mode
				if item.FailedReqs >= 3 && item.FailedReqs == item.TotalReqs {
					item.Alive = false
				}
			} else {
				item.Alive = true
			}
			return
		}
	}
}

// CheckProxy tests connectivity and discovers the egress IP for a single proxy.
func (p *ProxyPool) CheckProxy(ctx context.Context, id string) (*ProxyItem, error) {
	p.mu.RLock()
	var target *ProxyItem
	for _, item := range p.proxies {
		if item.ID == id {
			target = item
			break
		}
	}
	p.mu.RUnlock()

	if target == nil {
		return nil, fmt.Errorf("proxy ID %q not found", id)
	}

	dialer, err := NewProxyDialer(target.RawURL, 8*time.Second)
	if err != nil {
		p.updateCheckResult(id, false, 0, "", err.Error())
		return nil, err
	}

	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: 8 * time.Second,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   10 * time.Second,
	}

	start := time.Now()
	// Test against ipify or httpbin
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://api.ipify.org", nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		p.updateCheckResult(id, false, latency, "", err.Error())
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("status %s", resp.Status)
		p.updateCheckResult(id, false, latency, "", err.Error())
		return nil, err
	}

	ipBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	ipStr := strings.TrimSpace(string(ipBytes))

	p.updateCheckResult(id, true, latency, ipStr, "")
	return target, nil
}

func (p *ProxyPool) updateCheckResult(id string, alive bool, latency int64, ip, lastErr string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, item := range p.proxies {
		if item.ID == id {
			item.Alive = alive
			item.LatencyMs = latency
			item.LastCheck = time.Now()
			item.LastError = lastErr
			if ip != "" {
				item.EgressIP = ip
			}
			_ = p.saveLocked()
			return
		}
	}
}

// CheckAll checks all proxies concurrently.
func (p *ProxyPool) CheckAll(ctx context.Context) {
	p.mu.RLock()
	ids := make([]string, len(p.proxies))
	for i, item := range p.proxies {
		ids[i] = item.ID
	}
	p.mu.RUnlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 5) // max 5 concurrent checks
	for _, id := range ids {
		wg.Add(1)
		go func(pid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, _ = p.CheckProxy(ctx, pid)
		}(id)
	}
	wg.Wait()
}

// Save writes the current pool configuration to disk.
func (p *ProxyPool) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveLocked()
}

func (p *ProxyPool) saveLocked() error {
	if p.storagePath == "" {
		return nil
	}
	cfg := PoolConfig{
		Mode:       p.mode,
		SelectedID: p.selectedID,
		Proxies:    p.proxies,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.storagePath, data, 0o644)
}

// Load reads the pool configuration from disk.
func (p *ProxyPool) Load() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.storagePath == "" {
		return nil
	}
	data, err := os.ReadFile(p.storagePath)
	if err != nil {
		if os.IsNotExist(err) {
			directItem := &ProxyItem{
				ID:     "direct",
				RawURL: "direct://",
				Scheme: "direct",
				Host:   "Doğrudan Bağlantı (DIRECT)",
				Alive:  true,
			}
			p.proxies = []*ProxyItem{directItem}
			p.selectedID = "direct"
			return p.saveLocked()
		}
		return err
	}

	var cfg PoolConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if cfg.Mode != "" {
		p.mode = cfg.Mode
	}
	p.selectedID = cfg.SelectedID
	if cfg.Proxies != nil {
		unique := make([]*ProxyItem, 0, len(cfg.Proxies))
		seenID := make(map[string]bool)
		for _, item := range cfg.Proxies {
			if strings.TrimSpace(item.RawURL) == "" {
				continue
			}
			if item.ID == "" {
				item.ID = generateID(item.RawURL)
			}
			if seenID[item.ID] {
				continue
			}
			seenID[item.ID] = true
			unique = append(unique, item)
		}
		p.proxies = unique
	}

	// Verify selectedID is valid
	if len(p.proxies) > 0 {
		exists := false
		for _, item := range p.proxies {
			if item.ID == p.selectedID {
				exists = true
				break
			}
		}
		if !exists {
			p.selectedID = p.proxies[0].ID
		}
	} else {
		p.selectedID = ""
	}
	return nil
}
