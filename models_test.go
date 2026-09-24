package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
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

// TestToModelInfosContextWindow pins the selectable context budget. The catalog
// ships models whose default window sits far below the hard input cap
// (hy4-preview advertises maxInputTokens 1000000 but defaults to a 200000
// window), so publishing maxInputTokens tells clients to build prompts this
// account cannot actually serve.
func TestToModelInfosContextWindow(t *testing.T) {
	models := toModelInfos([]upstreamModel{
		// Real hy4-preview entry: the default window wins over the 1M hard cap.
		{ID: "hy4-preview", Name: "hy4 preview", MaxInputTokens: 1000000, MaxOutputTokens: 64000,
			ContextWindow: &contextWindow{DefaultLength: 200000, SupportedLengths: []int64{200000, 1000000}}},
		// deepseek-v4.1-flash: same shape, larger default.
		{ID: "deepseek-v4.1-flash", MaxInputTokens: 1000000, MaxOutputTokens: 128000,
			ContextWindow: &contextWindow{DefaultLength: 300000, SupportedLengths: []int64{300000, 1000000}}},
		// A default that is not selectable falls back to the smallest supported
		// length, matching resolveEffectiveContextBudget in the CodeBuddy CLI.
		{ID: "odd-default", MaxInputTokens: 1000000,
			ContextWindow: &contextWindow{DefaultLength: 7, SupportedLengths: []int64{500000, 1000000}}},
		// A single supported length is not a selectable budget, so the raw cap
		// still applies.
		{ID: "single-length", MaxInputTokens: 272000,
			ContextWindow: &contextWindow{DefaultLength: 272000, SupportedLengths: []int64{272000}}},
		// No context block at all: unchanged behaviour.
		{ID: "plain", MaxInputTokens: 96000},
	})
	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	want := map[string]int64{
		"hy4-preview":         200000,
		"deepseek-v4.1-flash": 300000,
		"odd-default":         500000,
		"single-length":       272000,
		"plain":               96000,
	}
	for id, wantContext := range want {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("model %q missing", id)
		}
		if got.ContextLength != wantContext {
			t.Errorf("%s ContextLength = %d, want %d", id, got.ContextLength, wantContext)
		}
	}
	// Output limits stay independent of the context window.
	if got := byID["hy4-preview"].MaxCompletionTokens; got != 64000 {
		t.Errorf("hy4-preview MaxCompletionTokens = %d, want 64000", got)
	}
}

// TestContextWindowNeverExceedsMaxInput guards the invariant that a selectable
// budget never advertises more than the hard input cap, whatever the catalog
// puts in supportedLengths.
func TestContextWindowNeverExceedsMaxInput(t *testing.T) {
	m := upstreamModel{ID: "x", MaxInputTokens: 300000,
		ContextWindow: &contextWindow{DefaultLength: 1000000, SupportedLengths: []int64{1000000, 2000000}}}
	if got := m.effectiveContextLength(); got != 300000 {
		t.Errorf("effectiveContextLength = %d, want the 300000 input cap", got)
	}
}

