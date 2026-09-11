package main

import (
	"encoding/json"
	"testing"
)

// TestExtractModelsV3Config uses the exact schema CodeBuddy serves (same field
// names as product-ide-cn.json inside CodeBuddy.app).
func TestExtractModelsV3Config(t *testing.T) {
	raw := json.RawMessage(`{
		"agent": {"agents": null},
		"models": [
			{"id":"glm-5.2","name":"GLM-5.2","vendor":"zhipuai","maxOutputTokens":8192,
			 "maxInputTokens":1000000,"supportsToolCall":true,"supportsImages":true},
			{"id":"hy3-preview-agent","name":"Hy3 Preview Agent","vendor":"tencent",
			 "maxOutputTokens":8192,"maxInputTokens":262144,"supportsToolCall":true}
		],
		"mcp": {"enableFilterCount":0}
	}`)
	got := extractModels(raw)
	if len(got) != 2 {
		t.Fatalf("models = %d, want 2", len(got))
	}
	if got[0].ID != "glm-5.2" || got[0].MaxInputTokens != 1000000 || !got[0].SupportsImages {
		t.Fatalf("first model = %+v", got[0])
	}
	if got[1].ID != "hy3-preview-agent" || got[1].Name != "Hy3 Preview Agent" {
		t.Fatalf("second model = %+v", got[1])
	}
}

func TestExtractModelsShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"bare array", `[{"id":"a"},{"id":"b"}]`, []string{"a", "b"}},
		{"array of id strings", `["a","b","c"]`, []string{"a", "b", "c"}},
		{"models wrapper", `{"models":[{"id":"a"}]}`, []string{"a"}},
		{"list wrapper", `{"list":[{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}},
		{"data wrapper", `{"data":{"models":[{"id":"a"}]}}`, []string{"a"}},
		{"id map", `{"a":{"maxInputTokens":10},"b":{"maxInputTokens":20}}`, []string{"a", "b"}},
		{"null models", `{"models":null}`, nil},
		{"empty object", `{}`, nil},
		{"empty input", ``, nil},
		// Real anonymous /v3/config: sibling config objects must not become models.
		{"v3 config without models", `{"agent":{"agents":null},"mcp":{"enableFilterCount":0},` +
			`"codebase":{"remote":{"disabled":false}},"features":null}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractModels(json.RawMessage(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d models (%v), want %d", len(got), got, len(tc.want))
			}
			for i, id := range tc.want {
				if got[i].ID != id {
					t.Errorf("model[%d].ID = %q, want %q", i, got[i].ID, id)
				}
				if got[i].Name == "" {
					t.Errorf("model[%d].Name should default to the id", i)
				}
			}
		})
	}
}

func TestToModelInfosDefaults(t *testing.T) {
	models := toModelInfos([]upstreamModel{
		{ID: "full", Name: "Full", MaxInputTokens: 262144, MaxOutputTokens: 16384, SupportsImages: true},
		{ID: "bare"},
	})
	if len(models) != 2 {
		t.Fatalf("got %d models", len(models))
	}
	full := models[0]
	if full.ContextLength != 262144 || full.MaxCompletionTokens != 16384 {
		t.Errorf("full limits = %d/%d", full.ContextLength, full.MaxCompletionTokens)
	}
	if len(full.SupportedInputModalities) != 2 {
		t.Errorf("full modalities = %v, want text+image", full.SupportedInputModalities)
	}
	bare := models[1]
	if bare.ContextLength != defaultContextLength || bare.MaxCompletionTokens != defaultMaxCompletion {
		t.Errorf("bare limits = %d/%d, want defaults %d/%d",
			bare.ContextLength, bare.MaxCompletionTokens, defaultContextLength, defaultMaxCompletion)
	}
	if bare.DisplayName != "bare" {
		t.Errorf("bare DisplayName = %q, want id", bare.DisplayName)
	}
	if len(bare.SupportedInputModalities) != 1 {
		t.Errorf("bare modalities = %v, want text only", bare.SupportedInputModalities)
	}
}

