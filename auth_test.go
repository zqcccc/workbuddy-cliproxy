package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAccountIdentity(t *testing.T) {
	cases := []struct {
		name string
		sa   *storedAuth
		want string
	}{
		{"uid wins", &storedAuth{Account: storedAccount{UID: "abc-123"}}, "abc-123"},
		{"unsafe uid sanitized", &storedAuth{Account: storedAccount{UID: "a/b c@d"}}, "a-b-c-d"},
		{"token fallback", &storedAuth{Auth: storedTokens{AccessToken: "tok-xyz"}}, ""},
		{"nothing at all", &storedAuth{}, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := accountIdentity(tc.sa)
			if tc.want != "" && got != tc.want {
				t.Fatalf("accountIdentity = %q, want %q", got, tc.want)
			}
			if tc.want == "" {
				// Token-derived: 12 hex chars, stable across calls.
				if len(got) != 12 {
					t.Fatalf("hashed identity = %q, want 12 chars", got)
				}
				if again := accountIdentity(tc.sa); again != got {
					t.Fatalf("identity not stable: %q vs %q", got, again)
				}
			}
			if strings.ContainsAny(got, "/\\:") {
				t.Errorf("identity %q is not filesystem safe", got)
			}
		})
	}
}

// TestToAuthDataPerAccount is the regression test for the one-account-only bug:
// FileName drives the persisted auth file, so it must differ per account.
func TestToAuthDataPerAccount(t *testing.T) {
	cn := &storedAuth{
		Auth:    storedTokens{AccessToken: "a", RefreshToken: "r"},
		Account: storedAccount{UID: "uid-one", Nickname: "One"},
	}
	global := &storedAuth{
		Region:  regionGlobal,
		Auth:    storedTokens{AccessToken: "b", RefreshToken: "s"},
		Account: storedAccount{UID: "uid-two"},
	}

	a := toAuthData(cn)
	b := toAuthData(global)

	if a.FileName == b.FileName {
		t.Fatalf("two accounts share FileName %q", a.FileName)
	}
	if a.ID == b.ID {
		t.Fatalf("two accounts share ID %q", a.ID)
	}
	// Asserted against providerName so the suite also passes for the
	// workbuddy-global.so build (-tags global).
	wantA := providerName + "-uid-one.json"
	wantB := providerName + "-uid-two.json"
	if a.FileName != wantA || b.FileName != wantB {
		t.Errorf("file names = %q / %q, want %q / %q", a.FileName, b.FileName, wantA, wantB)
	}
	if a.Provider != providerName || b.Provider != providerName {
		t.Errorf("provider must stay %q", providerName)
	}
	// Stable identity: same account always maps to the same file.
	if again := toAuthData(cn); again.ID != a.ID || again.FileName != a.FileName {
		t.Errorf("identity unstable across calls: %+v vs %+v", again, a)
	}
	// Labels should be distinguishable in the host UI.
	if a.Label == b.Label {
		t.Errorf("labels identical (%q)", a.Label)
	}
	if !strings.Contains(b.Label, "Global") {
		t.Errorf("global label %q should be marked", b.Label)
	}
}

func TestRegionStoredRoundTrip(t *testing.T) {
	raw := []byte(`{"region":"global","auth":{"accessToken":"t","refreshToken":"r","expiresAt":1},"account":{"uid":"u"}}`)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	if normalizeRegion(sa.Region) != regionGlobal {
		t.Fatalf("region = %q, want global", sa.Region)
	}
	// Region must survive the marshal round-trip used for StorageJSON.
	again, err := parseStored(toAuthData(sa).StorageJSON)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if normalizeRegion(again.Region) != regionGlobal {
		t.Fatalf("region lost on round-trip: %q", again.Region)
	}
}

// TestRegionDefaultCN keeps old files (no region field) working.
func TestRegionDefaultsCN(t *testing.T) {
	sa, err := parseStored([]byte(`{"auth":{"accessToken":"t"},"account":{"uid":"u"}}`))
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	if normalizeRegion(sa.Region) != regionCN {
		t.Errorf("missing region should default to cn, got %q", sa.Region)
	}
	if got := baseFor(sa.Region); got != "https://copilot.tencent.com" {
		t.Errorf("baseFor(cn) = %q", got)
	}
	if got := originFor(sa.Region); got != "https://www.codebuddy.cn" {
		t.Errorf("originFor(cn) = %q", got)
	}
}

func TestRegionResolution(t *testing.T) {
	cases := []struct {
		region string
		want   string
	}{
		{"", regionCN},
		{"cn", regionCN},
		{"CN", regionCN},
		{"global", regionGlobal},
		{"GLOBAL", regionGlobal},
		{" Global ", regionGlobal},
		{"nonsense", regionCN},
	}
	for _, tc := range cases {
		if got := normalizeRegion(tc.region); got != tc.want {
			t.Errorf("normalizeRegion(%q) = %q, want %q", tc.region, got, tc.want)
		}
	}
	if got := baseFor(regionGlobal); got != "https://www.workbuddy.ai" {
		t.Errorf("baseFor(global) = %q", got)
	}
	if got := originFor(regionGlobal); got != "https://www.workbuddy.ai" {
		t.Errorf("originFor(global) = %q", got)
	}
}

// setConfiguredRegionForTest restores the package-level region after a test
// mutates it through the register/reconfigure path.
func setConfiguredRegionForTest(region string) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfgRegion = region
}