// TestNormalizeContextLengthsIsDeduplicated keeps a catalog that repeats one
// length from looking like a two-option selectable budget.
func TestNormalizeContextLengthsIsDeduplicated(t *testing.T) {
	got := normalizeContextLengths([]int64{500000, 500000, 1000000}, 1000000)
	if len(got) != 2 || got[0] != 500000 || got[1] != 1000000 {
		t.Errorf("normalizeContextLengths = %v, want [500000 1000000]", got)
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

// TestRealCatalogContextWindow replays a verbatim slice of a live Global
// /v3/config response: the models that carry a selectable contextWindow are the
// ones whose advertised window used to be wrong.
func TestRealCatalogContextWindow(t *testing.T) {
	const globalCatalog = `[
 {"contextWindow":{"defaultLength":300000,"supportedLengths":[300000,1000000]},
  "credits":"x0.00","descriptionZh":"DeepSeek 旗舰模型","id":"deepseek-v4.1-flash",
  "maxAllowedSize":1000000,"maxInputTokens":1000000,"maxOutputTokens":128000,
  "name":"Deepseek-V4.1-Flash","supportsImages":true,"supportsReasoning":true,
  "supportsToolCall":true,"vendor":"f"},
 {"contextWindow":{"defaultLength":200000,"supportedLengths":[200000,1000000]},
  "credits":"x0.00","descriptionZh":"混元思考模型","id":"hy4-preview",
  "maxAllowedSize":1000000,"maxInputTokens":1000000,"maxOutputTokens":64000,
  "name":"Hy4 preview","supportsReasoning":true,"supportsToolCall":true,"vendor":"j"},
 {"credits":"x0.57","id":"hy3","maxAllowedSize":192000,"maxInputTokens":192000,
  "maxOutputTokens":64000,"name":"Hy3","supportsToolCall":true,"vendor":"j"},
 {"credits":"","id":"default-model","isDefault":true,"name":"Auto",
  "maxInputTokens":176000,"maxOutputTokens":24000,"supportsImages":true,
  "supportsToolCall":true,"vendor":"e"}
]`
	raw := extractModels(json.RawMessage(`{"models":` + globalCatalog + `}`))
	// The isDefault tag has to survive decoding for the alias filter to work.
	// A struct literal in another test would not catch a mistyped json tag.
	decoded := map[string]bool{}
	for _, m := range raw {
		decoded[m.ID] = m.IsDefault
	}
	if !decoded["default-model"] {
		t.Fatalf("isDefault did not decode: %v", raw)
	}
	models := toModelInfos(raw)
	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if len(models) != 3 {
		t.Fatalf("published %d models, want 3 (default-model must be dropped)", len(models))
	}
	// The default window, not the 1M hard cap, is what this account can serve.
	for id, want := range map[string]int64{"deepseek-v4.1-flash": 300000, "hy4-preview": 200000, "hy3": 192000} {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("model %q missing", id)
		}
		if got.ContextLength != want {
			t.Errorf("%s ContextLength = %d, want %d", id, got.ContextLength, want)
		}
	}
}

// TestToModelInfosHidesRoutingAlias keeps upstream's "pick a backend for me"
// entries out of the published list. Global's default-model and CN's auto both
// carry isDefault; a priced id a user may pick (CN's `default`, x2.00 credits)
// must still be published — only the isDefault aliases are suppressed.
func TestToModelInfosHidesRoutingAlias(t *testing.T) {
	models := toModelInfos([]upstreamModel{
		{ID: "default-model", Name: "Auto", IsDefault: true},
		{ID: "auto", Name: "Auto", IsDefault: true},
		{ID: "default", Name: "Default", Credits: "x2.00 credits"},
		{ID: "hy3", Name: "Hy3"},
	})
	got := map[string]bool{}
	for _, m := range models {
		got[m.ID] = true
	}
	if got["default-model"] || got["auto"] {
		t.Fatalf("alias leaked into published list: %v", models)
	}
	if !got["default"] || !got["hy3"] {
		t.Fatalf("priced id wrongly suppressed: %v", models)
	}
}
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

// TestExtraModelSpecDecodesBothForms covers the two ways extra_models can be
// written: a bare id (historical form, takes the defaults) and a mapping that
// carries the catalog fields so a hidden model gets published with the limits
// upstream actually enforces.
func TestExtraModelSpecDecodesBothForms(t *testing.T) {
	const cfg = `
extra_models:
  - hy4-preview-f
  - id: hy4-preview-x
    name: Hy4 preview
    maxInputTokens: 1000000
    maxOutputTokens: 64000
    supportsImages: true
    contextWindow:
      defaultLength: 200000
      supportedLengths: [200000, 1000000]
`
	var parsed struct {
		ExtraModels []extraModelSpec `yaml:"extra_models"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if len(parsed.ExtraModels) != 2 {
		t.Fatalf("decoded %d entries, want 2", len(parsed.ExtraModels))
	}

	bare := parsed.ExtraModels[0].upstreamModel
	// Name stays empty on purpose so the live catalog can supply the real one;
	// the id is the downstream fallback when nothing provides a name.
	if bare.ID != "hy4-preview-f" || bare.Name != "" {
		t.Errorf("bare entry = %+v, want id hy4-preview-f and an empty name", bare)
	}
	if bare.MaxInputTokens != 0 || bare.ContextWindow != nil {
		t.Errorf("bare entry carries metadata: %+v", bare)
	}

	full := parsed.ExtraModels[1].upstreamModel
	if full.ID != "hy4-preview-x" || full.Name != "Hy4 preview" {
		t.Errorf("mapping entry = %+v", full)
	}
	if full.MaxInputTokens != 1000000 || full.MaxOutputTokens != 64000 {
		t.Errorf("mapping limits = %d/%d, want 1000000/64000", full.MaxInputTokens, full.MaxOutputTokens)
	}
	if !full.SupportsImages {
		t.Error("mapping entry lost supportsImages")
	}
	if got := full.effectiveContextLength(); got != 200000 {
		t.Errorf("effectiveContextLength = %d, want the 200000 default window", got)
	}
}

// TestAppendExtraModelsUsesConfiguredLimits is the regression this change
// exists for: the Global realm hides the hy4 family from /v3/config, so its
// limits can only come from configuration. A configured entry must publish the
// real output cap and the default window instead of the 200000/8192 defaults.
func TestAppendExtraModelsUsesConfiguredLimits(t *testing.T) {
	restore := configuredExtraModels()
	t.Cleanup(func() { setConfiguredExtraModelsForTest(restore) })

	base := []pluginapi.ModelInfo{{ID: "hy3", ContextLength: 192000}}
	setConfiguredExtraModelsForTest([]extraModelSpec{
		{upstreamModel{ID: "hy4-preview-f", Name: "hy4-preview-f"}},
		{upstreamModel{
			ID:              "hy4-preview-x",
			Name:            "Hy4 preview",
			MaxInputTokens:  1000000,
			MaxOutputTokens: 64000,
			ContextWindow:   &contextWindow{DefaultLength: 200000, SupportedLengths: []int64{200000, 1000000}},
		}},
	})

	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range appendExtraModels(base, nil) {
		byID[m.ID] = m
	}

	// No live catalog, but the id is one whose limits were measured off the
	// real API, so those win over the generic defaults.
	if got := byID["hy4-preview-f"].ContextLength; got != 1000000 {
		t.Errorf("bare hy4-preview-f ContextLength = %d, want the measured 1000000", got)
	}
	if got := byID["hy4-preview-f"].MaxCompletionTokens; got != 64000 {
		t.Errorf("bare hy4-preview-f MaxCompletionTokens = %d, want the measured 64000", got)
	}

	// Configured entry: real limits reach the client.
	full := byID["hy4-preview-x"]
	if full.MaxCompletionTokens != 64000 {
		t.Errorf("hy4-preview-x MaxCompletionTokens = %d, want 64000", full.MaxCompletionTokens)
	}
	if full.ContextLength != 200000 {
		t.Errorf("hy4-preview-x ContextLength = %d, want the 200000 default window", full.ContextLength)
	}
	if full.SupportedInputModalities[0] != "text" {
		t.Errorf("hy4-preview-x modalities = %v", full.SupportedInputModalities)
	}
	if full.DisplayName != "Hy4 preview" {
		t.Errorf("hy4-preview-x DisplayName = %q", full.DisplayName)
	}
}

// TestAppendExtraModelsFillsFromLiveCatalog is the behaviour this change
// exists for: a bare configured id must pick up the limits the live catalog
// reports, on every refresh, instead of keeping the 200000/8192 defaults. That
// is what keeps the published numbers equal to the ones the console API shows.
func TestAppendExtraModelsFillsFromLiveCatalog(t *testing.T) {
	restore := configuredExtraModels()
	t.Cleanup(func() { setConfiguredExtraModelsForTest(restore) })

	// What the console API returns for the Global realm today.
	live := []upstreamModel{{
		ID:              "hy4-preview",
		Name:            "Hy4 preview",
		MaxInputTokens:  1000000,
		MaxOutputTokens: 64000,
		SupportsImages:  true,
		ContextWindow:   &contextWindow{DefaultLength: 200000, SupportedLengths: []int64{200000, 1000000}},
	}}

	// Configured as bare ids, the way every existing deployment has them.
	setConfiguredExtraModelsForTest(extraIDs("hy4-preview"))

	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range appendExtraModels(nil, live) {
		byID[m.ID] = m
	}
	got := byID["hy4-preview"]

	// Real output cap, not the 8192 default.
	if got.MaxCompletionTokens != 64000 {
		t.Errorf("MaxCompletionTokens = %d, want 64000 from the live catalog", got.MaxCompletionTokens)
	}
	// The default window, resolved the same way a discovered model would be.
	if got.ContextLength != 200000 {
		t.Errorf("ContextLength = %d, want the 200000 default window", got.ContextLength)
	}
	if got.DisplayName != "Hy4 preview" {
		t.Errorf("DisplayName = %q, want the live name", got.DisplayName)
	}
	if len(got.SupportedInputModalities) != 2 || got.SupportedInputModalities[1] != "image" {
		t.Errorf("modalities = %v, want text+image", got.SupportedInputModalities)
	}
}

// TestAppendExtraModelsConfigWinsOverLive keeps an explicit override winning:
// an operator who pins a number is stating a fact about their deployment, so
// the live value must not clobber it.
func TestAppendExtraModelsConfigWinsOverLive(t *testing.T) {
	restore := configuredExtraModels()
	t.Cleanup(func() { setConfiguredExtraModelsForTest(restore) })

	live := []upstreamModel{{
		ID:              "hy4-preview",
		MaxInputTokens:  1000000,
		MaxOutputTokens: 64000,
		ContextWindow:   &contextWindow{DefaultLength: 200000, SupportedLengths: []int64{200000, 1000000}},
	}}
	setConfiguredExtraModelsForTest([]extraModelSpec{{upstreamModel{
		ID:              "hy4-preview",
		MaxInputTokens:  1000000,
		MaxOutputTokens: 32000, // deliberate cap below upstream
		ContextWindow:   &contextWindow{DefaultLength: 1000000, SupportedLengths: []int64{200000, 1000000}},
	}}})

	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range appendExtraModels(nil, live) {
		byID[m.ID] = m
	}
	got := byID["hy4-preview"]
	if got.MaxCompletionTokens != 32000 {
		t.Errorf("MaxCompletionTokens = %d, want the configured 32000", got.MaxCompletionTokens)
	}
	if got.ContextLength != 1000000 {
		t.Errorf("ContextLength = %d, want the configured 1000000 window", got.ContextLength)
	}
}

// TestMergeCatalogsKeepsBothSides covers why both endpoints are fetched: each
// lists models the other omits, so reading only one silently drops models the
// account can serve.
func TestMergeCatalogsKeepsBothSides(t *testing.T) {
	console := []upstreamModel{
		{ID: "hy4-preview-x", MaxInputTokens: 1000000, MaxOutputTokens: 64000},
		{ID: "hy3", MaxInputTokens: 192000, MaxOutputTokens: 64000},
	}
	v3 := []upstreamModel{
		{ID: "hy4-preview-f", MaxInputTokens: 1000000, MaxOutputTokens: 64000},
		// Same id, but no contextWindow: the console entry is richer and wins.
		{ID: "hy3", MaxInputTokens: 192000, MaxOutputTokens: 64000},
	}

	byID := map[string]upstreamModel{}
	for _, m := range mergeCatalogs(console, v3) {
		byID[m.ID] = m
	}
	if len(byID) != 3 {
		t.Fatalf("merged %d models, want 3", len(byID))
	}
	for _, id := range []string{"hy4-preview-x", "hy4-preview-f", "hy3"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("merge lost %q", id)
		}
	}
}

// TestMergeCatalogsPrefersContextWindow guards the tie-break: when both sides
// describe an id, the one carrying the selectable budget wins, because a bare
// entry would fall back to the built-in defaults.
func TestMergeCatalogsPrefersContextWindow(t *testing.T) {
	console := []upstreamModel{{
		ID:              "hy4-preview",
		MaxInputTokens:  1000000,
		MaxOutputTokens: 64000,
		ContextWindow:   &contextWindow{DefaultLength: 200000, SupportedLengths: []int64{200000, 1000000}},
	}}
	v3 := []upstreamModel{{ID: "hy4-preview", MaxInputTokens: 1000000, MaxOutputTokens: 64000}}

	merged := mergeCatalogs(console, v3)
	if len(merged) != 1 {
		t.Fatalf("merged %d, want 1", len(merged))
	}
	if merged[0].ContextWindow == nil {
		t.Error("merge dropped the contextWindow from the console entry")
	}
	// Order must not matter: the richer entry wins either way.
	if other := mergeCatalogs(v3, console); len(other) == 1 && other[0].ContextWindow == nil {
		t.Error("merge is order-dependent: reversed input lost the contextWindow")
	}
}

// TestMeasuredValuesMatchLiveAPI is a tripwire against the hardcoded table
// drifting from reality: if the upstream numbers change, these fail and the
// table gets revisited instead of silently serving stale limits.
func TestMeasuredValuesMatchLiveAPI(t *testing.T) {
	cases := []struct {
		id           string
		wantInput    int64
		wantOutput   int64
		wantCtx      int64
		wantSupports bool
	}{
		// hy4-preview: only id carrying a selectable budget, so it resolves to
		// the 200000 default window rather than the 1M hard cap.
		{"hy4-preview", 1000000, 64000, 200000, true},
		{"hy4-preview-f", 1000000, 64000, 1000000, true},
		{"hy4-preview-x", 1000000, 64000, 1000000, true},
	}
	setConfiguredExtraModelsForTest(nil)
	defer setConfiguredExtraModelsForTest(nil)

	var ids []string
	for _, c := range cases {
		ids = append(ids, c.id)
	}
	// Empty catalog: forces the measured table to be the only source.
	setConfiguredExtraModelsForTest(extraIDs(ids...))
	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range appendExtraModels(nil, nil) {
		byID[m.ID] = m
	}
	for _, c := range cases {
		got, ok := byID[c.id]
		if !ok {
			t.Errorf("%s missing from published list", c.id)
			continue
		}
		if got.ContextLength != c.wantCtx {
			t.Errorf("%s ContextLength = %d, want %d", c.id, got.ContextLength, c.wantCtx)
		}
		if got.MaxCompletionTokens != c.wantOutput {
			t.Errorf("%s MaxCompletionTokens = %d, want %d", c.id, got.MaxCompletionTokens, c.wantOutput)
		}
		if len(got.SupportedInputModalities) < 2 && c.wantSupports {
			t.Errorf("%s modalities = %v, want text+image", c.id, got.SupportedInputModalities)
		}
	}
}

// TestLiveCatalogBeatsMeasuredTable pins the precedence: a live catalog entry
// is fresher than the baked-in measurement and must win.
func TestLiveCatalogBeatsMeasuredTable(t *testing.T) {
	setConfiguredExtraModelsForTest(extraIDs("hy4-preview"))
	defer setConfiguredExtraModelsForTest(nil)

	live := []upstreamModel{{
		ID:              "hy4-preview",
		Name:            "Hy4 preview",
		MaxInputTokens:  500000, // upstream revised its limits downward
		MaxOutputTokens: 32000,
	}}

	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range appendExtraModels(nil, live) {
		byID[m.ID] = m
	}
	got := byID["hy4-preview"]
	if got.ContextLength != 500000 || got.MaxCompletionTokens != 32000 {
		t.Errorf("got ctx=%d out=%d, want the live 500000/32000 to override the measured table",
			got.ContextLength, got.MaxCompletionTokens)
	}
}

// configEnvelope builds the plugin.register payload the host sends with a
// config block attached.
func configEnvelope(t *testing.T, cfgYAML string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"config_yaml": []byte(cfgYAML)})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

// withPluginConfig installs a config block for one test and restores the
// previous one afterwards, so a mode set here cannot leak into another test.
func withPluginConfig(t *testing.T, cfgYAML string) {
	t.Helper()
	applyConfigYAML(configEnvelope(t, cfgYAML))
	t.Cleanup(func() {
		applyConfigYAML(configEnvelope(t, "publish_mode: \"\"\npublish_allow: []\nextra_models: []\nmodel_prefix: \"\"\n"))
	})
}

func publishIDs(models []upstreamModel) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

// TestApplyPublishPolicyAllowLeavesOtherProvidersAlone pins the guard against
// the failure this mode exists for: with force-model-prefix unset the host
// matches a bare id against "<prefix>/<id>" too, so a plugin publishing
// global/gpt-5.6-luna also captures plain gpt-5.6-luna — traffic that belongs
// to the codex/openai provider and that this account cannot serve.
func TestApplyPublishPolicyAllowLeavesOtherProvidersAlone(t *testing.T) {
	withPluginConfig(t, `
publish_mode: allow
publish_allow:
  - hy3
extra_models:
  - hy4-preview-f
`)
	remote := []upstreamModel{
		{ID: "hy3"},
		{ID: "hy4-preview-f"},
		{ID: "gpt-5.6-luna"},
		{ID: "kimi-k3-1"},
	}
	got := publishIDs(applyPublishPolicy(remote, &storedAuth{}))
	want := []string{"hy3", "hy4-preview-f"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published %v, want %v", got, want)
	}
}

// TestApplyPublishPolicyDefaultsToWholeCatalog keeps the historical behaviour:
// without publish_mode the plugin advertises everything upstream serves.
func TestApplyPublishPolicyDefaultsToWholeCatalog(t *testing.T) {
	withPluginConfig(t, "extra_models:\n  - hy4-preview-f\n")
	remote := []upstreamModel{{ID: "hy3"}, {ID: "gpt-5.6-luna"}}
	got := publishIDs(applyPublishPolicy(remote, &storedAuth{}))
	want := []string{"gpt-5.6-luna", "hy3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published %v, want %v", got, want)
	}
}

// TestApplyPublishPolicyAllowOnlyFiltersNeverAdds makes sure publish_allow
// cannot invent an id the catalog does not serve.
func TestApplyPublishPolicyAllowOnlyFiltersNeverAdds(t *testing.T) {
	withPluginConfig(t, "publish_mode: allow\npublish_allow:\n  - not-in-catalog\n")
	remote := []upstreamModel{{ID: "hy3"}}
	got := publishIDs(applyPublishPolicy(remote, &storedAuth{}))
	if len(got) != 0 {
		t.Fatalf("published %v, want nothing", got)
	}
}

// TestExtractSupplementIDsFromBanner pins the real-time completion source: the
// free-trial banner inside the same /v3/config response names the servable
// hy4-preview-f even when data.models omits it (every Global account, and the
// CN account that carries -x instead of -f).
func TestExtractSupplementIDsFromBanner(t *testing.T) {
	raw := json.RawMessage(`{
		"models": [{"id":"hy4-preview"}],
		"productFeaturesConfig": {"ModelTrialBanner": {"banners": [
			{"modelId":"hy4-preview-f","targetModelId":"hy4-preview","trialDays":14}
		]}},
		"modelPromotions": [{"id":"p","modelIds":["deepseek-v4-flash-ioa"]}],
		"modelTiers": [{"id":"tier","modelIds":["glm-5.3"]}]
	}`)
	ids, targets := extractSupplementIDs(raw)
	byID := map[string]bool{}
	for _, id := range ids {
		byID[id] = true
	}
	for _, want := range []string{"hy4-preview-f", "deepseek-v4-flash-ioa", "glm-5.3"} {
		if !byID[want] {
			t.Errorf("supplement %q missing from %v", want, ids)
		}
	}
	if targets["hy4-preview-f"] != "hy4-preview" {
		t.Errorf("banner target = %q, want hy4-preview", targets["hy4-preview-f"])
	}
}

// TestExtractSupplementIDsTolerance keeps a reshuffled envelope from breaking
// discovery: a bare array, an empty object and a {"data": ...} envelope must
// all decode without supplements rather than erroring.
func TestExtractSupplementIDsTolerance(t *testing.T) {
	banner := `"productFeaturesConfig": {"ModelTrialBanner": {"banners": [
		{"modelId":"hy4-preview-f","targetModelId":"hy4-preview"}]}}`
	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{"bare array", `[{"id":"a"}]`, 0},
		{"empty object", `{}`, 0},
		{"null banner", `{"productFeaturesConfig":null}`, 0},
		{"data envelope", `{"data": {` + banner + `}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, _ := extractSupplementIDs(json.RawMessage(tc.raw))
			if len(ids) != tc.want {
				t.Fatalf("got %v, want %d ids", ids, tc.want)
			}
		})
	}
}

