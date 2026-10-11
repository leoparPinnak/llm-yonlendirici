package main

import (
	"math"
	"testing"
)

func TestCalculateUsage_ScreenshotRow1(t *testing.T) {
	// Row 1: 27 Eyl 21:00–21:59 | mimo-v2.6-pro | 13 | 3.860.454 | 3.531.744 (%92) | 434.663 | 0,00
	prompt := int64(3860454)
	cached := int64(3531744)
	completion := int64(434663)

	res := CalculateUsage(prompt, cached, completion)

	expectedUncachedPrompt := int64(328710)
	if res.UncachedPrompt != expectedUncachedPrompt {
		t.Errorf("UncachedPrompt beklenen %d, alınan %d", expectedUncachedPrompt, res.UncachedPrompt)
	}

	// EVREN resmi %3 ağırlık: 3.531.744 * 0.03 = 105.952,32 -> 105.952
	expectedCachedWeighted := int64(105952)
	if res.CachedWeighted != expectedCachedWeighted {
		t.Errorf("CachedWeighted beklenen %d, alınan %d", expectedCachedWeighted, res.CachedWeighted)
	}

	// Net: 328.710 + 105.952 + 434.663 = 869.325
	expectedNet := int64(869325)
	if res.NetDeduction != expectedNet {
		t.Errorf("NetDeduction beklenen %d, alınan %d", expectedNet, res.NetDeduction)
	}

	expectedGross := int64(4295117)
	if res.GrossDeduction != expectedGross {
		t.Errorf("GrossDeduction beklenen %d, alınan %d", expectedGross, res.GrossDeduction)
	}

	if res.CacheHitRatio < 91.4 || res.CacheHitRatio > 91.6 {
		t.Errorf("CacheHitRatio beklenen ~91.48%%, alınan %.2f%%", res.CacheHitRatio)
	}
}

func TestCalculateUsage_ScreenshotFullDay(t *testing.T) {
	// Görseldeki 7 saatin tamamının toplam resmi hesaplaması (%3 ağırlıklı cache):
	rows := []struct {
		prompt, cached, completion int64
	}{
		{3860454, 3531744, 434663}, // 21:00
		{6525154, 5603536, 1089725}, // 20:00
		{9050715, 8033888, 1257844}, // 19:00
		{7594651, 6643456, 1150499}, // 18:00
		{8184, 0, 8184},             // 05:00 qwen
		{8571794, 7922896, 886585},  // 05:00 mimo
		{2010524, 1676496, 384323},  // 04:00 mimo
	}

	var totalNet, totalGross, totalCached int64
	for _, r := range rows {
		res := CalculateUsage(r.prompt, r.cached, r.completion)
		totalNet += res.NetDeduction
		totalGross += res.GrossDeduction
		totalCached += res.CachedTokens
	}

	// 10.423.644 token: 21:59'da 10M limitine ulaşılarak HTTP 429'a girildiği an!
	if math.Abs(float64(totalNet-10423644)) > 1 {
		t.Errorf("Günlük toplam net harcama beklenen 10423644 (±1), alınan %d", totalNet)
	}
}

// TestParseUsage_Reasoning, yanıt usage gövdesindeki reasoning token'larının
// (completion_tokens_details.reasoning_tokens / usage kökü) UsageResult'a
// ayrıca yazıldığını doğrular — kartlarda input/output/reasoning ayrımı için.
func TestParseUsage_Reasoning(t *testing.T) {
	raw := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":10},"completion_tokens_details":{"reasoning_tokens":17}}}`)
	usage, ok := ParseUsageFromBytes(raw)
	if !ok {
		t.Fatal("usage çözümlenemedi")
	}
	if usage.ReasoningTokens != 17 {
		t.Fatalf("reasoning = %d, beklenen 17", usage.ReasoningTokens)
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 40 || usage.CachedTokens != 10 {
		t.Fatalf("temel alanlar bozuldu: %+v", usage)
	}

	// Bazı sağlayıcılar reasoning alanını usage köküne yazar.
	raw2 := []byte(`{"usage":{"prompt_tokens":5,"completion_tokens":2,"reasoning_tokens":3}}`)
	u2, ok2 := ParseUsageFromBytes(raw2)
	if !ok2 {
		t.Fatal("kök usage çözümlenemedi")
	}
	if u2.ReasoningTokens != 3 {
		t.Fatalf("kök reasoning = %d, beklenen 3", u2.ReasoningTokens)
	}

	// Reasoning alanlı olmayan gövdeler hata değil, 0 döner.
	raw3 := []byte(`{"usage":{"prompt_tokens":7,"completion_tokens":4}}`)
	u3, ok3 := ParseUsageFromBytes(raw3)
	if !ok3 || u3.ReasoningTokens != 0 {
		t.Fatalf("reasoning'siz usage 0 olmalı (ok=%v, r=%d)", ok3, u3.ReasoningTokens)
	}
}
