package main

import (
	_ "embed"
	"net/http"
)

// asr_test.html, ses (ASR) uç noktasını sürükle-bırak ile denemek için
// hazırlanmış bağımsız test sayfasıdır. İkiliye gömülür ve iki adresten
// sunulur: http://127.0.0.1:8110/asr-test (zen gateway) ve
// http://127.0.0.1:8088/asr-test (evren server). Sayfa dosyadan (file://)
// açıldığında da çalışır: proxy yalnızca yerel kaynaklara CORS başlığı verir.
//
//go:embed asr_test.html
var embeddedASRTestHTML string

// handleASRTestPage, ses test sayfasını olduğu gibi döner.
// Statiktir: hiçbir durum okumaz/yazmaz, oturum atama mantığına dokunmaz.
// @param w - yanıt yazıcı.
// @param r - gelen istek (GET beklenir).
func handleASRTestPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(embeddedASRTestHTML))
}
