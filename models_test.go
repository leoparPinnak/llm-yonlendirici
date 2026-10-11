package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDefaultFreeModelsContainKeyModels(t *testing.T) {
	models := defaultFreeModels()
	foundMimo := false
	foundSpaceBunny := false

	for _, m := range models {
		if m.ID == "mimo-v2.6-flash-free" {
			foundMimo = true
			if m.ContextWindow != 200000 || m.MaxTokens != 32000 || !m.Reasoning {
				t.Errorf("mimo model has unexpected attributes: %+v", m)
			}
		}
		if m.ID == "space-bunny-free" {
			foundSpaceBunny = true
			if m.ContextWindow != 1048576 || m.MaxTokens != 524288 || !m.Reasoning {
				t.Errorf("space bunny model has unexpected attributes: %+v", m)
			}
			if len(m.InputModalities) == 0 {
				t.Errorf("space bunny model missing input modalities")
			}
		}
	}

	if !foundMimo {
		t.Error("default free models missing mimo-v2.6-flash-free")
	}
	if !foundSpaceBunny {
		t.Error("default free models missing space-bunny-free")
	}
}

func TestParseZenModelsResponse(t *testing.T) {
	sampleResp := []byte(`{
		"object": "list",
		"data": [
			{"id": "claude-sonnet-5", "object": "model", "created": 1790000000, "owned_by": "opencode"},
			{"id": "space-bunny-free", "object": "model", "created": 1790000000, "owned_by": "opencode"},
			{"id": "mimo-v2.6-flash-free", "object": "model", "created": 1790000000, "owned_by": "opencode"},
			{"id": "custom-unknown-free", "object": "model", "created": 1790000000, "owned_by": "opencode"}
		]
	}`)

	models, err := parseZenModelsResponse(sampleResp)
	if err != nil {
		t.Fatalf("failed to parse models response: %v", err)
	}

	if len(models) != 4 {
		t.Fatalf("expected 4 models, got %d", len(models))
	}

	// First two should be space-bunny-free and mimo-v2.6-flash-free due to prioritization
	if models[0].ID != "space-bunny-free" {
		t.Errorf("expected space-bunny-free first, got %s", models[0].ID)
	}
	if models[0].ContextWindow != 1048576 {
		t.Errorf("expected context 1048576, got %d", models[0].ContextWindow)
	}
	if models[0].MaxTokens != 524288 {
		t.Errorf("expected max tokens 524288, got %d", models[0].MaxTokens)
	}

	if models[1].ID != "mimo-v2.6-flash-free" {
		t.Errorf("expected mimo-v2.6-flash-free second, got %s", models[1].ID)
	}

	// Unknown model should get reasonable defaults
	var unknown *ModelInfo
	for i := range models {
		if models[i].ID == "custom-unknown-free" {
			unknown = &models[i]
			break
		}
	}
	if unknown == nil {
		t.Fatal("custom-unknown-free missing")
	}
	if unknown.ContextWindow <= 0 || unknown.Name == "" {
		t.Errorf("unknown model did not receive defaults: %+v", unknown)
	}
}

func TestModelCatalogDirectUpstreamQuery(t *testing.T) {
	serverCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalled = true
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		// Verify costume headers reached upstream
		if r.Header.Get("Authorization") != "Bearer public" {
			t.Errorf("Authorization = %q, want Bearer public", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Opencode-Client") != "cli" {
			t.Errorf("X-Opencode-Client missing or wrong")
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": "paid-model", "object": "model", "created": 1790000000, "owned_by": "opencode"},
				{"id": "space-bunny-free", "object": "model", "created": 1790000000, "owned_by": "opencode"}
			]
		}`))
	}))
	defer server.Close()

	cost := newCostume("opencode/test/1.0/cli")
	catalog := newModelCatalog(server.URL, cost, server.Client(), false)

	// By default, GetModels filters for free models only
	models := catalog.GetModels(context.Background(), false)
	if !serverCalled {
		t.Error("upstream /models was not called")
	}
	if len(models) != 1 || models[0].ID != "space-bunny-free" {
		t.Errorf("expected 1 free model space-bunny-free, got %+v", models)
	}

	// With returnAll: true, paid-model should also be returned
	allModels := catalog.GetModels(context.Background(), true)
	if len(allModels) != 2 {
		t.Errorf("expected 2 models with returnAll=true, got %d", len(allModels))
	}
}
