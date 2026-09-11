package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// realResourcePayload is a verbatim (trimmed) /v2/billing/meter/get-user-resource
// response. Note CycleCapacityRemainPrecise arriving as a *string* while
// CycleCapacitySizePrecise arrives as a number: the endpoint mixes both.
const realResourcePayload = `{
  "code": 0,
  "msg": "OK",
  "data": {
    "Response": {
      "Data": {
        "TotalCount": 2,
        "TotalDosage": 1640,
        "Accounts": [
          {
            "PackageName": "CodeBuddy个人体验版",
            "PackageCode": "TCACA_code_008_cfWoLwvjU4",
            "CycleCapacitySizePrecise": "500",
            "CycleCapacityRemainPrecise": "500",
            "CycleEndTime": "2026-09-30 23:59:59",
            "DeductionEndTime": "2047-09-09 09:00:00"
          },
          {
            "PackageName": "CodeBuddy个人版国内运营裂变包",
            "PackageCode": "TCACA_code_007_nzdH5h4Nl0",
            "CycleCapacitySizePrecise": 1500,
            "CycleCapacityRemainPrecise": 840.36000015,
            "CycleEndTime": "2026-09-16 22:26:56"
          }
        ]
      }
    }
  }
}`

func testStoredAuth() *storedAuth {
	return &storedAuth{
		Region: regionCN,
		Auth: storedTokens{
			AccessToken:  "token-abc",
			RefreshToken: "refresh-abc",
			Domain:       "www.codebuddy.cn",
		},
		Account: storedAccount{
			UID:      "98e520f0-17bc-4370-a161-1b3885812d84",
			Nickname: "知了十八",
		},
	}
}

// newQuotaServer answers both billing paths and records the last request so a
// test can assert the mandatory UA header and the POST body.
func newQuotaServer(t *testing.T, resourceStatus int) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var lastReq http.Request
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the resource call is recorded: the plan-type call that follows
		// would otherwise overwrite it with an empty body.
		if r.URL.Path == pathUserResource {
			lastReq = *r
			lastBody, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathUserResource:
			if resourceStatus != http.StatusOK {
				w.WriteHeader(resourceStatus)
				_, _ = w.Write([]byte(`{"code":10085,"msg":"请求不合法"}`))
				return
			}
			_, _ = w.Write([]byte(realResourcePayload))
		case pathPaymentType:
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"paymentType":"free"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &lastReq, &lastBody
}

func TestFetchQuotaParsesPackages(t *testing.T) {
	srv, lastReq, lastBody := newQuotaServer(t, http.StatusOK)
	got := fetchQuotaFrom(srv.URL, testStoredAuth())
	if got.Error != "" {
		t.Fatalf("unexpected error: %s", got.Error)
	}
	if got.Left != 1640 {
		t.Errorf("left = %v, want 1640", got.Left)
	}
	if len(got.Packages) != 2 {
		t.Fatalf("packages = %d, want 2", len(got.Packages))
	}
	// Packages come back sorted by remaining credits, largest first.
	if got.Packages[0].Left < 840.3 || got.Packages[0].Left > 840.4 || got.Packages[0].Total != 1500 {
		t.Errorf("first package = %v/%v, want ~840.36/1500", got.Packages[0].Left, got.Packages[0].Total)
	}
	if got.Packages[1].Left != 500 || got.Packages[1].Total != 500 {
		t.Errorf("second package = %v/%v, want 500/500", got.Packages[1].Left, got.Packages[1].Total)
	}
	if got.Packages[1].CycleEnd != "2026-09-30 23:59:59" {
		t.Errorf("cycle end = %q", got.Packages[1].CycleEnd)
	}
	if got.Plan != "free" {
		t.Errorf("plan = %q, want free", got.Plan)
	}
	if got.Label != "知了十八" || got.Region != regionCN {
		t.Errorf("label/region = %q/%q", got.Label, got.Region)
	}
	// The gateway rejects the call without a CLI User-Agent and without a body.
	if ua := lastReq.Header.Get("User-Agent"); !strings.Contains(ua, "CodeBuddy/") {
		t.Errorf("User-Agent = %q", ua)
	}
	if lastReq.Method != http.MethodPost {
		t.Errorf("method = %s, want POST (the route is method-specific)", lastReq.Method)
	}
	if !strings.Contains(string(*lastBody), quotaProductCode) {
		t.Errorf("body = %s, want ProductCode", *lastBody)
	}
}

