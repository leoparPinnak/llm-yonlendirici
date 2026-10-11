package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelLimit specifies context and output token bounds.
type ModelLimit struct {
	Context int `json:"context,omitempty"`
	Input   int `json:"input,omitempty"`
	Output  int `json:"output,omitempty"`
}

// ModelInfo carries rich metadata for one model, matching standard OpenAI
// fields and enriched fields that DSH discovery.ts and client frontends read.
type ModelInfo struct {
	ID               string      `json:"id"`
	Object           string      `json:"object"`
	Created          int64       `json:"created"`
	OwnedBy          string      `json:"owned_by"`
	Name             string      `json:"name,omitempty"`
	DisplayName      string      `json:"display_name,omitempty"`
	ContextWindow    int         `json:"context_window,omitempty"`
	ContextWindowAlt int         `json:"contextWindow,omitempty"`
	MaxTokens        int         `json:"max_tokens,omitempty"`
	MaxTokensAlt     int         `json:"maxTokens,omitempty"`
	Reasoning        bool        `json:"reasoning,omitempty"`
	InputModalities  []string    `json:"input_modalities,omitempty"`
	Limit            *ModelLimit `json:"limit,omitempty"`
}

// modelSpec defines known capacities and capabilities for models.
type modelSpec struct {
	Name            string
	ContextWindow   int
	MaxTokens       int
	Reasoning       bool
	InputModalities []string
}

