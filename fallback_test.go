package main

import (
	"net/http"
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

func TestNextFallbackModelPrefersFreeAndSkipsCooling(t *testing.T) {
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

	// Nothing left to try: every free id is parked.
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}}, "hy3", nil)
	if _, ok := nextFallbackModel(sa, map[string]struct{}{}); ok {
		t.Fatal("should not fall back to a priced model")
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