func TestFetchQuotaExpiredToken(t *testing.T) {
	srv, _, _ := newQuotaServer(t, http.StatusUnauthorized)
	got := fetchQuotaFrom(srv.URL, testStoredAuth())
	if got.Error == "" {
		t.Fatal("expected an error for an expired token")
	}
	if !strings.Contains(got.Error, "重新登录") {
		t.Errorf("error = %q, want a re-login hint", got.Error)
	}
	// A failed balance lookup must never leave a stale balance behind.
	if got.Left != 0 || len(got.Packages) != 0 {
		t.Errorf("left/packages = %v/%d, want zero", got.Left, len(got.Packages))
	}
}

// TestFetchQuotaToleratesMixedTimestampTypes covers a real response shape:
// DeductionEndTime arriving as a millisecond epoch while CycleEndTime stays a
// formatted string. Decoding it into a string field failed the whole account.
func TestFetchQuotaToleratesMixedTimestampTypes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == pathUserResource {
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalDosage":"500","Accounts":[
			 {"PackageName":"p","PackageCode":"c","CycleCapacitySizePrecise":500,
			  "CycleCapacityRemainPrecise":"40","CycleEndTime":"2026-09-30 23:59:59",
			  "DeductionEndTime":2047300014000},
			 {"PackageName":"q","CycleCapacitySizePrecise":100,
			  "CycleCapacityRemainPrecise":100,"DeductionEndTime":1791680409000}]}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"paymentType":"free"}}`))
	}))
	defer srv.Close()

	got := fetchQuotaFrom(srv.URL, testStoredAuth())
	if got.Error != "" {
		t.Fatalf("unexpected error: %s", got.Error)
	}
	if got.Left != 500 {
		t.Errorf("left = %v, want 500 (TotalDosage arrived as a string)", got.Left)
	}
	if len(got.Packages) != 2 {
		t.Fatalf("packages = %d, want 2", len(got.Packages))
	}
	// A formatted CycleEndTime wins over the epoch.
	if got.Packages[1].CycleEnd != "2026-09-30 23:59:59" {
		t.Errorf("cycle end = %q", got.Packages[1].CycleEnd)
	}
	// The epoch-only row is rendered as a local timestamp, not dropped and not
	// shown as a raw number.
	if got.Packages[0].CycleEnd == "" || got.Packages[0].CycleEnd == "1791680409000" {
		t.Errorf("epoch cycle end = %q, want a formatted time", got.Packages[0].CycleEnd)
	}
	if !strings.Contains(got.Packages[0].CycleEnd, "2026-10-11") {
		t.Errorf("epoch cycle end = %q, want the 2026-10-11 date", got.Packages[0].CycleEnd)
	}
}

func TestAppendBalanceToLabel(t *testing.T) {
	base := "WorkBuddy (知了十八)"
	cases := []struct {
		name string
		acc  quotaAccount
		want string
	}{
		{"balance", quotaAccount{Left: 1640}, base + " · 剩 1640 credits"},
		{"fractional", quotaAccount{Left: 840.36}, base + " · 剩 840.36 credits"},
		{"zero balance still shown", quotaAccount{Left: 0}, base + " · 剩 0 credits"},
		// A failed lookup must leave the label untouched rather than printing
		// a misleading "0".
		{"lookup failed", quotaAccount{Error: "登录已过期"}, base},
	}
	for _, c := range cases {
		if got := appendBalance(base, c.acc); got != c.want {
			t.Errorf("%s: appendBalance = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBaseAuthLabel(t *testing.T) {
	sa := testStoredAuth()
	if got, want := baseAuthLabel(sa), "WorkBuddy (知了十八)"; got != want {
		t.Errorf("cn label = %q, want %q", got, want)
	}
	sa.Region = regionGlobal
	if got, want := baseAuthLabel(sa), "WorkBuddy (知了十八) · Global"; got != want {
		t.Errorf("global label = %q, want %q", got, want)
	}
	// The balance suffix is rebuilt from the base label every time, so it can
	// never accumulate across refreshes.
	if got := appendBalance(baseAuthLabel(sa), quotaAccount{Left: 350}); got != "WorkBuddy (知了十八) · Global · 剩 350 credits" {
		t.Errorf("global label with balance = %q", got)
	}
}

func TestCachedBalanceReusesUpstreamWithinTTL(t *testing.T) {
	// The host parses the same credential several times in one reload; only the
	// first parse may reach upstream.
	calls := 0
	fetch := func(*storedAuth) quotaAccount {
		calls++
		return quotaAccount{Left: float64(100 + calls)}
	}
	sa := testStoredAuth()
	first := cachedBalance(sa, fetch)
	second := cachedBalance(sa, fetch)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if first.Left != 101 || second.Left != 101 {
		t.Errorf("balances = %v / %v, want the cached 101 twice", first.Left, second.Left)
	}
	// Expiring the entry is what makes a later host refresh pick up a new
	// balance instead of showing a stale one forever.
	quotaMu.Lock()
	quotaBalanceCache[sa.Account.UID] = quotaBalanceEntry{
		account: first, at: time.Now().Add(-quotaCacheTTL - time.Second),
	}
	quotaMu.Unlock()
	if got := cachedBalance(sa, fetch); got.Left != 102 || calls != 2 {
		t.Errorf("after expiry: balance = %v, calls = %d, want 102 / 2", got.Left, calls)
	}
}

func TestNumericValue(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{"840.36", 840.36},
		{840.36, 840.36},
		{json.Number("12.5"), 12.5},
		{"", 0},
		{nil, 0},
		{"abc", 0},
	}
	for _, c := range cases {
		if got := numericValue(c.in); got != c.want {
			t.Errorf("numericValue(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestOwnCredentialFiltering(t *testing.T) {
	// The other realm's plugin name: CN and Global ship as two plugins and each
	// must claim only its own accounts.
	other := "workbuddy-global"
	if providerName == other {
		other = "workbuddy"
	}
	cases := []struct {
		name  string
		entry pluginapi.HostAuthFileEntry
		want  bool
	}{
		{"our provider", pluginapi.HostAuthFileEntry{Provider: providerName}, true},
		{"stored type", pluginapi.HostAuthFileEntry{Type: providerName}, true},
		{"other realm provider", pluginapi.HostAuthFileEntry{Provider: other}, false},
		// A CN credential carries type "workbuddy"; the Global plugin used to
		// claim it through that shared type and listed every CN account.
		{"other realm type", pluginapi.HostAuthFileEntry{Type: other}, false},
		{"unrelated provider", pluginapi.HostAuthFileEntry{Provider: "codex", Type: "codex"}, false},
		{"empty", pluginapi.HostAuthFileEntry{}, false},
	}
	for _, c := range cases {
		if got := ownCredential(c.entry); got != c.want {
			t.Errorf("%s: ownCredential = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMasking(t *testing.T) {
	if got := maskName("知了十八"); got != "知***" {
		t.Errorf("maskName = %q, want 知***", got)
	}
	if got := maskName("a"); got != "a*" {
		t.Errorf("maskName single = %q", got)
	}
	if got := maskName(""); got != "" {
		t.Errorf("maskName empty = %q", got)
	}
	if got := shortID("98e520f0-17bc-4370-a161-1b3885812d84"); got != "98e520f0…" {
		t.Errorf("shortID = %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("shortID short = %q", got)
	}
}

func TestRenderQuotaHTMLMaskedAndEscaped(t *testing.T) {
	snapshot := quotaSnapshot{
		GeneratedAt: time.Now(),
		Accounts: []quotaAccount{
			{
				ID:     "workbuddy-uid",
				Label:  "知了十八",
				UID:    "98e520f0-17bc-4370-a161-1b3885812d84",
				Region: regionCN,
				Plan:   "free",
				Left:   1640,
				Packages: []quotaPackage{
					{Name: `<script>alert(1)</script>`, Left: 500, Total: 500, CycleEnd: "2026-09-30"},
				},
			},
			{ID: "x", Label: "过期号", Region: regionGlobal, Error: "登录已过期"},
		},
	}
	page := renderQuotaHTML(snapshot, true)
	if !strings.Contains(page, "知***") {
		t.Error("masked page leaks the account name")
	}
	if strings.Contains(page, "98e520f0-17bc-4370-a161") || strings.Contains(page, "知了十八") {
		t.Error("masked page leaks the account id or full name")
	}
	if !strings.Contains(page, "98e520f0…") {
		t.Error("masked page should still show a short id")
	}
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("package name is not escaped")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("expected an escaped package name")
	}
	if !strings.Contains(page, "1640") {
		t.Error("balance missing from page")
	}
	if !strings.Contains(page, "登录已过期") {
		t.Error("per-account error missing from page")
	}
	// The unmasked copy is what the authenticated JSON path is for; the page
	// builder must still be able to emit it.
	if full := renderQuotaHTML(snapshot, false); !strings.Contains(full, "知了十八") {
		t.Error("unmasked render should keep the full name")
	}
}