// TestBuildSupplementsResolution pins the freshness order: the live
// cross-realm entry wins, the measured table covers what no endpoint
// describes, and the banner target's limits are the last resort before a bare
// id — with the price left blank so an unpriced trial never enters the free
// fallback pool.
func TestBuildSupplementsResolution(t *testing.T) {
	window := &contextWindow{DefaultLength: 200000, SupportedLengths: []int64{200000, 1000000}}
	primary := []upstreamModel{{
		ID: "hy4-preview", Name: "Hy4 preview",
		Credits: "x0.29 credits", MaxInputTokens: 1000000, MaxOutputTokens: 64000,
		SupportsImages: true, ContextWindow: window,
	}}
	cross := []upstreamModel{{
		ID: "hy4-preview-f", Name: "Hy4 preview",
		Credits: "x0.00", MaxInputTokens: 1000000, MaxOutputTokens: 64000,
		SupportsImages: true,
	}}
	ids := []string{"hy4-preview-f", "hy4-preview", "completion-1.0", "default-model", "hy4-preview-new"}
	targets := map[string]string{"hy4-preview-f": "hy4-preview", "hy4-preview-new": "hy4-preview"}
	byID := map[string]upstreamModel{}
	for _, m := range buildSupplements(ids, targets, primary, cross) {
		byID[m.ID] = m
	}
	// Already catalogued, service and alias ids are never supplements.
	for _, skip := range []string{"hy4-preview", "completion-1.0", "default-model"} {
		if _, ok := byID[skip]; ok {
			t.Errorf("%q must not become a supplement", skip)
		}
	}
	// Cross-realm entry wins with its live free price.
	f, ok := byID["hy4-preview-f"]
	if !ok {
		t.Fatal("hy4-preview-f missing")
	}
	if f.Credits != "x0.00" || f.MaxOutputTokens != 64000 || !f.SupportsImages {
		t.Errorf("hy4-preview-f = %+v, want the live cross-realm entry", f)
	}
	// No live source: target's window, but a blank price.
	nw, ok := byID["hy4-preview-new"]
	if !ok {
		t.Fatal("hy4-preview-new missing")
	}
	if nw.Credits != "" {
		t.Errorf("hy4-preview-new Credits = %q, want blank (unknown trial billing)", nw.Credits)
	}
	if nw.ContextWindow == nil || nw.MaxInputTokens != 1000000 || !nw.SupportsImages {
		t.Errorf("hy4-preview-new = %+v, want the target's limits", nw)
	}
}

