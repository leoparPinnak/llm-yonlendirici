package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const shutdownGrace = 10 * time.Second

func main() {
	zenListenFlag := flag.String("zen-listen", "127.0.0.1:8110", "Zen Kostüm & Ana Web UI dinleme adresi")
	evrenListenFlag := flag.String("evren-listen", "127.0.0.1:8088", "EVREN Token Proxy (Dual HTTP/HTTPS) dinleme adresi")
	verboseFlag := flag.Bool("verbose", false, "Detaylı istek logları")
	flag.Parse()

	log.Println("====================================================================")
	log.Println("     🚀 LLM YÖNLENDİRİCİ — UNIFIED AI GATEWAY & PROXY ROUTER        ")
	log.Println("====================================================================")

	// 1. Zen Kostüm Yapılandırmasını Hazırla
	zenCfg := defaultConfig()
	if *zenListenFlag != "" {
		zenCfg.Listen = *zenListenFlag
	}

	// 2. EVREN Yapılandırmasını Hazırla
	evrenCfg, err := LoadEvrenConfig("config.json")
	if err != nil {
		log.Printf("[UYARI] config.json okunamadı, varsayılanlar devrede: %v", err)
		evrenCfg = &defaultEvrenConfig
	}
	if *evrenListenFlag != "" {
		evrenCfg.ListenAddr = *evrenListenFlag
	}

	// 3. Logger ve Veri Havuzlarını Başlat
	// Bellek halka tamponları: son 200 istek (RequestLog) + son 50 oturum /
	// 200 istek (transcript) — RAM patlaması imkânsız.
	InitLogger(evrenCfg.LogFile, maxRequestLogEntries)
	InitTranscripts(maxTranscriptConversations, maxTranscriptExchanges)
	// OTURUM KİMLİĞİ KALICILIĞI: yeniden başlatma sonrası fp/köprü eşlemeleri
	// ve oturum kimlikleri korunur (sessions-state.json).
	chatSessions.setStatePath(sessionStateFileName)

	evrenKM, err := NewKeyManager(evrenCfg.KeysFile)
	if err != nil {
		log.Fatalf("[FATAL] EVREN KeyManager başlatılamadı: %v", err)
	}

	proxyPool := NewProxyPool("proxies.json")
	defer func() { _ = proxyPool.Save() }()

	proxyServer := NewProxyServer(zenCfg.Listen, proxyPool, *verboseFlag)

	// 4. EVREN Proxy ve Sunucusu
	evrenProxy := NewProxyHandler(evrenKM)
	evrenServer := NewEvrenServer(evrenKM, evrenProxy)

	// 5. Zen Kostüm Gateway (Gömülü Proxy ve Evren Arayüzüyle Birlikte)
	zenGw := newGateway(zenCfg, *verboseFlag, proxyPool, proxyServer, evrenServer)
	if zenGw.usage != nil {
		defer zenGw.usage.Close()
	}

	// 6. Dinleyicileri Kur
	// Listener A: Port 8110 (Zen Kostüm + Master Web Dashboard + Proxy Yönlendirici)
	zenListener, err := net.Listen("tcp", zenCfg.Listen)
	if err != nil {
		log.Fatalf("[FATAL] Zen Kostüm portu (%s) dinlenemedi: %v", zenCfg.Listen, err)
	}

	zenHTTPServer := &http.Server{
		Handler:           zenGw.handler(),
		ReadHeaderTimeout: 20 * time.Second,
		ErrorLog:          log.New(os.Stderr, "", 0),
	}

	// Listener B: Port 8088 (EVREN Sabit HTTP Dinleyicisi)
	evrenListener, err := net.Listen("tcp", evrenCfg.ListenAddr)
	if err != nil {
		log.Fatalf("[FATAL] EVREN HTTP portu (%s) dinlenemedi: %v", evrenCfg.ListenAddr, err)
	}

	evrenHTTPServer := &http.Server{
		Handler:           evrenServer,
		ReadHeaderTimeout: 20 * time.Second,
		ErrorLog:          log.New(os.Stderr, "", 0),
	}

	// 7. Servisleri Başlat
	go func() {
		if err := zenHTTPServer.Serve(zenListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[FATAL] Zen Kostüm sunucu hatası: %v", err)
		}
	}()

	go func() {
		if err := evrenHTTPServer.Serve(evrenListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[FATAL] EVREN sunucu hatası: %v", err)
		}
	}()

	// 8. Bilgilendirme Bannerı
	printUnifiedBanner(zenCfg, evrenCfg, proxyPool, evrenKM)

	// 9. Graceful Shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	log.Println("\n[KAPANIŞ] Kapatma sinyali alındı, tüm bağlantılar ve veriler kaydediliyor...")

	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	_ = zenHTTPServer.Shutdown(drainCtx)
	_ = evrenHTTPServer.Shutdown(drainCtx)

	_ = evrenKM.Save()
	_ = proxyPool.Save()
	if zenGw.usage != nil {
		zenGw.usage.Close()
	}

	log.Println("[KAPANIŞ] Tüm sayaçlar, loglar ve proxy verileri başarıyla kaydedildi. Güle güle!")
}

