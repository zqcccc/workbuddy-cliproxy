package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
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
// The yaml tags let the same shape be written in a plugin config block, so
// extra_models entries can carry real limits instead of taking the defaults.
type upstreamModel struct {
	ID                string         `json:"id" yaml:"id"`
	Name              string         `json:"name" yaml:"name"`
	Vendor            string         `json:"vendor" yaml:"vendor"`
	DescriptionZh     string         `json:"descriptionZh" yaml:"descriptionZh"`
	DescriptionEn     string         `json:"descriptionEn" yaml:"descriptionEn"`
	ContextWindow     *contextWindow `json:"contextWindow" yaml:"contextWindow"`
	MaxAllowedSize    int64          `json:"maxAllowedSize" yaml:"maxAllowedSize"`
	MaxInputTokens    int64          `json:"maxInputTokens" yaml:"maxInputTokens"`
	MaxOutputTokens   int64          `json:"maxOutputTokens" yaml:"maxOutputTokens"`
	SupportsToolCall  bool           `json:"supportsToolCall" yaml:"supportsToolCall"`
	SupportsImages    bool           `json:"supportsImages" yaml:"supportsImages"`
	SupportsReasoning bool           `json:"supportsReasoning" yaml:"supportsReasoning"`
	OnlyReasoning     bool           `json:"onlyReasoning" yaml:"onlyReasoning"`
	IsDefault         bool           `json:"isDefault" yaml:"isDefault"`
	// Credits is the billing multiplier the catalog publishes, rendered as
	// "x0.00", "x3.31 credits" or "x0.00" for a free model. It is what tells
	// a free fallback apart from one that would bill the account.
	Credits string `json:"credits" yaml:"credits"`
}

// isFree reports whether the catalog bills nothing for this model. The
// multiplier is what /v3/config publishes per id: "x0.00" costs nothing,
// anything else is a multiple of a credit.
func (m upstreamModel) isFree() bool {
	token := strings.TrimSpace(m.Credits)
	if token == "" {
		return false
	}
	token = strings.TrimPrefix(token, "x")
	token = strings.TrimPrefix(token, "X")
	if i := strings.IndexByte(token, ' '); i >= 0 {
		token = token[:i]
	}
	value, err := strconv.ParseFloat(token, 64)
	if err != nil {
		return false
	}
	return value == 0
}

// contextWindow is the newer selectable context-budget block the catalog
// carries alongside maxInputTokens, e.g.
//
//	"contextWindow": {"defaultLength": 300000, "supportedLengths": [300000, 1000000]}
//
// Some models are sold with a default window smaller than the hard input cap
// (hy4-preview: default 200000, maxInputTokens 1000000). The CodeBuddy CLI
// computes its compaction budget with resolveEffectiveContextBudget, which
// prefers defaultLength whenever it is one of the supported lengths, and only
// falls back to maxInputTokens when there is no selectable window. Advertising
// the hard cap instead makes downstream clients (Claude Code, Cline, ...) pack
// prompts the account cannot actually serve, so mirror the CLI's resolution.
type contextWindow struct {
	DefaultLength    int64   `json:"defaultLength" yaml:"defaultLength"`
	SupportedLengths []int64 `json:"supportedLengths" yaml:"supportedLengths"`
}

// extraModelSpec is one entry of the plugins.configs.<id>.extra_models list.
// It decodes from either a bare id string or an object shaped like an upstream
// catalog entry, so an operator can publish a model the catalog omits with its
// real limits attached:
//
//	extra_models:
//	  - hy4-preview-f
//	  - id: hy4-preview-x
//	    maxInputTokens: 1000000
//	    maxOutputTokens: 64000
//	    contextWindow: {defaultLength: 200000, supportedLengths: [200000, 1000000]}
type extraModelSpec struct {
	upstreamModel
}