// realCatalog is a verbatim slice of a live /v3/config response captured from
// a CodeBuddy CN account (trimmed to the interesting cases, field names and
// nesting untouched). It pins the real schema against future refactors.
const realCatalog = `[
 {"descriptionEn":"Enhanced model with improved performance for various tasks",
  "descriptionZh":"性能增强的模型，适用于各种任务","id":"enhance-1.0","maxOutputTokens":32000,
  "name":"Enhance-1.0","supportsImages":false,"supportsToolCall":true,"vendor":"f"},
 {"credits":"x0.95 credits","descriptionZh":"原生多模态模型","id":"glm-5v-turbo",
  "maxAllowedSize":200000,"maxInputTokens":200000,"maxOutputTokens":38000,"name":"GLM-5v-Turbo",
  "onlyReasoning":true,"reasoning":{"effort":"medium","summary":"auto"},"supportsImages":true,
  "supportsReasoning":true,"supportsToolCall":true,"temperature":1,"vendor":"e"},
 {"credits":"x3.31 credits","id":"gpt-5.5","maxAllowedSize":1000000,"maxInputTokens":1000000,
  "maxOutputTokens":72000,"name":"GPT-5.5","onlyReasoning":true,"reasoning":{"effort":"high"},
  "supportsImages":true,"supportsReasoning":true,"supportsToolCall":true,"vendor":"e"},
 {"id":"deepseek-v3.2","maxInputTokens":96000,"maxOutputTokens":32000,"name":"DeepSeek-V3.2",
  "supportsImages":false,"supportsReasoning":true,"supportsToolCall":true,"vendor":"f"},
 {"id":"gpt-5.1","maxInputTokens":272000,"maxOutputTokens":72000,"name":"GPT-5.1",
  "supportsImages":true,"supportsToolCall":true,"vendor":"e"},
 {"id":"completion-1.0","maxOutputTokens":256,"name":"completion-1.0"},
 {"id":"nes-1.2","maxInputTokens":32000,"maxOutputTokens":8192,"name":"auto","vendor":"f"},
 {"id":"completion-1.2","maxOutputTokens":256,"name":"completion-1.2"}
]`

func TestRealCatalog(t *testing.T) {
	models := toModelInfos(extractModels(json.RawMessage(`{"models":` + realCatalog + `}`)))

	byID := map[string]bool{}
	for _, m := range models {
		byID[m.ID] = true
	}
	// Internal completion / NES / prompt-enhancement models must be filtered out.
	for _, hidden := range []string{"completion-1.0", "completion-1.2", "nes-1.2", "enhance-1.0"} {
		if byID[hidden] {
			t.Errorf("service model %q should not be published", hidden)
		}
	}
	// Real chat models must survive.
	for _, want := range []string{"glm-5v-turbo", "gpt-5.5", "gpt-5.1", "deepseek-v3.2"} {
		if !byID[want] {
			t.Errorf("chat model %q missing from %v", want, byID)
		}
	}
	if len(models) != 4 {
		t.Fatalf("published %d models, want 4: %v", len(models), byID)
	}

	glm := models[0]
	if glm.DisplayName != "GLM-5v-Turbo" || glm.ContextLength != 200000 || glm.MaxCompletionTokens != 38000 {
		t.Errorf("glm mapped wrong: %+v", glm)
	}
	if glm.Description != "原生多模态模型" {
		t.Errorf("glm Description = %q, want the Chinese description", glm.Description)
	}
	if len(glm.SupportedInputModalities) != 2 {
		t.Errorf("glm is multimodal, modalities = %v", glm.SupportedInputModalities)
	}

	ds := byID["deepseek-v3.2"]
	if !ds {
		return
	}
	for _, m := range models {
		if m.ID == "deepseek-v3.2" && len(m.SupportedInputModalities) != 1 {
			t.Errorf("deepseek-v3.2 is text-only, modalities = %v", m.SupportedInputModalities)
		}
	}
}

func TestFallbackModels(t *testing.T) {
	models := fallbackModels()
	if len(models) != 7 {
		t.Fatalf("fallback models = %d, want 7", len(models))
	}
	for _, m := range models {
		if m.ID == "" || m.OwnedBy != providerName || m.Object != "model" {
			t.Errorf("model %+v missing required fields", m)
		}
	}
}
