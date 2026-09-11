package main

import (
	"net/http"
	"testing"
	"time"
)

func resetCooldowns(t *testing.T) {
	t.Helper()
	cdMu.Lock()
	cooling = map[string]cooldownState{}
	lastSeen = map[string]time.Time{}
	cdMu.Unlock()
}

func auth(uid string) *storedAuth {
	return &storedAuth{Auth: storedTokens{AccessToken: "tok-" + uid}, Account: storedAccount{UID: uid}}
}

func TestMarkCooldownBacksOff(t *testing.T) {
	resetCooldowns(t)
	sa := auth("uid-a")

	if cooldownRemaining(accountIdentity(sa)) != 0 {
		t.Fatal("fresh credential should not be cooling")
	}
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}})
	remaining := cooldownRemaining(accountIdentity(sa))
	if remaining <= 0 {
		t.Fatal("expected the credential to be cooling after a 429")
	}
	if remaining > cooldownMax {
		t.Fatalf("wait %v exceeds max %v", remaining, cooldownMax)
	}

	// A later success forgives it.
	clearCooldown(sa)
	if got := cooldownRemaining(accountIdentity(sa)); got != 0 {
		t.Fatalf("cooldown survived a success: %v", got)
	}
}

func TestCooldownHonoursRetryAfter(t *testing.T) {
	resetCooldowns(t)
	sa := auth("uid-b")
	h := http.Header{}
	h.Set("Retry-After", "120")
	markCooldown(sa, &http.Response{StatusCode: 429, Header: h})
	// 120s requested; allow a little slack for the clock.
	if got := cooldownRemaining(accountIdentity(sa)); got <= 115*time.Second || got > 121*time.Second {
		t.Fatalf("remaining = %v, want ~120s", got)
	}
}

func TestCooldownGrowsWithRepeatedFailures(t *testing.T) {
	resetCooldowns(t)
	sa := auth("uid-c")
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}})
	first := cooldownRemaining(accountIdentity(sa))
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}})
	second := cooldownRemaining(accountIdentity(sa))
	if second <= first {
		t.Fatalf("second wait %v should exceed first %v", second, first)
	}
}

func TestSuppressModelsOnlyWhenAnotherCredentialIsHealthy(t *testing.T) {
	resetCooldowns(t)
	broken := auth("uid-broken")
	healthy := auth("uid-healthy")

	// A lone credential keeps its models: hiding them would turn a real
	// upstream error into "unknown model".
	noteIdentity(accountIdentity(broken))
	markCooldown(broken, &http.Response{StatusCode: 429, Header: http.Header{}})
	if suppressModels(broken) {
		t.Fatal("suppressed although no other credential can serve the model")
	}

	// With a healthy peer available, the cooling one steps aside so requests
	// fail over instead of being retried against a refusing upstream.
	noteIdentity(accountIdentity(healthy))
	if !suppressModels(broken) {
		t.Fatal("expected the cooling credential to step aside for a healthy peer")
	}
	if suppressModels(healthy) {
		t.Fatal("healthy credential must keep its models")
	}
}
