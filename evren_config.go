package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

type EvrenConfig struct {
	ListenAddr            string `json:"listen_addr"`
	UpstreamBase          string `json:"upstream_base"`
	DefaultDailyLimit     int64  `json:"default_daily_limit"`
	CalculationMode       string `json:"calculation_mode"` // "net" (Prompt-Cached+Completion) or "gross" (Prompt+Completion)
	KeysFile              string `json:"keys_file"`
	LogFile               string `json:"log_file"`
	AutoInjectStreamUsage bool   `json:"auto_inject_stream_usage"`
	RetryOnQuotaError     bool   `json:"retry_on_quota_error"`
	SwitchThresholdTokens int64  `json:"switch_threshold_tokens"` // E.g. switch when remaining <= this value
	// MaxImagesPerRequest, model bazlı sınır tanımlı OLMAYAN modeller için
	// genel yedek görsel sınırıdır. 0 = genel sınır kapalı; bu sayede
	// yalnızca ModelImageLimits içindeki modeller kırpılır.
	MaxImagesPerRequest int `json:"max_images_per_request"`
	// ModelImageLimits, istek başına izin verilen görsel sayısını MODEL BAZLI
	// tanımlar: anahtar model id, değer 0'dan büyük bir üst sınır. Değer 0
	// o model için kırpmanın açıkça kapalı olduğunu bildirir. Router'dan geçen
	// her model için ayrı ayrı ayarlanabilir; upstream'in "At most 8 image(s)"
	// gibi model-özel sınırları buraya yazılır.
	ModelImageLimits map[string]int `json:"model_image_limits"`
}

// imageLimitForModel, bir model için devredeki görsel sınırını döndürür.
// Önce model bazlı tablo, sonra genel yedek; ikisi de yoksa 0 (sınırsız).
// @param model - istek gövdesindeki model id; boş olabilir.
// @returns 0 → kırpma yok; 0'dan büyük → istek başına izin verilen görsel.
func (cfg EvrenConfig) imageLimitForModel(model string) int {
	if model != "" && cfg.ModelImageLimits != nil {
		if limit, ok := cfg.ModelImageLimits[model]; ok {
			if limit < 0 {
				return 0
			}
			return limit
		}
	}
	if cfg.MaxImagesPerRequest > 0 {
		return cfg.MaxImagesPerRequest
	}
	return 0
}

var (
	defaultEvrenConfig = EvrenConfig{
		ListenAddr:            "127.0.0.1:8088",
		UpstreamBase:          "https://evren-llmapi.ssyz.org.tr/v1",
		DefaultDailyLimit:     10000000, // 10 Milyon
		CalculationMode:       "net",      // Evren'in uyguladığı kesin formül: non-cached + 3% cached + completion
		KeysFile:              "keys.json",
		LogFile:               "usage_history.jsonl",
		AutoInjectStreamUsage: true,
		RetryOnQuotaError:     true,
		SwitchThresholdTokens: 50000, // Kalan kota 50.000 altına indiğinde veya 10M dolduğunda diğerine geçer
		// Genel sınır kapalı: kırpma yalnızca ModelImageLimits'deki modellerde.
		MaxImagesPerRequest: 0,
		ModelImageLimits: map[string]int{
			// Free havuz tek istekte en fazla 8 görsel kabul eder.
			"mimo-v2.6-pro": defaultMaxImagesPerRequest,
		},
	}
	evrenCfgLock sync.RWMutex
	globalEvrenCfg = defaultEvrenConfig
)