// knownModelSpecs stores verified specs for OpenCode Zen models.
var knownModelSpecs = map[string]modelSpec{
	"space-bunny-free": {
		Name:            "Space Bunny Free",
		ContextWindow:   1048576,
		MaxTokens:       524288,
		Reasoning:       true,
		InputModalities: []string{"text", "image", "video"},
	},
	"mimo-v2.6-flash-free": {
		Name:            "MiMo-V2.6-Flash Free",
		ContextWindow:   200000,
		MaxTokens:       32000,
		Reasoning:       true,
		InputModalities: []string{"text", "image", "audio", "video"},
	},
	"mimo-v2.5-free": {
		Name:            "MiMo V2.5 Free",
		ContextWindow:   200000,
		MaxTokens:       32000,
		Reasoning:       true,
		InputModalities: []string{"text", "image"},
	},
	"mimo-v2-pro-free": {
		Name:            "MiMo V2 Pro Free",
		ContextWindow:   1048576,
		MaxTokens:       64000,
		Reasoning:       true,
		InputModalities: []string{"text", "image"},
	},
	"mimo-v2-flash-free": {
		Name:            "MiMo V2 Flash Free",
		ContextWindow:   262144,
		MaxTokens:       65536,
		Reasoning:       true,
		InputModalities: []string{"text", "image"},
	},
	"mimo-v2-omni-free": {
		Name:            "MiMo V2 Omni Free",
		ContextWindow:   262144,
		MaxTokens:       64000,
		Reasoning:       true,
		InputModalities: []string{"text", "image"},
	},
	"deepseek-v4-flash-free": {
		Name:            "DeepSeek V4 Flash Free",
		ContextWindow:   200000,
		MaxTokens:       128000,
		Reasoning:       true,
		InputModalities: []string{"text", "image"},
	},
	"nemotron-3.5-lightning-free": {
		Name:            "Nemotron 3.5 Lightning Free",
		ContextWindow:   262144,
		MaxTokens:       262144,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
	"nemotron-3-ultra-free": {
		Name:            "Nemotron 3 Ultra Free",
		ContextWindow:   1000000,
		MaxTokens:       128000,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
	"nemotron-3-super-free": {
		Name:            "Nemotron 3 Super Free",
		ContextWindow:   204800,
		MaxTokens:       128000,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
	"muse-spark-1.3-contributor-free": {
		Name:            "Muse Spark 1.3 Free",
		ContextWindow:   1048576,
		MaxTokens:       131072,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
	"muse-spark-1.2-contributor-free": {
		Name:            "Muse Spark 1.2 Free",
		ContextWindow:   1048576,
		MaxTokens:       131072,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
	"glm-5-free": {
		Name:            "GLM-5 Free",
		ContextWindow:   204800,
		MaxTokens:       131072,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
	"glm-4.7-free": {
		Name:            "GLM-4.7 Free",
		ContextWindow:   204800,
		MaxTokens:       131072,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
	"qwen3.6-plus-free": {
		Name:            "Qwen3.6 Plus Free",
		ContextWindow:   262144,
		MaxTokens:       65536,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
	"ling-3.0-flash-fin-free": {
		Name:            "Ling 3.0 Flash Fin Free",
		ContextWindow:   262144,
		MaxTokens:       32768,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
	"jev-1.13-free": {
		Name:            "Jev 1.13 Free",
		ContextWindow:   262144,
		MaxTokens:       65536,
		Reasoning:       true,
		InputModalities: []string{"text"},
	},
}

// defaultFreeModels returns the built-in fallback catalog of free models.
func defaultFreeModels() []ModelInfo {
	list := make([]ModelInfo, 0, len(knownModelSpecs))
	for id := range knownModelSpecs {
		list = append(list, enrichModel(id, "opencode", 1790000000))
	}
	sortModels(list)
	return list
}

func enrichModel(id, ownedBy string, created int64) ModelInfo {
	spec, found := knownModelSpecs[id]
	name := spec.Name
	if !found || name == "" {
		name = cleanModelName(id)
	}
	ctxWindow := spec.ContextWindow
	if ctxWindow <= 0 {
		ctxWindow = 200000
	}
	maxTokens := spec.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 32000
	}
	modalities := spec.InputModalities
	if len(modalities) == 0 {
		modalities = []string{"text"}
	}

	return ModelInfo{
		ID:               id,
		Object:           "model",
		Created:          created,
		OwnedBy:          ownedBy,
		Name:             name,
		DisplayName:      name,
		ContextWindow:    ctxWindow,
		ContextWindowAlt: ctxWindow,
		MaxTokens:        maxTokens,
		MaxTokensAlt:     maxTokens,
		Reasoning:        spec.Reasoning || strings.Contains(id, "free"),
		InputModalities:  modalities,
		Limit: &ModelLimit{
			Context: ctxWindow,
			Output:  maxTokens,
		},
	}
}

func cleanModelName(id string) string {
	parts := strings.Split(id, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func sortModels(models []ModelInfo) {
	sort.Slice(models, func(i, j int) bool {
		score := func(id string) int {
			if id == "space-bunny-free" {
				return 0
			}
			if id == "mimo-v2.6-flash-free" {
				return 1
			}
			if strings.HasSuffix(id, "-free") {
				return 2
			}
			return 3
		}
		si, sj := score(models[i].ID), score(models[j].ID)
		if si != sj {
			return si < sj
		}
		return models[i].ID < models[j].ID
	})
}

// ModelCatalog manages dynamic queries to OpenCode Zen upstream /models endpoint.
type ModelCatalog struct {
	mu           sync.RWMutex
	upstreamURL  string
	costume      *costume
	client       *http.Client
	verbose      bool
	cachedModels []ModelInfo
	lastFetch    time.Time
	cacheTTL     time.Duration
}

// newModelCatalog creates a dynamic catalog instance.
func newModelCatalog(upstreamBase string, costume *costume, client *http.Client, verbose bool) *ModelCatalog {
	modelsURL := strings.TrimRight(upstreamBase, "/") + "/models"
	return &ModelCatalog{
		upstreamURL:  modelsURL,
		costume:      costume,
		client:       client,
		verbose:      verbose,
		cachedModels: defaultFreeModels(),
		cacheTTL:     30 * time.Second, // 30s cache for fast repeat requests
	}
}

// GetModels returns the live model list, fetching directly from OpenCode Zen servers.
func (mc *ModelCatalog) GetModels(ctx context.Context, returnAll bool) []ModelInfo {
	mc.mu.RLock()
	expired := time.Since(mc.lastFetch) > mc.cacheTTL
	models := mc.cachedModels
	mc.mu.RUnlock()

	if expired || len(models) == 0 {
		// Attempt fresh fetch from OpenCode Zen
		fresh, err := mc.fetchFromZen(ctx)
		if err == nil && len(fresh) > 0 {
			mc.mu.Lock()
			mc.cachedModels = fresh
			mc.lastFetch = time.Now()
			models = fresh
			mc.mu.Unlock()
		} else if mc.verbose {
			log.Printf("zen-kostum: upstream /models sorgusu başarısız (%v), önbellek kullanılıyor", err)
		}
	}

	if returnAll {
		return models
	}

	// Filter for free-tier compatible models by default
	freeList := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		if strings.HasSuffix(m.ID, "-free") {
			freeList = append(freeList, m)
		}
	}
	if len(freeList) == 0 {
		return models // fallback to all if none matched
	}
	return freeList
}

// fetchFromZen directly queries OpenCode Zen's /models endpoint using the client costume.
func (mc *ModelCatalog) fetchFromZen(ctx context.Context) ([]ModelInfo, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, mc.upstreamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	mc.costume.dress(req.Header)

	resp, err := mc.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", mc.upstreamURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return parseZenModelsResponse(body)
}

// parseZenModelsResponse converts OpenAI-format /models response into enriched ModelInfo list.
func parseZenModelsResponse(body []byte) ([]ModelInfo, error) {
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal /models: %w", err)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no models in response")
	}

	results := make([]ModelInfo, 0, len(resp.Data))
	for _, item := range resp.Data {
		results = append(results, enrichModel(item.ID, item.OwnedBy, item.Created))
	}

	sortModels(results)
	return results, nil
}
