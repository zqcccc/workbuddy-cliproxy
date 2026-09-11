package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// pathConfigV3 is the live remote product configuration that every
	// CodeBuddy client (IDE / CLI) pulls on startup. Its data.models array is
	// the authoritative per-account model catalog. CodeBuddy.app ships a local
	// copy of the same schema in product-ide-cn.json, but that copy is stale
	// (still Claude-3.7 / GPT-5 era), which is exactly why we read it remotely.
	pathConfigV3 = "/v3/config"
	// pathConsoleModels is the older console catalog. Kept as a fallback
	// for deployments where /v3/config is not routed or yields nothing usable.
	pathConsoleModels = "/console/enterprises/personal/models"

	modelCacheTTL = 30 * time.Minute

	defaultContextLength int64 = 200000
	defaultMaxCompletion int64 = 8192
)

// upstreamModel mirrors one entry of the CodeBuddy model catalog. Field names
// match the schema served by /v3/config and bundled in product-ide-cn.json.
type upstreamModel struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Vendor            string `json:"vendor"`
	DescriptionZh     string `json:"descriptionZh"`
	DescriptionEn     string `json:"descriptionEn"`
	MaxInputTokens    int64  `json:"maxInputTokens"`
	MaxOutputTokens   int64  `json:"maxOutputTokens"`
	SupportsToolCall  bool   `json:"supportsToolCall"`
	SupportsImages    bool   `json:"supportsImages"`
	SupportsReasoning bool   `json:"supportsReasoning"`
	OnlyReasoning     bool   `json:"onlyReasoning"`
	IsDefault         bool   `json:"isDefault"`
}

// serviceModelPrefixes are internal, non-conversational models that CodeBuddy
// ships inside the same catalog: code completion (completion-*), next-edit
// suggestion (nes-*) and prompt enhancement (enhance-*). They cannot serve
// /v1/chat/completions, so they stay out of the published list.
var serviceModelPrefixes = []string{"completion-", "nes-", "enhance-"}

