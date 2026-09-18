package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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

// throttled is the 429 body CodeBuddy returns when a model hits its rate
// limit. The reset timestamp is the only hint at how long the wait is.
var throttled = []byte(`{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-09-18 15:17:07 UTC+8, alternatively, you can switch to the other models to continue using it.","requestId":"x"}`)

func TestMarkCooldownBacksOff(t *testing.T) {
	resetCooldowns(t)
	sa := auth("uid-a")

	if cooldownRemaining(accountIdentity(sa), "hy3") != 0 {
		t.Fatal("fresh credential should not be cooling")
	}
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}}, "hy3", nil)
	remaining := cooldownRemaining(accountIdentity(sa), "hy3")
	if remaining <= 0 {
		t.Fatal("expected the model to be cooling after a 429")
	}
	if remaining > cooldownMax {
		t.Fatalf("wait %v exceeds max %v", remaining, cooldownMax)
	}

	// A later success forgives it.
	clearCooldown(sa, "hy3")
	if got := cooldownRemaining(accountIdentity(sa), "hy3"); got != 0 {
		t.Fatalf("cooldown survived a success: %v", got)
	}
}

func TestCooldownIsPerModel(t *testing.T) {
	resetCooldowns(t)
	sa := auth("uid-a")
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}}, "deepseek-v4.1-flash", nil)

	// Upstream throttles one id, so only that id steps aside. Hiding the rest
	// is what made a throttled model look like an account with no models.
	if cooldownRemaining(accountIdentity(sa), "hy3") != 0 {
		t.Fatal("a 429 on one model must not cool down the others")
	}
	if cooldownRemaining(accountIdentity(sa), "deepseek-v4.1-flash") <= 0 {
		t.Fatal("the throttled model should be cooling")
	}
}

func TestCooldownHonoursRetryAfter(t *testing.T) {
	resetCooldowns(t)
	sa := auth("uid-b")
	h := http.Header{}
	h.Set("Retry-After", "120")
	markCooldown(sa, &http.Response{StatusCode: 429, Header: h}, "hy3", nil)
	// 120s requested; allow a little slack for the clock.
	if got := cooldownRemaining(accountIdentity(sa), "hy3"); got <= 115*time.Second || got > 121*time.Second {
		t.Fatalf("remaining = %v, want ~120s", got)
	}
}

func TestCooldownHonoursResetTimestamp(t *testing.T) {
	resetCooldowns(t)
	sa := auth("uid-b")
	// A reset two hours out beats our own 60s guess, and it is not multiplied
	// by the failure count: upstream said when the window lifts.
	reset := time.Now().Add(2 * time.Hour).In(time.FixedZone("upstream", 8*3600))
	body := []byte(`{"code":6004,"msg":"... reset at ` + reset.Format("2006-01-02 15:04:05") + ` UTC+8 ..."}`)
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}}, "hy3", body)
	if got := cooldownRemaining(accountIdentity(sa), "hy3"); got <= 90*time.Minute || got > 121*time.Minute {
		t.Fatalf("remaining = %v, want ~120m from the reset timestamp", got)
	}
}

func TestCooldownGrowsWithRepeatedFailures(t *testing.T) {
	resetCooldowns(t)
	sa := auth("uid-c")
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}}, "hy3", nil)
	first := cooldownRemaining(accountIdentity(sa), "hy3")
	markCooldown(sa, &http.Response{StatusCode: 429, Header: http.Header{}}, "hy3", nil)
	second := cooldownRemaining(accountIdentity(sa), "hy3")
	if second <= first {
		t.Fatalf("second wait %v should exceed first %v", second, first)
	}
}

func TestSuppressModelOnlyWhenAnotherCredentialIsHealthy(t *testing.T) {
	resetCooldowns(t)
	broken := auth("uid-broken")
	healthy := auth("uid-healthy")

	// A lone credential keeps its models: hiding them would turn a real
	// upstream error into "unknown model".
	noteIdentity(accountIdentity(broken))
	markCooldown(broken, &http.Response{StatusCode: 429, Header: http.Header{}}, "hy3", nil)
	if suppressModel(broken, "hy3") {
		t.Fatal("suppressed although no other credential can serve the model")
	}

	// With a healthy peer available, the cooling one steps aside so requests
	// fail over instead of being retried against a refusing upstream.
	noteIdentity(accountIdentity(healthy))
	if !suppressModel(broken, "hy3") {
		t.Fatal("expected the cooling credential to step aside for a healthy peer")
	}
	if suppressModel(healthy, "hy3") {
		t.Fatal("healthy credential must keep its models")
	}
	// The peer's health says nothing about a different id, and neither does
	// the throttled one's 429.
	if suppressModel(broken, "hy4-preview-f") {
		t.Fatal("a 429 on hy3 must not hide hy4-preview-f")
	}
}

func TestWithoutThrottledModelsKeepsTheRest(t *testing.T) {
	resetCooldowns(t)
	broken := auth("uid-broken")
	healthy := auth("uid-healthy")
	noteIdentity(accountIdentity(broken))
	noteIdentity(accountIdentity(healthy))
	markCooldown(broken, &http.Response{StatusCode: 429, Header: http.Header{}}, "deepseek-v4.1-flash", throttled)

	all := make([]pluginapi.ModelInfo, 0, 3)
	for _, id := range []string{"hy3", "deepseek-v4.1-flash", "hy4-preview-f"} {
		all = append(all, pluginapi.ModelInfo{ID: id})
	}
	got := withoutThrottledModels(broken, all)
	if len(got) != 2 {
		t.Fatalf("kept %d models, want 2", len(got))
	}
	for _, m := range got {
		if m.ID == "deepseek-v4.1-flash" {
			t.Fatal("the throttled model should have been withheld")
		}
	}
	// The cache must not pin the filtered list: the caller filters every time.
	if len(all) != 3 {
		t.Fatalf("filtering mutated the caller's slice: %d", len(all))
	}
}
