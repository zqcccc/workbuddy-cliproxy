package main

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func seedCatalog(t *testing.T, sa *storedAuth, models ...upstreamModel) {
	t.Helper()
	resetCooldowns(t)
	key := modelCacheKey(pluginapi.AuthModelRequest{}, sa)
	modelCache.mu.Lock()
	delete(modelCache.entries, key)
	modelCache.mu.Unlock()
	modelCacheRememberRemote(key, models)
}

func TestIsFreeReadsTheCatalogMultiplier(t *testing.T) {
	cases := []struct {
		credits string
		free    bool
	}{
		{"x0.00", true},
		{"x0.00", true},
		{"x0.34 credits", false},
		{"x3.31 credits", false},
		{"", false},
	}
	for _, c := range cases {
		if got := (upstreamModel{Credits: c.credits}).isFree(); got != c.free {
			t.Fatalf("credits %q: isFree = %v, want %v", c.credits, got, c.free)
		}
	}
}

// setExtraModels pins the operator-declared extras for one test. Config lives
// in package state, so a test that leaves it set would change what another
// test's fallback can pick.
func setExtraModels(t *testing.T, ids ...string) {
	t.Helper()
	specs := make([]extraModelSpec, 0, len(ids))
	for _, id := range ids {
		specs = append(specs, extraModelSpec{upstreamModel{ID: id}})
	}
	cfgMu.Lock()
	previous := cfgExtraModels
	cfgExtraModels = specs
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfgExtraModels = previous
		cfgMu.Unlock()
	})
}

func TestNextFallbackModelPrefersFreeAndSkipsCooling(t *testing.T) {
	setExtraModels(t)
	atomic.StoreUint64(&fallbackCursor, 0)
	sa := auth("uid-fallback")
	seedCatalog(t, sa,
		upstreamModel{ID: "gpt-5.6-terra", Credits: "x1.39 credits"},
		upstreamModel{ID: "deepseek-v4.1-flash", Credits: "x0.00"},
		upstreamModel{ID: "hy3", Credits: "x0.00"},
		upstreamModel{ID: "completion-fast", Credits: "x0.00"},
	)

	// The priced model is never chosen while a free one can answer.
	got, ok := nextFallbackModel(sa, map[string]struct{}{})
	if !ok || got != "deepseek-v4.1-flash" {
		t.Fatalf("got %q, want the first free catalog id", got)
	}

	// A throttled id is skipped for the next one, and a service model is
	// never a candidate at all.
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}}, "deepseek-v4.1-flash", nil)
	got, ok = nextFallbackModel(sa, map[string]struct{}{})
	if !ok || got != "hy3" {
		t.Fatalf("got %q, want hy3 after the throttled id", got)
	}

	// Nothing left to try: every free id is parked, and the priced one stays
	// out of it either way.
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}}, "hy3", nil)
	if id, ok := nextFallbackModel(sa, map[string]struct{}{}); ok {
		t.Fatalf("should not fall back to a priced model, got %q", id)
	}

	// A hand-published id is the last resort, after everything the catalog
	// prices as free.
	setExtraModels(t, "hy4-preview-f")
	if id, ok := nextFallbackModel(sa, map[string]struct{}{}); !ok || id != "hy4-preview-f" {
		t.Fatalf("got %q, want the configured extra model", id)
	}
}

func TestFallbackRotatesAcrossCandidates(t *testing.T) {
	setExtraModels(t)
	atomic.StoreUint64(&fallbackCursor, 0)
	sa := auth("uid-rotate")
	seedCatalog(t, sa,
		upstreamModel{ID: "hy3", Credits: "x0.00"},
		upstreamModel{ID: "hy4-preview", Credits: "x0.00"},
		upstreamModel{ID: "gpt-5.6-terra", Credits: "x1.39 credits"},
	)

	// Repeated fallbacks must not all land on the same id: that is what pushed
	// the substitute model over its own burst limit.
	picked := map[string]int{}
	for i := 0; i < 4; i++ {
		id, ok := nextFallbackModel(sa, map[string]struct{}{})
		if !ok {
			t.Fatal("expected a candidate")
		}
		picked[id]++
	}
	if len(picked) != 2 {
		t.Fatalf("fallback kept choosing the same id: %v", picked)
	}
	for id := range picked {
		if id == "gpt-5.6-terra" {
			t.Fatal("rotated onto a priced model")
		}
	}
}

func TestSetModelInBodyRewritesOnlyTheModel(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	got := string(setModelInBody(body, "hy3"))
	want := `{"messages":[{"content":"hi","role":"user"}],"model":"hy3","stream":true}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// A payload that is not JSON is left alone rather than corrupted.
	if string(setModelInBody([]byte("not json"), "hy3")) != "not json" {
		t.Fatal("non-JSON payload should pass through unchanged")
	}
}

// TestNextFallbackModelSkipsRoutingAlias covers upstream's "pick a backend for
// me" entries. Global ships it as default-model, CN as auto; both carry
// isDefault and a blank price. Upstream answers them with a backend of its own
// choosing — measured as glm-5.2 at x0.79 — so a fallback must never name one.
//
// Both are priced x0.00 here on purpose: an alias must be rejected for being
// an alias, not for being expensive. A fix that only filtered on price would
// let these through.
func TestNextFallbackModelSkipsRoutingAlias(t *testing.T) {
	setExtraModels(t)
	atomic.StoreUint64(&fallbackCursor, 0)
	sa := auth("uid-alias")
	seedCatalog(t, sa,
		upstreamModel{ID: "default-model", Name: "Auto", IsDefault: true, Credits: "x0.00"},
		upstreamModel{ID: "auto", Name: "Auto", IsDefault: true, Credits: "x0.00"},
		upstreamModel{ID: "hy3", Credits: "x0.00"},
	)

	// Repeated draws must never surface an alias, however it is priced.
	for range 6 {
		got, ok := nextFallbackModel(sa, map[string]struct{}{})
		if !ok || got != "hy3" {
			t.Fatalf("got %q, want hy3", got)
		}
	}
}

// TestNextFallbackModelSkipsBlankPrice covers the other half: the catalog
// leaves `credits` empty for entries it does not price, and a blank price is
// not a free price. Only an explicit x0.00 may be used as a fallback.
func TestNextFallbackModelSkipsBlankPrice(t *testing.T) {
	setExtraModels(t)
	atomic.StoreUint64(&fallbackCursor, 0)
	sa := auth("uid-blank")
	seedCatalog(t, sa,
		upstreamModel{ID: "some-unpriced", Credits: ""},
		upstreamModel{ID: "hy3", Credits: "x0.00"},
	)

	got, ok := nextFallbackModel(sa, map[string]struct{}{})
	if !ok || got != "hy3" {
		t.Fatalf("got %q, want hy3", got)
	}
}
