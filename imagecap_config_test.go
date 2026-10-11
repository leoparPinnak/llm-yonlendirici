package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestImageLimitForModel, sınırın model bazlı nasıl çözüldüğünü doğrular.
func TestImageLimitForModel(t *testing.T) {
	cases := []struct {
		name   string
		cfg    EvrenConfig
		model  string
		expect int
	}{
		{
			name: "tablodaki model kendi sınırını alır",
			cfg: EvrenConfig{
				ModelImageLimits: map[string]int{"mimo-v2.6-pro": 8},
			},
			model:  "mimo-v2.6-pro",
			expect: 8,
		},
		{
			name: "tabloda olmayan model genel yedekle döner (0 = kapalı)",
			cfg: EvrenConfig{
				ModelImageLimits: map[string]int{"mimo-v2.6-pro": 8},
			},
			model:  "space-bunny-free",
			expect: 0,
		},
		{
			name: "genel yedek açıkken tabloda olmayan model onu alır",
			cfg: EvrenConfig{
				MaxImagesPerRequest: 4,
				ModelImageLimits:    map[string]int{"mimo-v2.6-pro": 8},
			},
			model:  "başka-model",
			expect: 4,
		},
		{
			name: "tabloda 0 olan model genel yedekten kaçar (açıkça kapalı)",
			cfg: EvrenConfig{
				MaxImagesPerRequest: 4,
				ModelImageLimits:    map[string]int{"mimo-v2.6-pro": 0},
			},
			model:  "mimo-v2.6-pro",
			expect: 0,
		},
		{
			name: "boş model adı genel yedeğe düşer",
			cfg: EvrenConfig{
				MaxImagesPerRequest: 4,
				ModelImageLimits:    map[string]int{"mimo-v2.6-pro": 8},
			},
			model:  "",
			expect: 4,
		},
		{
			name: "negatif sınır asla devreye girmez",
			cfg: EvrenConfig{
				ModelImageLimits: map[string]int{"mimo-v2.6-pro": -3},
			},
			model:  "mimo-v2.6-pro",
			expect: 0,
		},
		{
			name:   "yapılandırma tamamen boşsa sınır yok",
			cfg:    EvrenConfig{},
			model:  "mimo-v2.6-pro",
			expect: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.imageLimitForModel(tc.model)
			if got != tc.expect {
				t.Fatalf("imageLimitForModel(%q) = %d, beklenen %d", tc.model, got, tc.expect)
			}
		})
	}
}

// TestCapImagesOnlyConfiguredModels, proxy katmanında yalnızca sınırı tanımlı
// modelin kırpıldığını (model adı gövdeden okunarak) doğrular.
func TestCapImagesOnlyConfiguredModels(t *testing.T) {
	cfg := EvrenConfig{
		ModelImageLimits: map[string]int{"mimo-v2.6-pro": 8},
	}
	body := chatBodyWithImages(10)

	// Sınırı tanımlı model: 2 düşer.
	limited := append([]byte(nil), body...)
	model := chatModelName(limited)
	if model != "mimo-v2.6-pro" {
		t.Fatalf("model okunamadı: %q", model)
	}
	if limit := cfg.imageLimitForModel(model); limit != 8 {
		t.Fatalf("beklenen limit 8, %d geldi", limit)
	}
	capped, dropped, err := capImagesInChatBody(limited, cfg.imageLimitForModel(model))
	if err != nil {
		t.Fatalf("hata: %v", err)
	}
	if dropped != 2 {
		t.Fatalf("2 düşmeliydi, %d düştü", dropped)
	}
	if imageCount(t, capped) != 8 {
		t.Fatalf("8 görsel kalmalı, %d kaldı", imageCount(t, capped))
	}

	// Sınırı TANIMLI OLMAYAN model: dokunulmaz.
	other := []byte(`{"model":"space-bunny-free","messages":[` +
		`{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,QQQ"}},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,RRR"}}` +
		`]}]}`)
	limit := cfg.imageLimitForModel(chatModelName(other))
	if limit != 0 {
		t.Fatalf("tanımsız modelde sınır 0 olmalı, %d geldi", limit)
	}
	if limit > 0 {
		out, d, _ := capImagesInChatBody(other, limit)
		if d != 0 || string(out) != string(other) {
			t.Fatal("tanımsız model değişmemeli")
		}
	}
}

// TestUpdateImageLimitsValidatesAndPersists, ayar kaydının hem doğruladığını
// hem config.json'a yazdığını ve belleğe işlediğini kanıtlar.
func TestUpdateImageLimitsValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if err := UpdateImageLimits(path, map[string]int{"mimo-v2.6-pro": 8}, 0); err != nil {
		t.Fatalf("kayıt başarısız: %v", err)
	}
	if got := GetConfig().imageLimitForModel("mimo-v2.6-pro"); got != 8 {
		t.Fatalf("bellekte limit 8 olmalı, %d", got)
	}
	if got := GetConfig().imageLimitForModel("diğer"); got != 0 {
		t.Fatalf("tanımsız modelde genel yedek 0 olmalı, %d", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config.json okunamadı: %v", err)
	}
	if !bytesContain(data, []byte(`"mimo-v2.6-pro": 8`)) {
		t.Fatalf("config.json'a yazılmadı: %s", data)
	}

	// Geçersiz değerler reddedilmeli.
	if err := UpdateImageLimits(path, map[string]int{"x": -1}, 0); err == nil {
		t.Fatal("negatif sınır reddedilmeli")
	}
	if err := UpdateImageLimits(path, nil, 999); err == nil {
		t.Fatal("aşırı genel sınır reddedilmeli")
	}
	// Boş tablo geçerli: tüm modeller kırpmasız.
	if err := UpdateImageLimits(path, map[string]int{}, 0); err != nil {
		t.Fatalf("boş tablo geçerli olmalı: %v", err)
	}
	if len(GetConfig().ModelImageLimits) != 0 {
		t.Fatal("boş tablo bellekte de boş kalmalı")
	}
}

// TestLoadEvrenConfigKeepsExplicitEmptyTable, config.json'da tablo varsa
// (boş dahi olsa) dosyadaki hali korunur — varsayılan geri eklenmez.
func TestLoadEvrenConfigKeepsExplicitEmptyTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "listen_addr": "127.0.0.1:8088",
  "model_image_limits": {}
}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadEvrenConfig(path)
	if err != nil {
		t.Fatalf("yüklenemedi: %v", err)
	}
	if len(cfg.ModelImageLimits) != 0 {
		t.Fatalf("boş tablo korunmalı, %d model geldi", len(cfg.ModelImageLimits))
	}

	// Tablo hiç yoksa varsayılan model seti devreye girer.
	if err := os.WriteFile(path, []byte(`{"listen_addr":"127.0.0.1:8088"}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadEvrenConfig(path)
	if err != nil {
		t.Fatalf("yüklenemedi: %v", err)
	}
	if cfg.imageLimitForModel("mimo-v2.6-pro") != defaultMaxImagesPerRequest {
		t.Fatalf("varsayılan tablo uygulanmadı: %d", cfg.imageLimitForModel("mimo-v2.6-pro"))
	}
}

// bytesContain, ham baytlar içinde alt dizi arar.
func bytesContain(haystack, needle []byte) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