// TestOtherRegion pins the pairing used when one login click opens both realms.
func TestOtherRegion(t *testing.T) {
	cases := map[string]string{
		regionCN:     regionGlobal,
		regionGlobal: regionCN,
		"":           regionGlobal, // empty normalizes to cn
		"GLOBAL":     regionCN,
	}
	for in, want := range cases {
		if got := otherRegion(in); got != want {
			t.Errorf("otherRegion(%q) = %q, want %q", in, got, want)
		}
	}
	// Round-tripping must always come back to the starting realm.
	for _, start := range []string{regionCN, regionGlobal} {
		if got := otherRegion(otherRegion(start)); got != start {
			t.Errorf("otherRegion twice = %q, want %q", got, start)
		}
	}
}

func TestApplyConfigYAML(t *testing.T) {
	restore := configuredRegion()
	t.Cleanup(func() { setConfiguredRegionForTest(restore) })

	// The host sends config_yaml as a JSON byte slice (base64 on the wire).
	body, err := json.Marshal(map[string]any{"config_yaml": []byte("region: global\n")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	applyConfigYAML(body)
	if got := configuredRegion(); got != regionGlobal {
		t.Fatalf("configuredRegion = %q, want global", got)
	}

	// An empty or malformed block must not disturb the current setting.
	applyConfigYAML([]byte(`{}`))
	if got := configuredRegion(); got != regionGlobal {
		t.Fatalf("empty config changed region to %q", got)
	}
	body, _ = json.Marshal(map[string]any{"config_yaml": []byte("region: cn\n")})
	applyConfigYAML(body)
	if got := configuredRegion(); got != regionCN {
		t.Fatalf("configuredRegion = %q, want cn", got)
	}
}

func TestEnsureSystemFirst(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	// CN is left completely alone so existing traffic stays byte-identical.
	if got := ensureSystemFirst(payload, regionCN); string(got) != string(payload) {
		t.Fatalf("CN payload was rewritten: %s", got)
	}
	if got := ensureSystemFirst(payload, ""); string(got) != string(payload) {
		t.Fatalf("empty region was rewritten: %s", got)
	}

	// Global gets an empty system message prepended.
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	out := ensureSystemFirst(payload, regionGlobal)
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(obj.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(obj.Messages))
	}
	if obj.Messages[0]["role"] != "system" {
		t.Fatalf("first role = %v, want system", obj.Messages[0]["role"])
	}
	if obj.Messages[1]["role"] != "user" || obj.Messages[1]["content"] != "hi" {
		t.Fatalf("user message mangled: %v", obj.Messages[1])
	}

	// Already system-first: untouched.
	withSys := []byte(`{"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)
	if got := ensureSystemFirst(withSys, regionGlobal); string(got) != string(withSys) {
		t.Fatalf("system-first payload was rewritten: %s", got)
	}

	// Garbage and empty message lists pass through unchanged.
	for _, bad := range []string{``, `not json`, `{"messages":[]}`, `{}`} {
		if got := ensureSystemFirst([]byte(bad), regionGlobal); string(got) != bad {
			t.Fatalf("input %q became %q", bad, got)
		}
	}
}

func TestConfigRegionNotClobberedByRegionlessBlock(t *testing.T) {
	restore := configuredRegion()
	t.Cleanup(func() { setConfiguredRegionForTest(restore) })

	// A workbuddy-global.so must stay global even when its config block omits
	// the region key, otherwise it silently degrades to CN.
	setConfiguredRegionForTest(regionGlobal)
	body, err := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\npriority: 100\n")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	applyConfigYAML(body)
	if got := configuredRegion(); got != regionGlobal {
		t.Fatalf("regionless config block changed region to %q, want global", got)
	}
}

func TestProviderNameIsValidPluginID(t *testing.T) {
	// The host derives a plugin's id from its file name and rejects ids that
	// do not match this pattern, which would silently disable the plugin.
	valid := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	if !valid.MatchString(providerName) {
		t.Fatalf("providerName %q is not a valid plugin id", providerName)
	}
	// Credential file names are derived from it, so it must be path-safe too.
	if strings.ContainsAny(providerName, `/\ `) {
		t.Fatalf("providerName %q is not safe in a file name", providerName)
	}
	switch buildRegion {
	case regionCN, regionGlobal:
	default:
		t.Fatalf("buildRegion = %q, want cn or global", buildRegion)
	}
}

func setConfiguredExtraModelsForTest(v []string) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfgExtraModels = v
}

func TestAppendExtraModels(t *testing.T) {
	restore := configuredExtraModels()
	t.Cleanup(func() { setConfiguredExtraModelsForTest(restore) })

	base := []pluginapi.ModelInfo{{ID: "hy3"}, {ID: "gpt-5.5"}}

	// Nothing configured: the discovered list is returned untouched.
	setConfiguredExtraModelsForTest(nil)
	if got := appendExtraModels(base); len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	// Configured ids are appended; ids already discovered are not duplicated.
	setConfiguredExtraModelsForTest([]string{"hy4-preview-f", "hy4-preview-x", "hy3", "  "})
	got := appendExtraModels(base)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (2 existing + 2 new)", len(got))
	}
	ids := map[string]bool{}
	for _, m := range got {
		ids[m.ID] = true
	}
	for _, want := range []string{"hy3", "gpt-5.5", "hy4-preview-f", "hy4-preview-x"} {
		if !ids[want] {
			t.Errorf("missing %q", want)
		}
	}
	// Service models are never advertised, even when configured.
	setConfiguredExtraModelsForTest([]string{"completion-1.0"})
	if got := appendExtraModels(base); len(got) != 2 {
		t.Fatalf("service model was added: %d", len(got))
	}
}