// TestBuildSupplementsMeasuredFallback pins the last resort before a bare id:
// when the cross-realm catalog does not describe the banner id either, the
// measured table still publishes the verified free-trial numbers instead of
// the generic defaults — so hy4-preview-f stays in the free fallback pool.
func TestBuildSupplementsMeasuredFallback(t *testing.T) {
	primary := []upstreamModel{{ID: "hy4-preview", Name: "Hy4 preview"}}
	got := buildSupplements(
		[]string{"hy4-preview-f"},
		map[string]string{"hy4-preview-f": "hy4-preview"},
		primary, nil, // no cross-realm view: measured table must fire
	)
	if len(got) != 1 {
		t.Fatalf("got %v, want one supplement", got)
	}
	f := got[0]
	if !f.isFree() {
		t.Errorf("hy4-preview-f Credits = %q, want the measured x0.00", f.Credits)
	}
	if f.MaxOutputTokens != 64000 || f.MaxInputTokens != 1000000 {
		t.Errorf("hy4-preview-f = %+v, want the measured limits", f)
	}
}

// TestAssembleModelsSupplementsSurviveAllow is the behaviour the Global
// realm needs: under publish_mode=allow the primary catalog is filtered to
// the allowlist, but the banner-derived trial id is still published, and the
// extra_models fill reads the full pre-publish catalog.
func TestAssembleModelsSupplementsSurviveAllow(t *testing.T) {
	withPluginConfig(t, "publish_mode: allow\npublish_allow:\n  - hy3\n")
	restore := configuredExtraModels()
	t.Cleanup(func() { setConfiguredExtraModelsForTest(restore) })
	setConfiguredExtraModelsForTest(extraIDs("hy4-preview-x"))

	primary := []upstreamModel{
		{ID: "hy3", Name: "Hy3", MaxInputTokens: 192000, MaxOutputTokens: 64000},
		{ID: "gpt-5.6-luna", Name: "GPT", MaxInputTokens: 1000000, MaxOutputTokens: 128000},
	}
	supplements := []upstreamModel{{
		ID: "hy4-preview-f", Name: "Hy4 preview", Credits: "x0.00",
		MaxInputTokens: 1000000, MaxOutputTokens: 64000, SupportsImages: true,
	}}
	models, full := assembleModels(&storedAuth{}, primary, supplements, true)

	got := map[string]pluginapi.ModelInfo{}
	for _, m := range models {
		got[m.ID] = m
	}
	if _, ok := got["hy3"]; !ok {
		t.Error("allowlisted hy3 missing")
	}
	if _, ok := got["hy4-preview-f"]; !ok {
		t.Error("banner supplement hy4-preview-f must survive publish_mode=allow")
	}
	if _, ok := got["gpt-5.6-luna"]; ok {
		t.Error("gpt-5.6-luna must stay filtered under publish_mode=allow")
	}
	if _, ok := got["hy4-preview-x"]; !ok {
		t.Error("extra_models hy4-preview-x missing")
	}
	// The fallback pool keeps everything the account can serve.
	byFull := map[string]bool{}
	for _, m := range full {
		byFull[m.ID] = true
	}
	for _, want := range []string{"hy3", "gpt-5.6-luna", "hy4-preview-f"} {
		if !byFull[want] {
			t.Errorf("full catalog lost %q", want)
		}
	}
	// The plugin always publishes bare ids: the host prepends model_prefix
	// (global/...) itself. A literal "global/hy4-preview-f" here would come
	// out as "global/global/hy4-preview-f" and lose its routing.
	for _, m := range models {
		if strings.Contains(m.ID, "/") {
			t.Errorf("published id %q must stay bare, the host adds the prefix", m.ID)
		}
	}
}