func isServiceModel(id string) bool {
	for _, prefix := range serviceModelPrefixes {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

// v3ConfigData is the subset of /v3/config data we care about.
type v3ConfigData struct {
	Models json.RawMessage `json:"models"`
}

// -----------------------------------------------------------------------------
// Discovery
// -----------------------------------------------------------------------------

// fetchRemoteModels asks CodeBuddy which models the account may use. It prefers
// /v3/config and falls back to the legacy console catalog when that endpoint is
// not routed or yields nothing usable.
func fetchRemoteModels(sa *storedAuth) ([]upstreamModel, error) {
	headers := func(r *http.Request) { backendHeaders(r, sa) }
	base := baseFor(sa.Region)

	// /v3/config is the current catalog. An anonymous caller gets models:null,
	// so an empty list is treated the same as a failure and falls through.
	primaryErr := fmt.Errorf("v3/config: no model list")
	if data, _, err := doJSON(discoveryHTTPClient(), http.MethodGet, base+pathConfigV3, headers, nil); err == nil {
		if models := extractModels(data); len(models) > 0 {
			return models, nil
		}
	} else {
		primaryErr = fmt.Errorf("v3/config: %w", err)
	}

	// Legacy console catalog, still the most widely routed one.
	data, _, err := doJSON(discoveryHTTPClient(), http.MethodGet, base+pathConsoleModels, headers, nil)
	if err != nil {
		return nil, fmt.Errorf("%v; console/models: %w", primaryErr, err)
	}
	models := extractModels(data)
	if len(models) == 0 {
		return nil, fmt.Errorf("%v; console/models: empty model list", primaryErr)
	}
	return models, nil
}

// discoveryHTTPClient is a short-timeout client so a slow catalog endpoint can
// never stall a host RPC for the shared client's full 120s timeout.
func discoveryHTTPClient() *http.Client {
	discoveryClientOnce.Do(func() {
		discoveryClient = &http.Client{
			Timeout:   15 * time.Second,
			Transport: sharedHTTPClient().Transport,
		}
	})
	return discoveryClient
}

var (
	discoveryClientOnce sync.Once
	discoveryClient     *http.Client
)

// -----------------------------------------------------------------------------
// Tolerant decoding
// -----------------------------------------------------------------------------

// extractModels pulls a model list out of an arbitrary upstream payload. The
// catalog has been served as {models:[...]}, as a bare array, as an array of
// id strings, and as an {id: spec} map, so accept all of them rather than
// breaking when Tencent reshuffles the envelope.
func extractModels(raw json.RawMessage) []upstreamModel {
	if len(raw) == 0 {
		return nil
	}
	// /v3/config wraps the list in data.models.
	var cfg v3ConfigData
	if err := json.Unmarshal(raw, &cfg); err == nil && len(cfg.Models) > 0 {
		if models := decodeModelList(cfg.Models, 0); len(models) > 0 {
			return models
		}
	}
	return decodeModelList(raw, 0)
}

func decodeModelList(raw json.RawMessage, depth int) []upstreamModel {
	if depth > 3 {
		return nil
	}
	trimmed := json.RawMessage{}
	if err := json.Unmarshal(raw, &trimmed); err != nil {
		return nil
	}

	// Bare array: entries may be full objects or plain id strings.
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err == nil {
		var out []upstreamModel
		for _, item := range items {
			var id string
			if json.Unmarshal(item, &id) == nil {
				if id != "" {
					out = append(out, upstreamModel{ID: id, Name: id})
				}
				continue
			}
			var m upstreamModel
			if json.Unmarshal(item, &m) == nil && m.ID != "" {
				if m.Name == "" {
					m.Name = m.ID
				}
				out = append(out, m)
			}
		}
		return out
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	// Wrapper objects: {models:[...]}, {list:[...]}, {data:[...]}. If a wrapper
	// key is present but yields nothing (e.g. {"models":null}, which is what
	// /v3/config returns for an anonymous caller), stop here rather than
	// mistaking the wrapper key for a model id.
	wrapped := false
	for _, key := range []string{"models", "list", "data", "items"} {
		nested, ok := obj[key]
		if !ok {
			continue
		}
		wrapped = true
		if models := decodeModelList(nested, depth+1); len(models) > 0 {
			return models
		}
	}
	if wrapped {
		return nil
	}

	// Otherwise treat it as an {id: spec} map. Map iteration is unordered, so
	// sort to keep the published list stable across calls. Entries must look
	// like model specs: /v3/config carries sibling objects such as "agent",
	// "mcp" and "codebase" that are not models and must not leak into the list.
	var out []upstreamModel
	for id, spec := range obj {
		var fields map[string]json.RawMessage
		if json.Unmarshal(spec, &fields) != nil || !looksLikeModelSpec(fields) {
			continue
		}
		var m upstreamModel
		if json.Unmarshal(spec, &m) == nil && m.ID == "" {
			m.ID = id
		}
		if m.Name == "" {
			m.Name = m.ID
		}
		if m.ID != "" {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// modelSpecKeys are the fields a per-model spec object is expected to carry.
var modelSpecKeys = []string{
	"id", "name", "vendor", "maxInputTokens", "maxOutputTokens",
	"supportsToolCall", "supportsImages", "supportsExtra", "disabledMultimodal",
	"maxAllowedSize", "credits", "isDefault",
}

// looksLikeModelSpec reports whether an object carries at least one known model
// field, so unrelated config sections are not mistaken for model entries.
func looksLikeModelSpec(fields map[string]json.RawMessage) bool {
	for _, key := range modelSpecKeys {
		if _, ok := fields[key]; ok {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Conversion
// -----------------------------------------------------------------------------

func toModelInfos(models []upstreamModel) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if m.ID == "" || isServiceModel(m.ID) {
			continue
		}
		display := m.Name
		if display == "" {
			display = m.ID
		}
		contextLength := m.MaxInputTokens
		if contextLength <= 0 {
			contextLength = defaultContextLength
		}
		maxCompletion := m.MaxOutputTokens
		if maxCompletion <= 0 {
			maxCompletion = defaultMaxCompletion
		}
		info := pluginapi.ModelInfo{
			ID:                         m.ID,
			Object:                     "model",
			OwnedBy:                    providerName,
			DisplayName:                display,
			Name:                       m.ID,
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              contextLength,
			MaxCompletionTokens:        maxCompletion,
			SupportedInputModalities:   []string{"text"},
			SupportedOutputModalities:  []string{"text"},
			UserDefined:                true,
		}
		if m.SupportsImages {
			info.SupportedInputModalities = []string{"text", "image"}
		}
		// Chinese first: the plugin and its users are on the CN deployment.
		info.Description = firstNonEmpty(m.DescriptionZh, m.DescriptionEn)
		out = append(out, info)
	}
	return out
}

// -----------------------------------------------------------------------------
// Cache
// -----------------------------------------------------------------------------

type modelCacheEntry struct {
	models  []pluginapi.ModelInfo
	expires time.Time
}

var modelCache = struct {
	mu      sync.Mutex
	entries map[string]*modelCacheEntry
}{entries: map[string]*modelCacheEntry{}}

// modelCacheKey must be stable per account (not per token), otherwise a refresh
// would invalidate the cache. The realm is part of it because CN and Global
// publish different catalogs.
func modelCacheKey(req pluginapi.AuthModelRequest, sa *storedAuth) string {
	return normalizeRegion(sa.Region) + ":" + accountIdentity(sa)
}

func modelCacheLookup(key string) []pluginapi.ModelInfo {
	modelCache.mu.Lock()
	defer modelCache.mu.Unlock()
	entry, ok := modelCache.entries[key]
	if !ok || time.Now().After(entry.expires) {
		return nil
	}
	return entry.models
}

func modelCacheStore(key string, models []pluginapi.ModelInfo) {
	modelCache.mu.Lock()
	defer modelCache.mu.Unlock()
	modelCache.entries[key] = &modelCacheEntry{models: models, expires: time.Now().Add(modelCacheTTL)}
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

// modelsForAuth returns the account's live catalog. Any failure degrades to the
// bundled fallback list so /v1/models never comes back empty.
func modelsForAuth(req pluginapi.AuthModelRequest, sa *storedAuth) []pluginapi.ModelInfo {
	key := modelCacheKey(req, sa)
	if cached := modelCacheLookup(key); cached != nil {
		return cached
	}
	remote, err := fetchRemoteModels(sa)
	if err != nil || len(remote) == 0 {
		reason := "empty model list"
		if err != nil {
			reason = err.Error()
		}
		hostLog("warn", "workbuddy: model discovery failed, using built-in fallback", map[string]any{
			"uid":   sa.Account.UID,
			"error": reason,
		})
		return fallbackModels()
	}
	models := appendExtraModels(toModelInfos(remote))
	modelCacheStore(key, models)
	hostLog("info", "workbuddy: model discovery succeeded", map[string]any{
		"uid":   sa.Account.UID,
		"count": len(models),
	})
	return models
}

// appendExtraModels adds the ids configured under plugins.configs.<id>.
// extra_models. Upstream catalogs are not always complete: the Global realm
// serves the hy4 family but leaves it out of /v3/config, and the endpoint that
// does list it is browser-session only. Entries already present upstream are
// skipped so a discovered model keeps its real metadata instead of being
// replaced by these defaults.
func appendExtraModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	extra := configuredExtraModels()
	if len(extra) == 0 {
		return models
	}
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		seen[m.ID] = struct{}{}
	}
	out := models
	added := 0
	for _, raw := range extra {
		id := strings.TrimSpace(raw)
		if id == "" || isServiceModel(id) {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, pluginapi.ModelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    providerName,
			DisplayName:                id,
			Name:                       id,
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              defaultContextLength,
			MaxCompletionTokens:        defaultMaxCompletion,
			SupportedInputModalities:   []string{"text"},
			SupportedOutputModalities:  []string{"text"},
			UserDefined:                true,
		})
		added++
	}
	if added > 0 {
		hostLog("info", "workbuddy: added configured extra models", map[string]any{"count": added})
	}
	return out
}

// fallbackModels is the bundled catalog used for model.static (no credentials
// available) and whenever remote discovery fails.
func fallbackModels() []pluginapi.ModelInfo {
	specs := []struct {
		id            string
		name          string
		contextLength int64
		maxCompletion int64
	}{
		{"default-model", "Default", 176000, 24000},
		{"auto-chat", "Auto", 168000, 32000},
		{"glm-5v-turbo", "GLM-5v-Turbo", 200000, 38000},
		{"kimi-k2.5", "Kimi-K2.5", 256000, 32000},
		{"deepseek-v3.2", "DeepSeek-V3.2", 96000, 32000},
		{"gpt-5.5", "GPT-5.5", 1000000, 72000},
		{"gemini-3.5-flash", "Gemini-3.5-Flash", 1000000, 65536},
	}
	models := make([]pluginapi.ModelInfo, 0, len(specs))
	for _, m := range specs {
		models = append(models, pluginapi.ModelInfo{
			ID:                         m.id,
			Object:                     "model",
			OwnedBy:                    providerName,
			DisplayName:                m.name,
			Name:                       m.id,
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              m.contextLength,
			MaxCompletionTokens:        m.maxCompletion,
			SupportedInputModalities:   []string{"text"},
			SupportedOutputModalities:  []string{"text"},
			UserDefined:                true,
		})
	}
	return models
}
