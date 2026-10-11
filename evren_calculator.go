package main

import (
	"encoding/json"
	"math"
)

// UsageResult represents calculated token usage metrics for a request
type UsageResult struct {
	PromptTokens     int64   `json:"prompt_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	UncachedPrompt   int64   `json:"uncached_prompt"`
	CachedWeighted   int64   `json:"cached_weighted"` // Cached * %3 (EVREN resmi önbellek ağırlığı)
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"` // Yanıtlarda ayrıca yakalanırsa (completion_tokens_details.reasoning_tokens)
	TotalTokens      int64   `json:"total_tokens"`
	NetDeduction     int64   `json:"net_deduction"`   // EVREN Formülü: (Prompt - Cached) + (Cached * 0.03) + Completion
	GrossDeduction   int64   `json:"gross_deduction"` // Standart Formül: Prompt + Completion
	CacheHitRatio    float64 `json:"cache_hit_ratio"` // (Cached / Prompt) * 100
}

// CalculateUsage calculates the exact token deductions according to EVREN's official formula
func CalculateUsage(prompt, cached, completion int64) UsageResult {
	if prompt < 0 {
		prompt = 0
	}
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	if completion < 0 {
		completion = 0
	}

	uncachedPrompt := prompt - cached
	// EVREN Resmi Kuralı: Önbellekten gelen girdi token'ları kotaya %3 ağırlıkla yazılır.
	cachedWeighted := int64(math.Round(float64(cached) * 0.03))

	netDeduction := uncachedPrompt + cachedWeighted + completion
	grossDeduction := prompt + completion

	var cacheHitRatio float64
	if prompt > 0 {
		cacheHitRatio = (float64(cached) / float64(prompt)) * 100.0
		cacheHitRatio = math.Round(cacheHitRatio*100) / 100.0
	}

	return UsageResult{
		PromptTokens:     prompt,
		CachedTokens:     cached,
		UncachedPrompt:   uncachedPrompt,
		CachedWeighted:   cachedWeighted,
		CompletionTokens: completion,
		TotalTokens:      grossDeduction,
		NetDeduction:     netDeduction,
		GrossDeduction:   grossDeduction,
		CacheHitRatio:    cacheHitRatio,
	}
}

// RawUsagePayload parses flexible usage formats from upstream LLMs
type RawUsagePayload struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	InputTokens      *int64 `json:"input_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
	ReasoningTokens  *int64 `json:"reasoning_tokens"` // bazı sağlayıcılar usage köküne yazar

	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`

	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`

	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`

	OutputTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// ParseUsageFromBytes extracts usage details from any response payload
func ParseUsageFromBytes(data []byte) (UsageResult, bool) {
	// First check if root contains "usage"
	var wrapper struct {
		Usage *RawUsagePayload `json:"usage"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Usage != nil {
		return extractFromPayload(wrapper.Usage)
	}

	// Try direct usage object
	var direct RawUsagePayload
	if err := json.Unmarshal(data, &direct); err == nil && (direct.PromptTokens != nil || direct.InputTokens != nil) {
		return extractFromPayload(&direct)
	}

	return UsageResult{}, false
}

func extractFromPayload(p *RawUsagePayload) (UsageResult, bool) {
	var prompt int64
	if p.PromptTokens != nil {
		prompt = *p.PromptTokens
	} else if p.InputTokens != nil {
		prompt = *p.InputTokens
	}

	var completion int64
	if p.CompletionTokens != nil {
		completion = *p.CompletionTokens
	} else if p.OutputTokens != nil {
		completion = *p.OutputTokens
	}

	var cached int64
	if p.PromptTokensDetails != nil && p.PromptTokensDetails.CachedTokens != nil {
		cached = *p.PromptTokensDetails.CachedTokens
	} else if p.InputTokensDetails != nil && p.InputTokensDetails.CachedTokens != nil {
		cached = *p.InputTokensDetails.CachedTokens
	}

	if prompt == 0 && completion == 0 && cached == 0 {
		return UsageResult{}, false
	}

	usage := CalculateUsage(prompt, cached, completion)
	// Reasoning: önce completion/output detayları, yoksa usage kökündeki alan.
	switch {
	case p.CompletionTokensDetails != nil && p.CompletionTokensDetails.ReasoningTokens != nil:
		usage.ReasoningTokens = *p.CompletionTokensDetails.ReasoningTokens
	case p.OutputTokensDetails != nil && p.OutputTokensDetails.ReasoningTokens != nil:
		usage.ReasoningTokens = *p.OutputTokensDetails.ReasoningTokens
	case p.ReasoningTokens != nil:
		usage.ReasoningTokens = *p.ReasoningTokens
	}
	return usage, true
}
