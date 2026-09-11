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

// realSummaryPayload is a verbatim (trimmed) api1 response. Note
// CycleRemainCapacity arriving as a *string* for one package and as a number
// for the other: the endpoint mixes both.
const realSummaryPayload = `{
  "code": 0,
  "msg": "OK",
  "data": {
    "Packages": [
      {
        "PackageCode": "TCACA_code_008_cfWoLwvjU4",
        "CycleTotalCapacity": "500",
        "CycleRemainCapacity": "500",
        "CycleUsedCapacity": "0",
        "CapacityUnit": "credits"
      },
      {
        "PackageCode": "TCACA_code_007_nzdH5h4Nl0",
        "CycleTotalCapacity": 1500,
        "CycleRemainCapacity": 840.36000015,
        "CycleUsedCapacity": "659.63999985",
        "CapacityUnit": "credits"
      },
      {
        "PackageCode": "TCACA_code_002_AkiJS3ZHF5",
        "CycleTotalCapacity": 200,
        "CycleRemainCapacity": 50,
        "CycleUsedCapacity": "150",
        "CapacityUnit": "credits"
      }
    ],
    "SubscriptionPackageCode": "",
    "IsPaidUser": false,
    "IsProtectedPriceUser": false
  }
}`

// realFreePayload is the matching api3 response. Only the refilling package
// carries a slice; the bonus one carries none, which is what a real CN
// account looks like today.
const realFreePayload = `{
  "code": 0,
  "msg": "OK",
  "data": {
    "TotalCount": 2,
    "Accounts": [
      {
        "PackageName": "CodeBuddy个人体验版",
        "PackageCode": "TCACA_code_008_cfWoLwvjU4",
        "CapacityType": 4,
        "CycleEndTime": "2026-09-30 23:59:59",
        "SlicePeriodUsageDetails": [
          {
            "SlicePeriodCapacitySizePrecise": "500",
            "SlicePeriodCapacityRemainPrecise": "120"
          }
        ]
      },
      {
        "PackageName": "CodeBuddy个人版国内运营裂变包",
        "PackageCode": "TCACA_code_007_nzdH5h4Nl0",
        "CapacityType": 1,
        "CycleEndTime": "2026-09-16 22:26:56",
        "SlicePeriodUsageDetails": null
      }
    ]
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

// recordedCall is one request the fake upstream saw.
type recordedCall struct {
	method string
	header http.Header
	body   []byte
}

// newQuotaServer answers the three billing paths and records what was asked,
// so a test can assert the mandatory UA header, the POST method and the body.
func newQuotaServer(t *testing.T, summaryStatus int) (*httptest.Server, map[string]recordedCall) {
	t.Helper()
	calls := map[string]recordedCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls[r.URL.Path] = recordedCall{method: r.Method, header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathResourceSummary:
			if summaryStatus != http.StatusOK {
				w.WriteHeader(summaryStatus)
				_, _ = w.Write([]byte(`{"code":10085,"msg":"请求不合法"}`))
				return
			}
			_, _ = w.Write([]byte(realSummaryPayload))
		case pathFreePackages:
			_, _ = w.Write([]byte(realFreePayload))
		case pathPaymentType:
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"paymentType":"free"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestFetchQuotaParsesPackages(t *testing.T) {
	srv, calls := newQuotaServer(t, http.StatusOK)
	got := fetchQuotaFrom(srv.URL, testStoredAuth())
	if got.Error != "" {
		t.Fatalf("unexpected error: %s", got.Error)
	}
	// The balance is the sum of the packages; api1 has no total field.
	if got.Left < 1390.3 || got.Left > 1390.4 {
		t.Errorf("left = %v, want ~1390.36", got.Left)
	}
	if len(got.Packages) != 3 {
		t.Fatalf("packages = %d, want 3", len(got.Packages))
	}
	// The package that refills leads: it is the one an operator watches.
	first := got.Packages[0]
	if !first.Free || first.Code != "TCACA_code_008_cfWoLwvjU4" {
		t.Fatalf("first package = %+v, want the free 008 package", first)
	}
	if first.Left != 500 || first.Total != 500 {
		t.Errorf("free package = %v/%v, want 500/500", first.Left, first.Total)
	}
	if first.CycleEnd != "2026-09-30 23:59:59" {
		t.Errorf("cycle end = %q", first.CycleEnd)
	}
	// The refilling package carries the current slice; the other does not.
	if !first.HasSlice || first.SliceLeft != 120 || first.SliceTotal != 500 {
		t.Errorf("slice = %v %v/%v, want 120/500", first.HasSlice, first.SliceLeft, first.SliceTotal)
	}
	// 运营裂变包 is a free code in the client's list but does not refill.
	second := got.Packages[1]
	if !second.Free || second.Code != "TCACA_code_007_nzdH5h4Nl0" {
		t.Fatalf("second package = %+v, want the free 007 package", second)
	}
	if second.HasSlice {
		t.Errorf("bonus package has a slice it should not: %+v", second)
	}
	if second.Name != "CodeBuddy个人版国内运营裂变包" {
		t.Errorf("name = %q, want the upstream one", second.Name)
	}
	// The purchase sorts last and falls back to the local name map, since
	// api3 only knows about free codes.
	third := got.Packages[2]
	if third.Free {
		t.Errorf("paid package marked free: %+v", third)
	}
	if third.Name != "Pro 月包" {
		t.Errorf("paid package name = %q, want Pro 月包", third.Name)
	}
	if got.Plan != "free" {
		t.Errorf("plan = %q, want free", got.Plan)
	}
	if got.Label != "知了十八" || got.Region != regionCN {
		t.Errorf("label/region = %q/%q", got.Label, got.Region)
	}
	// The gateway rejects the call without a CLI User-Agent.
	call := calls[pathResourceSummary]
	if ua := call.header.Get("User-Agent"); !strings.Contains(ua, "CodeBuddy/") {
		t.Errorf("User-Agent = %q", ua)
	}
	if call.method != http.MethodPost {
		t.Errorf("method = %s, want POST (the route is method-specific)", call.method)
	}
	// api1 takes no business parameters, so an empty object is a valid body.
	if body := strings.TrimSpace(string(call.body)); body != "{}" {
		t.Errorf("summary body = %s, want {}", body)
	}
	// api3 is what makes the slice show up, and it must send PackageCodes or
	// upstream answers 400 / 10001.
	free := calls[pathFreePackages]
	if !strings.Contains(string(free.body), "PackageCodes") {
		t.Errorf("free body = %s, want PackageCodes", free.body)
	}
	if !strings.Contains(string(free.body), "SlicePeriodStartTime") {
		t.Errorf("free body = %s, want the slice range", free.body)
	}
}

// TestFetchQuotaWithoutSlice covers the account shape seen in production: api3
// returns the packages but no SlicePeriodUsageDetails, and the balance must
// still come through from the cycle figures instead of collapsing to zero.
func TestFetchQuotaWithoutSlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathResourceSummary:
			_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[
			 {"PackageCode":"TCACA_code_035_ArVxJcGDsm","CycleTotalCapacity":"100",
			  "CycleRemainCapacity":"22.71000001"}],"IsPaidUser":false}}`))
		case pathFreePackages:
			_, _ = w.Write([]byte(`{"code":0,"data":{"Accounts":[
			 {"PackageCode":"TCACA_code_035_ArVxJcGDsm","PackageName":"Free Plan Subscription",
			  "CapacityType":4,"CycleEndTime":"2026-09-30 23:59:59","SlicePeriodUsageDetails":null}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got := fetchQuotaFrom(srv.URL, testStoredAuth())
	if got.Error != "" {
		t.Fatalf("unexpected error: %s", got.Error)
	}
	if got.Left < 22.7 || got.Left > 22.8 {
		t.Errorf("left = %v, want ~22.71", got.Left)
	}
	if len(got.Packages) != 1 {
		t.Fatalf("packages = %d, want 1", len(got.Packages))
	}
	if got.Packages[0].HasSlice {
		t.Errorf("slice = %v/%v, want none", got.Packages[0].SliceLeft, got.Packages[0].SliceTotal)
	}
	if got.Packages[0].Total != 100 {
		t.Errorf("total = %v, want 100", got.Packages[0].Total)
	}
}

// TestFetchQuotaSurvivesFreePackageFailure checks that api3 is best-effort: it
// only enriches api1, so a failure must not hide the balance.
func TestFetchQuotaSurvivesFreePackageFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == pathResourceSummary {
			_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[
			 {"PackageCode":"TCACA_code_008_cfWoLwvjU4","CycleTotalCapacity":"500",
			  "CycleRemainCapacity":"500"}],"IsPaidUser":false}}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	got := fetchQuotaFrom(srv.URL, testStoredAuth())
	if got.Error != "" {
		t.Fatalf("unexpected error: %s", got.Error)
	}
	if got.Left != 500 {
		t.Errorf("left = %v, want 500", got.Left)
	}
	// No upstream name means the local code map has to supply one rather than
	// showing a raw TCACA_code_... string.
	if got.Packages[0].Name != packageDisplayNames["TCACA_code_008_cfWoLwvjU4"] {
		t.Errorf("name = %q, want the mapped name", got.Packages[0].Name)
	}
}

func TestFetchQuotaExpiredToken(t *testing.T) {
	srv, _ := newQuotaServer(t, http.StatusUnauthorized)
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

// TestFetchQuotaToleratesMixedTimestampTypes covers a real response shape the
// billing API still produces: CycleEndTime arriving as a millisecond epoch for
// one package and as a formatted string for another. Decoding that into a
// string field failed the whole account.
func TestFetchQuotaToleratesMixedTimestampTypes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathResourceSummary:
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Packages":[
			 {"PackageCode":"TCACA_code_008_cfWoLwvjU4","CycleTotalCapacity":500,
			  "CycleRemainCapacity":"40"},
			 {"PackageCode":"TCACA_code_007_nzdH5h4Nl0","CycleTotalCapacity":100,
			  "CycleRemainCapacity":100}],"IsPaidUser":false}}`))
		case pathFreePackages:
			_, _ = w.Write([]byte(`{"code":0,"data":{"Accounts":[
			 {"PackageCode":"TCACA_code_008_cfWoLwvjU4","CycleEndTime":"2026-09-30 23:59:59"},
			 {"PackageCode":"TCACA_code_007_nzdH5h4Nl0","CycleEndTime":1791680409000}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got := fetchQuotaFrom(srv.URL, testStoredAuth())
	if got.Error != "" {
		t.Fatalf("unexpected error: %s", got.Error)
	}
	if got.Left != 140 {
		t.Errorf("left = %v, want 140", got.Left)
	}
	if len(got.Packages) != 2 {
		t.Fatalf("packages = %d, want 2", len(got.Packages))
	}
	byCode := map[string]quotaPackage{}
	for _, p := range got.Packages {
		byCode[p.Code] = p
	}
	// A formatted CycleEndTime comes through untouched.
	if got := byCode["TCACA_code_008_cfWoLwvjU4"].CycleEnd; got != "2026-09-30 23:59:59" {
		t.Errorf("cycle end = %q", got)
	}
	// The epoch-only row is rendered as a local timestamp, not dropped and not
	// shown as a raw number.
	epoch := byCode["TCACA_code_007_nzdH5h4Nl0"].CycleEnd
	if epoch == "" || epoch == "1791680409000" {
		t.Errorf("epoch cycle end = %q, want a formatted time", epoch)
	}
	if !strings.Contains(epoch, "2026-10-11") {
		t.Errorf("epoch cycle end = %q, want the 2026-10-11 date", epoch)
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

// TestRenderQuotaHTMLGroupsFreePackages checks the reason api3 is read at all:
// the refilling free package is listed apart from the purchases, and an
// account whose backend sends no slice says so instead of showing a zero.
func TestRenderQuotaHTMLGroupsFreePackages(t *testing.T) {
	snapshot := quotaSnapshot{
		GeneratedAt: time.Now(),
		Accounts: []quotaAccount{{
			ID: "workbuddy-uid", Label: "小楚", Region: regionCN, Left: 665.26,
			Packages: []quotaPackage{
				{Name: "体验版", Code: "TCACA_code_008_cfWoLwvjU4", Left: 0, Total: 500,
					CycleEnd: "2026-09-30 23:59:59", Free: true,
					HasSlice: true, SliceLeft: 0, SliceTotal: 500},
				{Name: "Free Plan Subscription", Code: "TCACA_code_035_ArVxJcGDsm",
					Left: 22.71, Total: 100, Free: true},
				{Name: "Pro 月包", Code: "TCACA_code_002_AkiJS3ZHF5", Left: 642.55, Total: 2000},
			},
		}},
	}
	page := renderQuotaHTML(snapshot, false)
	for _, want := range []string{
		"免费包（周期刷新）", "付费/赠送包", "Pro 月包",
		"0 / 500", // the slice column
		"未下发",     // the package with no slice details
		"665.26",  // fractional totals keep two decimals
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// The paid group has no slice column, so the placeholder cell must appear
	// exactly once: only for the free package that lacks slice details.
	if n := strings.Count(page, `<span class="quiet">未下发</span>`); n != 1 {
		t.Errorf("placeholder cell appears %d times, want 1", n)
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