func printUnifiedBanner(zenCfg config, evrenCfg *EvrenConfig, pool *ProxyPool, km *KeyManager) {
	rule := strings.Repeat("═", 78)
	fmt.Println()
	fmt.Println(rule)
	fmt.Println("  🚀 LLM YÖNLENDİRİCİ — TÜM SERVİSLER TEK BİR PAKETTE DEVREDE")
	fmt.Println(rule)

	fmt.Printf("  🎭 [1] ZEN KOSTÜM (OpenCode Zen Free Gateway):\n")
	fmt.Printf("         • Web Arayüzü (Dashboard) : http://%s/ui\n", zenCfg.Listen)
	fmt.Printf("         • OpenAI Uyumlu Chat API   : http://%s/v1/chat/completions\n", zenCfg.Listen)
	fmt.Printf("         • Model Kataloğu           : http://%s/v1/models\n", zenCfg.Listen)
	fmt.Printf("         • Upstream Hedef           : %s\n", zenCfg.Upstream)

	mode, _, list := pool.GetStatus()
	fmt.Printf("  🌐 [2] GÖMÜLÜ PROXY YÖNLENDİRİCİ (Havuz & Rotasyon):\n")
	fmt.Printf("         • Proxy Yönetim Paneli     : http://%s/ui/proxy\n", zenCfg.Listen)
	fmt.Printf("         • Kayıtlı Proxy Sayısı     : %d adet (Mod: %s)\n", len(list), mode)
	fmt.Printf("         • Zen Egress Entegrasyonu  : Zen upstream istekleri bu havuzdan çıkar\n")

	keys := km.GetAllKeys()
	activeKeyName := "Yok"
	for _, k := range keys {
		if k.IsActive {
			activeKeyName = k.Name
			break
		}
	}
	fmt.Printf("  ⚡ [3] EVREN TOKEN PROXY ROUTER & KOTA YÖNETİCİSİ:\n")
	fmt.Printf("         • Web Arayüzü (Ana Hub)    : http://%s/ui/evren\n", zenCfg.Listen)
	fmt.Printf("         • Web Arayüzü (Direkt 8088): http://%s/dashboard\n", evrenCfg.ListenAddr)
	fmt.Printf("         • HTTP API Base (Sabit)    : http://%s/v1\n", evrenCfg.ListenAddr)
	fmt.Printf("         • Port 8110 Alternatif API : http://%s/evren/v1\n", zenCfg.Listen)
	fmt.Printf("         • Aktif API Anahtarı       : %s (Toplam %d anahtar)\n", activeKeyName, len(keys))
	fmt.Printf("         • Günlük Limit & Hesaplama : 10M Token (Gece 03:00 TSİ Sıfırlaması, %%3 Ön Bellek)\n")
	fmt.Printf("         • Transkript Dökümü (JSON) : http://%s/api/evren/logs/transcript\n", zenCfg.Listen)
	fmt.Printf("         • Oto Geçiş Anahtarı       : http://%s/api/evren/auto-switch (GET/POST)\n", zenCfg.Listen)

	fmt.Println(rule)
	fmt.Println("  💡 Tek bir tarayıcı sekmesinde tüm servisleri yönetmek için:")
	fmt.Printf("     👉 http://%s/ui\n", zenCfg.Listen)
	fmt.Println(rule)
	fmt.Println()
}