// TestAssemblePrefixGuardWithholdsBareCatalog pins the isolation rule: a
// namespaced instance (model_prefix set) must never publish bare ids. When
// the host does not force prefixes, every bare id below would ALSO go out
// as-is — so a shared id like gpt-5.6-sol would be claimed from its real
// provider (openai). The primary catalog is therefore withheld entirely;
// supplements and extra_models still go out, exactly like publish_mode=allow.
// With the host flag on, the same input publishes everything, and the host
// exposes only the global/* aliases.
func TestAssemblePrefixGuardWithholdsBareCatalog(t *testing.T) {
	withPluginConfig(t, "model_prefix: global\n")
	restore := configuredExtraModels()
	t.Cleanup(func() { setConfiguredExtraModelsForTest(restore) })
	setConfiguredExtraModelsForTest(nil)

	primary := []upstreamModel{
		{ID: "hy3", Name: "Hy3", MaxInputTokens: 192000, MaxOutputTokens: 64000},
		{ID: "gpt-5.6-sol", Name: "GPT", MaxInputTokens: 1000000, MaxOutputTokens: 128000},
	}
	supplements := []upstreamModel{{
		ID: "hy4-preview-f", Name: "Hy4 preview", Credits: "x0.00",
		MaxInputTokens: 1000000, MaxOutputTokens: 64000,
	}}

	idsOf := func(models []pluginapi.ModelInfo) map[string]bool {
		got := map[string]bool{}
		for _, m := range models {
			got[m.ID] = true
		}
		return got
	}

	// Host does not force prefixes: bulk catalog withheld, supplement survives.
	models, full := assembleModels(&storedAuth{}, primary, supplements, false)
	got := idsOf(models)
	if got["hy3"] || got["gpt-5.6-sol"] {
		t.Errorf("primary catalog leaked without host force: %v", got)
	}
	if !got["hy4-preview-f"] {
		t.Error("supplement hy4-preview-f must survive the guard")
	}
	if len(full) != 3 {
		t.Errorf("full catalog = %d models, want 3 (guard trims publish, never capability)", len(full))
	}

	// Host forces prefixes: everything goes out bare internally, the host
	// publishes only the global/* aliases — never the original names.
	models, _ = assembleModels(&storedAuth{}, primary, supplements, true)
	got = idsOf(models)
	for _, want := range []string{"hy3", "gpt-5.6-sol", "hy4-preview-f"} {
		if !got[want] {
			t.Errorf("%q missing when the host forces prefixes", want)
		}
	}
}