func LoadEvrenConfig(filename string) (*EvrenConfig, error) {
	evrenCfgLock.Lock()
	defer evrenCfgLock.Unlock()

	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			globalEvrenCfg = defaultEvrenConfig
			_ = SaveEvrenConfig(filename, &globalEvrenCfg)
			return &globalEvrenCfg, nil
		}
		return nil, err
	}

	// Görsel sınırı tablosu: dosyada ANAHTAR VARSA (boş dahi olsa) dosyadaki
	// hali birebir geçerli — kullanıcı bir modeli listeden çıkarabilmelidir.
	// Anahtar YOKSA varsayılan model seti korunur. Unmarshal mevcut haritaya
	// yalnızca eklediği için varsayılan harita asla kirlenmez.
	var probe struct {
		ModelImageLimits *map[string]int `json:"model_image_limits"`
	}
	_ = json.Unmarshal(data, &probe)

	cfg := defaultEvrenConfig
	if probe.ModelImageLimits != nil {
		cfg.ModelImageLimits = map[string]int{}
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if probe.ModelImageLimits != nil {
		cfg.ModelImageLimits = *probe.ModelImageLimits
	}
	if cfg.ModelImageLimits == nil {
		cfg.ModelImageLimits = map[string]int{}
	}

	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultEvrenConfig.ListenAddr
	}
	if cfg.UpstreamBase == "" {
		cfg.UpstreamBase = defaultEvrenConfig.UpstreamBase
	}
	if cfg.DefaultDailyLimit <= 0 {
		cfg.DefaultDailyLimit = defaultEvrenConfig.DefaultDailyLimit
	}
	if cfg.CalculationMode == "" {
		cfg.CalculationMode = defaultEvrenConfig.CalculationMode
	}
	if cfg.KeysFile == "" {
		cfg.KeysFile = defaultEvrenConfig.KeysFile
	}
	if cfg.LogFile == "" {
		cfg.LogFile = defaultEvrenConfig.LogFile
	}
	if cfg.MaxImagesPerRequest < 0 {
		cfg.MaxImagesPerRequest = 0
	}

	globalEvrenCfg = cfg
	return &globalEvrenCfg, nil
}

// evrenConfigFile, yapılandırmanın yazıldığı dosya (main.go ile aynı ad).
const evrenConfigFile = "config.json"

// maxImageLimitCeiling, tek model için izin verilen en büyük görsel sınırı.
// (Yok sayılan değerler yapılandırmayı bozmasın diye bir üst sınır tutulur.)
const maxImageLimitCeiling = 64

// UpdateImageLimits, model bazlı görsel sınırı tablosunu doğrular, config.json'a
// yazar ve bellekteki yapılandırmayı anında günceller.
// @param filename - kalıcı yapılandırma dosyası.
// @param limits - model id → 0 ve üzeri görsel sınırı (0 = o modelde kırpma kapalı).
// @param globalLimit - tabloda olmayan modeller için genel yedek sınır (0 = kapalı).
// @returns doğrulama veya yazma hatası; nil ise yeni ayarlar aktif.
func UpdateImageLimits(filename string, limits map[string]int, globalLimit int) error {
	if globalLimit < 0 || globalLimit > maxImageLimitCeiling {
		return fmt.Errorf("genel görsel sınırı 0..%d aralığında olmalı (alındı %d)", maxImageLimitCeiling, globalLimit)
	}
	clean := make(map[string]int, len(limits))
	for model, limit := range limits {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if limit < 0 || limit > maxImageLimitCeiling {
			return fmt.Errorf("%q için görsel sınırı 0..%d aralığında olmalı (alındı %d)", model, maxImageLimitCeiling, limit)
		}
		clean[model] = limit
	}

	evrenCfgLock.Lock()
	defer evrenCfgLock.Unlock()

	next := globalEvrenCfg
	next.ModelImageLimits = clean
	next.MaxImagesPerRequest = globalLimit
	if err := SaveEvrenConfig(filename, &next); err != nil {
		return err
	}
	globalEvrenCfg = next
	return nil
}

func SaveEvrenConfig(filename string, cfg *EvrenConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0644)
}

func GetConfig() EvrenConfig {
	evrenCfgLock.RLock()
	defer evrenCfgLock.RUnlock()
	return globalEvrenCfg
}