// UnmarshalYAML accepts both spellings. A scalar is an id with no metadata and
// keeps the historical behaviour (defaults for context and output length); a
// mapping is decoded as a catalog entry, so it flows through the same
// effectiveContextLength resolution as a discovered model.
func (s *extraModelSpec) UnmarshalYAML(value *yaml.Node) error {
	var id string
	if err := value.Decode(&id); err == nil {
		trimmed := strings.TrimSpace(id)
		if trimmed != "" {
			// Name is deliberately left empty: it is not operator input, so the
			// live catalog may still supply the real display name. The id is used
			// as the fallback downstream when nothing else provides one.
			s.upstreamModel = upstreamModel{ID: trimmed}
		}
		return nil
	}
	var m upstreamModel
	if err := value.Decode(&m); err != nil {
		return err
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	s.upstreamModel = m
	return nil
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

// fetchRemoteModels asks CodeBuddy which models the account may use. Both
// catalogs are fetched and merged, because each one lists models the other
// omits: /v3/config has hy4-preview-f but not hy4-preview-x, while the console
// catalog has hy4-preview-x, auto and the selectable contextWindow block but
// not hy4-preview-f. Neither is a superset, so a single source silently drops
// models the account can serve.
func fetchRemoteModels(sa *storedAuth) ([]upstreamModel, error) {
	headers := func(r *http.Request) { backendHeaders(r, sa) }
	base := baseFor(sa.Region)
	client := discoveryHTTPClient()

	v3, v3Err := fetchCatalog(client, http.MethodGet, base+pathConfigV3, headers)
	console, consoleErr := fetchCatalog(client, http.MethodGet, base+pathConsoleModels, headers)

	if len(v3) == 0 && len(console) == 0 {
		// Nothing usable at all. Report why, so the log says what to fix.
		if v3Err != nil && consoleErr != nil {
			return nil, fmt.Errorf("v3/config: %w; console/models: %w", v3Err, consoleErr)
		}
		if v3Err != nil {
			return nil, fmt.Errorf("v3/config: %w; console/models: empty model list", v3Err)
		}
		if consoleErr != nil {
			return nil, fmt.Errorf("v3/config: empty model list; console/models: %w", consoleErr)
		}
		return nil, fmt.Errorf("v3/config and console/models both returned an empty model list")
	}

	return mergeCatalogs(console, v3), nil
}

// fetchCatalog fetches one catalog endpoint and extracts its model list. A
// failure is returned alongside a nil list so the caller can merge whatever
// the other endpoint produced.
func fetchCatalog(client *http.Client, method, url string, headers func(*http.Request)) ([]upstreamModel, error) {
	data, _, err := doJSON(client, method, url, headers, nil)
	if err != nil {
		return nil, err
	}
	models := extractModels(data)
	if len(models) == 0 {
		return nil, fmt.Errorf("empty model list")
	}
	return models, nil
}

// mergeCatalogs combines two catalogs, preferring the richer entry when both
// describe the same id. The console catalog wins because it is the one that
// carries contextWindow; /v3/config still contributes ids it lists alone.
func mergeCatalogs(console, v3 []upstreamModel) []upstreamModel {
	byID := make(map[string]upstreamModel, len(console)+len(v3))
	order := make([]string, 0, len(console)+len(v3))
	for _, list := range [][]upstreamModel{console, v3} {
		for _, m := range list {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				continue
			}
			existing, seen := byID[id]
			if !seen {
				order = append(order, id)
				byID[id] = m
				continue
			}
			if !seen || richerModel(m, existing) {
				byID[id] = m
			}
		}
	}
	out := make([]upstreamModel, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// richerModel reports whether candidate carries more usable metadata than
// current. More fields means a better entry: an entry with a contextWindow or
// a non-zero input cap beats a bare id, since a bare id would fall back to the
// built-in defaults.
func richerModel(candidate, current upstreamModel) bool {
	return modelScore(candidate) > modelScore(current)
}

func modelScore(m upstreamModel) int {
	score := 0
	if m.ContextWindow != nil {
		score += 8
	}
	if m.MaxInputTokens > 0 {
		score += 4
	}
	if m.MaxOutputTokens > 0 {
		score += 2
	}
	if strings.TrimSpace(m.Name) != "" && strings.TrimSpace(m.Name) != m.ID {
		score++
	}
	return score
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

// effectiveContextLength resolves the context window this account can actually
// use for a model. It mirrors resolveEffectiveContextBudget in the CodeBuddy
// CLI: a selectable contextWindow wins over the raw input cap, because the
// platform bills and enforces the default length unless a session explicitly
// selects a bigger budget. It never reports more than maxInputTokens, so a
// client that trusts the advertised window cannot overrun the hard limit.
func (m upstreamModel) effectiveContextLength() int64 {
	if m.ContextWindow == nil {
		return m.MaxInputTokens
	}
	supported := normalizeContextLengths(m.ContextWindow.SupportedLengths, m.MaxInputTokens)
	if d := m.ContextWindow.DefaultLength; d > 0 && len(supported) >= 2 {
		for _, candidate := range supported {
			if candidate == d {
				return d
			}
		}
	}
	if len(supported) >= 2 {
		return supported[0]
	}
	return m.MaxInputTokens
}

// normalizeContextLengths keeps the positive, safe lengths the catalog offers,
// sorted ascending and capped at the hard input limit. Entries above
// maxInputTokens are not selectable, and duplicates would otherwise let one
// bogus value win the default match twice. A missing maxInputTokens means the
// catalog set no cap, so nothing is filtered out.
func normalizeContextLengths(lengths []int64, maxInputTokens int64) []int64 {
	out := make([]int64, 0, len(lengths))
	for _, length := range lengths {
		if length <= 0 {
			continue
		}
		if maxInputTokens > 0 && length > maxInputTokens {
			continue
		}
		out = append(out, length)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	// Drop duplicates so a repeated length cannot make a single real option
	// look like a selectable budget.
	kept := out[:0]
	for _, length := range out {
		if len(kept) == 0 || kept[len(kept)-1] != length {
			kept = append(kept, length)
		}
	}
	return kept
}

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
		contextLength := m.effectiveContextLength()
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
	// remoteIDs is the id set of the catalog upstream actually served, before
	// extra_models added anything. It is what tells a real model apart from an
	// id we publish on our own: upstream answers unknown ids with a silent
	// fallback to its default backend instead of an error.
	remoteIDs map[string]struct{}
	// remote is the catalog itself, kept in upstream order. The request path
	// reads it to find another model when the requested one is throttled.
	remote []upstreamModel
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

// modelCacheRememberRemote attaches the raw catalog to the cached entry — the
// full list upstream served, before the publish policy trimmed it — so a
// request can later be checked against what upstream really serves and a
// throttled model can fall back to any id the account can use. It is a
// separate call from modelCacheStore to keep that signature untouched; the
// catalog lives as long as the models it produced.
func modelCacheRememberRemote(key string, remote []upstreamModel) {
	ids := make(map[string]struct{}, len(remote))
	for _, m := range remote {
		if id := strings.TrimSpace(m.ID); id != "" {
			ids[id] = struct{}{}
		}
	}
	modelCache.mu.Lock()
	defer modelCache.mu.Unlock()
	entry, ok := modelCache.entries[key]
	if !ok || time.Now().After(entry.expires) {
		entry = &modelCacheEntry{expires: time.Now().Add(modelCacheTTL)}
		modelCache.entries[key] = entry
	}
	entry.remoteIDs = ids
	entry.remote = remote
}

// cachedCatalog returns the catalog upstream served for this account, in
// upstream order. It is empty until discovery has run once.
func cachedCatalog(sa *storedAuth) []upstreamModel {
	key := modelCacheKey(pluginapi.AuthModelRequest{}, sa)
	modelCache.mu.Lock()
	defer modelCache.mu.Unlock()
	entry := modelCache.entries[key]
	if entry == nil || time.Now().After(entry.expires) {
		return nil
	}
	return entry.remote
}

// catalogKnowsModel reports whether id is in the account's upstream catalog.
// The second result is false when nothing usable is cached yet, so callers can
// tell "not cached" apart from "not a real model" and stay silent in the first
// case instead of warning on every cold start.
func catalogKnowsModel(sa *storedAuth, id string) (known, cached bool) {
	key := modelCacheKey(pluginapi.AuthModelRequest{}, sa)
	modelCache.mu.Lock()
	entry := modelCache.entries[key]
	modelCache.mu.Unlock()
	if entry == nil || entry.remoteIDs == nil || time.Now().After(entry.expires) {
		return false, false
	}
	_, ok := entry.remoteIDs[id]
	return ok, true
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

// modelsForAuth returns the account's live catalog. Any failure degrades to the
// bundled fallback list so /v1/models never comes back empty.
func modelsForAuth(req pluginapi.AuthModelRequest, sa *storedAuth) []pluginapi.ModelInfo {
	key := modelCacheKey(req, sa)
	noteIdentity(accountIdentity(sa))

	models := modelCacheLookup(key)
	if models == nil {
		models = discoverModels(sa, key)
	}
	if len(models) == 0 {
		return nil
	}
	// Applied after the cache, never inside it: a throttled id has to
	// reappear the moment its backoff expires, and a cached list would pin
	// the suppressed state for the whole TTL.
	return withoutThrottledModels(sa, models)
}

// discoverModels fetches the catalog, applies the publish policy and the
// configured extra_models, and caches the result.
func discoverModels(sa *storedAuth, key string) []pluginapi.ModelInfo {
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
		// The fallback list is shared ids (gpt-5.5, gemini-3.5-flash, ...),
		// exactly the ones publish_mode=allow exists to give up. Advertising
		// them blind would claim traffic that belongs to another provider, so
		// an allow-list instance stays silent until discovery recovers.
		if mode, _ := configuredPublish(); mode == "allow" {
			return nil
		}
		return fallbackModels()
	}
	// The publish policy decides what we advertise. It must not limit what a
	// fallback may use: an id kept out of the advertised list is still one the
	// account can serve, and it is exactly what keeps a request alive when the
	// two or three published ids are the ones being throttled.
	full := remote
	remote = applyPublishPolicy(remote, sa)
	models := appendExtraModels(toModelInfos(remote), remote)
	modelCacheStore(key, models)
	modelCacheRememberRemote(key, full)
	hostLog("info", "workbuddy: model discovery succeeded", map[string]any{
		"uid":   sa.Account.UID,
		"count": len(models),
	})
	return models
}

// withoutThrottledModels drops the ids upstream throttled on this credential
// so the host serves them from another credential instead. Upstream rate
// limits are per model: a 429 on one id says nothing about the rest, and
// hiding the whole catalog here is what made a single throttled model look
// like an account with no models at all.
func withoutThrottledModels(sa *storedAuth, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	kept := make([]pluginapi.ModelInfo, 0, len(models))
	var dropped []string
	for _, m := range models {
		if suppressModel(sa, m.ID) {
			dropped = append(dropped, m.ID)
			continue
		}
		kept = append(kept, m)
	}
	if len(dropped) == 0 {
		return models
	}
	hostLog("info", "workbuddy: withholding throttled models while the credential cools down", map[string]any{
		"uid":    sa.Account.UID,
		"models": dropped,
	})
	return kept
}

// applyPublishPolicy drops catalog ids this instance must not claim.
//
// The default mode publishes the whole catalog, which is right when this
// realm is the only source for those ids. It is wrong the moment another
// provider serves the same id: with force-model-prefix unset the host matches
// a bare request id against "<prefix>/<id>" as well, so publishing
// "global/gpt-5.6-luna" also captures plain "gpt-5.6-luna" and sends that
// traffic to CodeBuddy, where the account cannot serve it and the upstream
// answers with an empty stream after billing the request.
//
// "allow" keeps only extra_models plus publish_allow, so an operator can list
// exactly the models only this realm provides (hy4-preview-f, hy3, ...) and
// leave every shared id to its real provider.
func applyPublishPolicy(remote []upstreamModel, sa *storedAuth) []upstreamModel {
	mode, allow := configuredPublish()
	if mode != "allow" {
		return remote
	}
	keep := make(map[string]struct{}, len(allow)+8)
	for _, spec := range configuredExtraModels() {
		if id := strings.TrimSpace(spec.upstreamModel.ID); id != "" {
			keep[id] = struct{}{}
		}
	}
	for _, id := range allow {
		if id = strings.TrimSpace(id); id != "" {
			keep[id] = struct{}{}
		}
	}
	out := make([]upstreamModel, 0, len(keep))
	for _, m := range remote {
		if _, ok := keep[strings.TrimSpace(m.ID)]; ok {
			out = append(out, m)
		}
	}
	publishLogOnce.Do(func() {
		hostLog("info", "workbuddy: publish_mode=allow, restricting advertised models", map[string]any{
			"uid":      sa.Account.UID,
			"region":   normalizeRegion(sa.Region),
			"kept":     len(out),
			"filtered": len(remote) - len(out),
		})
	})
	return out
}

var publishLogOnce sync.Once

// appendExtraModels adds the entries configured under plugins.configs.<id>.
// extra_models. Upstream catalogs are not always complete: the Global realm
// serves the hy4 family but leaves it out of /v3/config, and the endpoint that
// does list it (console/models) answers 500 there with a Bearer token.
// Entries already present in the discovered catalog are skipped so they keep
// their real metadata.
//
// Each entry is either a bare id or an object carrying catalog fields. Any
// field the entry leaves out is filled from remote, the catalog this account
// just fetched, so a configured model tracks the live upstream numbers instead
// of the built-in defaults. A bare id that upstream has since started
// advertising therefore picks up its real limits on the next refresh; the
// defaults only apply to ids no source describes at all.
func appendExtraModels(models []pluginapi.ModelInfo, remote []upstreamModel) []pluginapi.ModelInfo {
	extra := configuredExtraModels()
	if len(extra) == 0 {
		return models
	}
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		seen[m.ID] = struct{}{}
	}
	byID := make(map[string]upstreamModel, len(remote))
	for _, m := range remote {
		byID[strings.TrimSpace(m.ID)] = m
	}

	var pending []upstreamModel
	var unresolved []string
	for _, spec := range extra {
		m := spec.upstreamModel
		id := strings.TrimSpace(m.ID)
		if id == "" || isServiceModel(id) {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		m.ID = id
		// fillFromCatalog only fills fields the config left empty, so an
		// explicit value in the plugin config still wins over both sources.
		switch live, ok := byID[id]; {
		case ok:
			// Preferred source: what this account's catalog just returned.
			m = fillFromCatalog(m, live)
		default:
			// No reachable endpoint described it. Fall back to the numbers
			// measured off the real API rather than the generic defaults.
			if measured, ok := measuredModels[id]; ok {
				m = fillFromCatalog(m, measured)
			} else {
				unresolved = append(unresolved, id)
			}
		}
		if m.Name == "" {
			m.Name = id
		}
		pending = append(pending, m)
	}
	if len(pending) == 0 {
		return models
	}
	added := toModelInfos(pending)
	fields := map[string]any{"count": len(added)}
	if len(unresolved) > 0 {
		// No source described these, so they take the built-in defaults.
		fields["defaultsFor"] = unresolved
	}
	hostLog("info", "workbuddy: added configured extra models", fields)
	return append(models, added...)
}

// fillFromCatalog copies any field the configured entry left empty from the
// live catalog entry for the same id. Explicit config still wins: an operator
// who writes maxInputTokens overrides the upstream number rather than being
// ignored. Name is only taken when the entry has none, so a configured display
// name survives.
func fillFromCatalog(entry, live upstreamModel) upstreamModel {
	if entry.Name == "" {
		entry.Name = live.Name
	}
	if entry.ContextWindow == nil {
		entry.ContextWindow = live.ContextWindow
	}
	if entry.MaxInputTokens <= 0 {
		entry.MaxInputTokens = live.MaxInputTokens
	}
	if entry.MaxOutputTokens <= 0 {
		entry.MaxOutputTokens = live.MaxOutputTokens
	}
	if entry.DescriptionZh == "" {
		entry.DescriptionZh = live.DescriptionZh
	}
	if entry.DescriptionEn == "" {
		entry.DescriptionEn = live.DescriptionEn
	}
	entry.SupportsImages = entry.SupportsImages || live.SupportsImages
	entry.SupportsReasoning = entry.SupportsReasoning || live.SupportsReasoning
	entry.SupportsToolCall = entry.SupportsToolCall || live.SupportsToolCall
	return entry
}

// measuredModels records limits read off the real catalog endpoints, so a
// model no reachable endpoint describes still gets its true numbers instead
// of the generic defaults. Every value here was observed on a live response;
// the source is recorded next to it because these can drift.
//
// The hy4 family is why this exists:
//   - Global /v3/config omits all three ids (verified: 35 models, only hy3),
//     and the console catalog that does list them needs a browser cookie the
//     plugin does not hold, so at runtime Global only sees /v3/config.
//   - CN /v3/config does carry them, but which ids appear varies per account
//     (one account showed -f, another -x), so no single account sees all.
//
// hy4-preview also sells a selectable budget: the account can use 1M, but the
// platform bills and enforces 200000 unless a session picks the larger window,
// so 200000 is what gets advertised, matching the CodeBuddy CLI.
var measuredModels = map[string]upstreamModel{
	"hy4-preview": {
		ID: "hy4-preview", Name: "Hy4 preview",
		MaxInputTokens:    1000000,
		MaxOutputTokens:   64000,
		SupportsImages:    true,
		SupportsReasoning: true,
		ContextWindow:     &contextWindow{DefaultLength: 200000, SupportedLengths: []int64{200000, 1000000}},
	}, // source: Global console catalog, 200 via browser cookie, 2026-09-12
	"hy4-preview-f": {
		ID: "hy4-preview-f", Name: "Hy4 preview",
		MaxInputTokens:    1000000,
		MaxOutputTokens:   64000,
		SupportsImages:    true,
		SupportsReasoning: true,
	}, // source: CN /v3/config, account 98e520f0, 2026-09-12
	"hy4-preview-x": {
		ID: "hy4-preview-x", Name: "Hy4 preview",
		MaxInputTokens:    1000000,
		MaxOutputTokens:   64000,
		SupportsImages:    true,
		SupportsReasoning: true,
	}, // source: CN /v3/config, account 0fd66171, 2026-09-12
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