// TestAssembleNoPrefixUnaffected keeps the CN behaviour: without a
// model_prefix, bare publication is the operator's explicit choice and the
// host flag changes nothing.
func TestAssembleNoPrefixUnaffected(t *testing.T) {
	withPluginConfig(t, "model_prefix: \"\"\n")
	restore := configuredExtraModels()
	t.Cleanup(func() { setConfiguredExtraModelsForTest(restore) })
	setConfiguredExtraModelsForTest(nil)

	primary := []upstreamModel{{ID: "hy3"}, {ID: "gpt-5.6-sol"}}
	for _, forced := range []bool{false, true} {
		models, _ := assembleModels(&storedAuth{}, primary, nil, forced)
		if len(models) != 2 {
			t.Errorf("forced=%v: published %d models, want 2", forced, len(models))
		}
	}
}

// TestModelCacheRespectsHostFlag pins the cache split: a list published while
// the host forced prefixes must not be served after the flag flips off (it
// would leak bare ids), and vice versa.
func TestModelCacheRespectsHostFlag(t *testing.T) {
	key := "global:cache-flag-test"
	modelCache.mu.Lock()
	delete(modelCache.entries, key)
	modelCache.mu.Unlock()

	forced := []pluginapi.ModelInfo{{ID: "hy3"}, {ID: "gpt-5.6-sol"}}
	modelCacheStore(key, forced, true)
	if got := modelCacheLookup(key, true); len(got) != 2 {
		t.Fatalf("lookup under the same flag missed")
	}
	if got := modelCacheLookup(key, false); got != nil {
		t.Fatalf("lookup under the flipped flag hit with %v, want a miss", got)
	}
	// A rediscovery under the new flag overwrites the entry.
	guarded := []pluginapi.ModelInfo{{ID: "hy4-preview-f"}}
	modelCacheStore(key, guarded, false)
	if got := modelCacheLookup(key, false); len(got) != 1 {
		t.Fatalf("lookup after re-store missed")
	}
	modelCache.mu.Lock()
	delete(modelCache.entries, key)
	modelCache.mu.Unlock()
}
