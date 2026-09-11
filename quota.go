package main

// Credit balance ("quota") lookup.
//
// The balance a workbuddy account can still spend is an account-level pool of
// credits, not a per-model allowance: the model catalog only carries a rate
// (models[].credits in /v3/config) and the pool is what every model bills
// against. There is no per-model quota of any kind — see README.
//
// The pool is split into billing resource packages, and two of them matter:
//
//   - paid/bonus packages, which expire on their own schedule;
//   - the free plan package, which refills every cycle ("slice"). Packages
//     with CapacityType 4 are the refilling kind; the client calls them
//     分片递减型 and reads the current slice from SlicePeriodUsageDetails.
//
// Both are read from the two newer billing routes, api1 and api3 in the
// client's own naming. Two upstream quirks are load-bearing here:
//
//   - these routes live at the API *root*, with no /v2 prefix. /v2/billing/
//     meter/... answers 404 for them, and the prefix is what the older
//     get-user-resource route uses, so it is an easy thing to get wrong;
//   - they only accept POST, and a "CLI/<ver> CodeBuddy/<ver>" User-Agent is
//     mandatory, otherwise the gateway answers 403 with code 10085, which
//     reads like a permission problem but is only a UA check.
//     commonHeaders() already sends it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// pathResourceSummary is "api1": the balance, plan tier and paid status,
	// aggregated per package code. No /v2 prefix — see the file comment.
	pathResourceSummary = "/billing/meter/get-user-resource-summary"
	// pathFreePackages is "api3": the free packages with their current
	// refilling slice. Also at the API root.
	pathFreePackages = "/billing/meter/get-user-resource-free-packages"
	pathPaymentType  = "/v2/billing/meter/get-payment-type"

	// quotaCacheTTL keeps a management page refresh from firing one upstream
	// round trip per credential on every click.
	quotaCacheTTL = 60 * time.Second

	// quotaTimeout bounds one balance lookup. Keep it short: auth.parse is on
	// the host's synchronous credential-loading path, and a slow billing
	// endpoint must not stall a startup or a credential rescan.
	quotaTimeout = 10 * time.Second
)

// freePackageCodes is the client's FREE_PACKAGE_CODES list: the packages that
// are free rather than purchased. api3 requires PackageCodes to be sent —
// without it the route answers 400 / 10001 "PackageCodes required" — and it
// only ever returns these codes, so sending anything else is pointless.
var freePackageCodes = []string{
	"TCACA_code_001_PqouKr6QWV", // free
	"TCACA_code_008_cfWoLwvjU4", // freeMon: CN 体验版
	"TCACA_code_035_ArVxJcGDsm", // freeMonIntl
	"TCACA_code_006_DbXS0lrypC", // gift
	"TCACA_code_039_KRcQj7wUat", // proTrialMon
	"TCACA_code_040_mi9rCYg46x", // proTrialYear
	"TCACA_code_007_nzdH5h4Nl0", // activity
	"TCACA_code_028_NtpWi0jzXs", // bonus28
	"TCACA_code_037_WxOD3MpI2o", // bonusIntl
	"TCACA_code_029_6wCGEWquYy", // bonus29
	"TCACA_code_030_BjSt89qTvr", // bonus30
}

// packageDisplayNames maps a commodity code to a readable name. api1 returns
// only codes, so without this the page would show raw TCACA_code_... strings.
// Upstream PackageName wins when api3 supplies it; this is the fallback.
var packageDisplayNames = map[string]string{
	"TCACA_code_001_PqouKr6QWV": "免费版",
	"TCACA_code_002_AkiJS3ZHF5": "Pro 月包",
	"TCACA_code_003_FAnt7lcmRT": "Pro 年包",
	"TCACA_code_005_maRGyrHhw1": "Pro 月包 Plus",
	"TCACA_code_006_DbXS0lrypC": "赠送包（Pro 试用）",
	"TCACA_code_007_nzdH5h4Nl0": "运营裂变包",
	"TCACA_code_008_cfWoLwvjU4": "体验版（周期刷新）",
	"TCACA_code_009_0XmEQc2xOf": "加量包",
	"TCACA_code_023_4xbGhMrE6q": "青春版",
	"TCACA_code_026_BaESVICNoi": "高级版",
	"TCACA_code_027_0FCGVA6vSa": "旗舰版",
	"TCACA_code_028_NtpWi0jzXs": "赠送包 28",
	"TCACA_code_029_6wCGEWquYy": "赠送包 29",
	"TCACA_code_030_BjSt89qTvr": "赠送包 30",
	"TCACA_code_035_ArVxJcGDsm": "Free Plan（周期刷新）",
	"TCACA_code_036_lupO5WgNdG": "加量包（国际）",
	"TCACA_code_037_WxOD3MpI2o": "赠送包（国际）",
	"TCACA_code_038_OhvqZtiPKr": "加量包",
	"TCACA_code_039_KRcQj7wUat": "Pro 试用（月）",
	"TCACA_code_040_mi9rCYg46x": "Pro 试用（年）",
}

// isFreePackageCode reports whether a package is one of the free codes, which
// is what decides which group the page files it under.
func isFreePackageCode(code string) bool {
	for _, c := range freePackageCodes {
		if strings.EqualFold(c, strings.TrimSpace(code)) {
			return true
		}
	}
	return false
}

// capacityTypeSlice is the billing CapacityType meaning "refills every slice
// period" (the client's 分片递减型). The free plan package carries it.
const capacityTypeSlice = 4

// quotaPackage is one billing resource package: a block of credits with its
// own validity window.
type quotaPackage struct {
	Name     string  `json:"name"`
	Code     string  `json:"code,omitempty"`
	Left     float64 `json:"left"`
	Total    float64 `json:"total"`
	CycleEnd string  `json:"cycle_end,omitempty"`
	// Free marks the free-plan/bonus packages, which refill on a cycle rather
	// than being bought once.
	Free bool `json:"free,omitempty"`
	// Refills marks the packages upstream flags as CapacityType 4, the ones
	// that top up every period. It is the more useful signal of the two: the
	// slice details below are withheld for many accounts, but this flag comes
	// with every api3 row.
	Refills bool `json:"refills,omitempty"`
	// Slice* carry the current refill period when upstream reports one. They
	// are absent for accounts whose backend does not emit slice details, in
	// which case Left/Total (the full cycle) are the only figures available.
	SliceLeft  float64 `json:"slice_left,omitempty"`
	SliceTotal float64 `json:"slice_total,omitempty"`
	HasSlice   bool    `json:"has_slice,omitempty"`
}

// quotaAccount is the balance of one stored credential.
type quotaAccount struct {
	ID       string         `json:"id"`
	Label    string         `json:"label"`
	UID      string         `json:"uid,omitempty"`
	Region   string         `json:"region"`
	Plan     string         `json:"plan,omitempty"`
	Left     float64        `json:"left"`
	Packages []quotaPackage `json:"packages,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// quotaSnapshot is a point-in-time view of every credential this plugin owns.
type quotaSnapshot struct {
	Accounts    []quotaAccount `json:"accounts"`
	GeneratedAt time.Time      `json:"generated_at"`
	Error       string         `json:"error,omitempty"`
}

var (
	quotaMu      sync.Mutex
	quotaCached  *quotaSnapshot
	managementMu sync.Mutex
	// managementBasePath is the /v0/management prefix the host hands us at
	// registration time; plugin routes must live under it.
	managementBasePath = "/v0/management"
)

// ---------------------------------------------------------------------------
// Upstream calls
// ---------------------------------------------------------------------------

// summaryRequestBody builds api1's request. It takes no business parameters:
// the route aggregates the whole account, so an empty object is correct.
func summaryRequestBody() []byte {
	return []byte(`{}`)
}

// freePackagesRequestBody builds api3's request.
//
// PackageCodes is mandatory — omitting it makes the route answer 400 with code
// 10001 — and the slice range is what makes upstream attach the current
// refill period to each package. The range is today, because the slice is a
// daily window.
func freePackagesRequestBody() []byte {
	const layout = "2006-01-02 15:04:05"
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 0, 1).Add(-time.Second)
	body := map[string]any{
		"PageNumber":           1,
		"PageSize":             100,
		"PackageCodes":         freePackageCodes,
		"Status":               []int{0, 3},
		"SlicePeriodStartTime": start.Format(layout),
		"SlicePeriodEndTime":   end.Format(layout),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// summaryResponse is api1's payload: one entry per package code with the
// cycle figures, plus whether the account is a paying one.
//
// Packages is decoded as raw maps for the same reason as the free packages
// below: upstream mixes strings and numbers, and a strict struct turns a
// drift in one account into a failed lookup for the whole page.
type summaryResponse struct {
	Packages   []map[string]any `json:"Packages"`
	IsPaidUser bool             `json:"IsPaidUser"`
}

// freePackagesResponse is api3's payload. Only SlicePeriodUsageDetails is read
// from it; the rest duplicates api1 at a coarser granularity.
type freePackagesResponse struct {
	Accounts []map[string]any `json:"Accounts"`
}

// sliceUsage is the current refill period of one package.
type sliceUsage struct {
	left  float64
	total float64
}

// packageFacts is what api3 adds to a package api1 already described: a
// readable name, when the cycle ends, whether it refills, and the current
// refill slice.
type packageFacts struct {
	name     string
	cycleEnd string
	refills  bool
	slice    *sliceUsage
}

// freePackageFacts reads api3 and indexes it by package code.
//
// It is best-effort: api3 only enriches what api1 already returned, so a
// failure here must not hide the balance. Accounts whose backend does not
// emit SlicePeriodUsageDetails simply come back without a slice, and the
// caller keeps showing the full-cycle figures.
func freePackageFacts(base string, sa *storedAuth) map[string]packageFacts {
	facts := map[string]packageFacts{}
	data, _, err := quotaHTTP(base, sa, pathFreePackages, freePackagesRequestBody())
	if err != nil {
		return facts
	}
	var res freePackagesResponse
	if err := json.Unmarshal(data, &res); err != nil {
		return facts
	}
	for _, pkg := range res.Accounts {
		code := packageCode(pkg)
		if code == "" {
			continue
		}
		fact := facts[code]
		if name := packageName(pkg); name != "" {
			fact.name = name
		}
		if end := cycleEndValue(pkg["CycleEndTime"]); end != "" {
			fact.cycleEnd = end
		}
		// CapacityType is the only marker that survives for accounts whose
		// backend withholds the slice details.
		if numericValue(pkg["CapacityType"]) == capacityTypeSlice {
			fact.refills = true
		}
		if size, left, ok := sliceOf(pkg); ok {
			fact.slice = &sliceUsage{left: left, total: size}
		}
		facts[code] = fact
	}
	return facts
}

// sliceOf reads the current refill period out of one api3 package.
//
// Upstream omits SlicePeriodUsageDetails for some packages entirely (the
// client carries a comment saying the CN free plan is one of them), and a
// missing slice is not an error: the caller falls back to the cycle figures.
func sliceOf(pkg map[string]any) (size, left float64, ok bool) {
	details, _ := pkg["SlicePeriodUsageDetails"].([]any)
	if len(details) == 0 {
		return 0, 0, false
	}
	slice, _ := details[0].(map[string]any)
	if slice == nil {
		return 0, 0, false
	}
	size = numericValue(slice["SlicePeriodCapacitySizePrecise"])
	left = numericValue(slice["SlicePeriodCapacityRemainPrecise"])
	if size == 0 && left == 0 {
		return 0, 0, false
	}
	return size, left, true
}

// packageDisplayName prefers the name upstream sent, then the local code map,
// and falls back to the raw code so a new package is still visible.
func packageDisplayName(code, upstream string) string {
	if name := strings.TrimSpace(upstream); name != "" {
		return name
	}
	if name := packageDisplayNames[code]; name != "" {
		return name
	}
	return code
}

// packageName and packageCode read the two label fields tolerantly.
func packageName(pkg map[string]any) string {
	name, _ := pkg["PackageName"].(string)
	return strings.TrimSpace(name)
}

func packageCode(pkg map[string]any) string {
	code, _ := pkg["PackageCode"].(string)
	return strings.TrimSpace(code)
}

// cycleEndValue normalises a billing timestamp that may be either a formatted
// string or a millisecond epoch. A live account returned a number here, and
// decoding it as a string failed the whole response.
func cycleEndValue(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return epochMillis(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return ""
		}
		return epochMillis(f)
	default:
		return ""
	}
}

// epochMillis renders a millisecond epoch, ignoring values too small to be one.
func epochMillis(ms float64) string {
	if ms < 1e12 {
		return ""
	}
	return time.UnixMilli(int64(ms)).Local().Format("2006-01-02 15:04:05")
}

// numericValue tolerates both "840.36" and 840.36, which the endpoint mixes
// depending on which billing backend answered.
func numericValue(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	case json.Number:
		f, _ := t.Float64()
		return f
	default:
		return 0
	}
}

// quotaHTTP posts to one billing path and returns the inner data payload.
// Unlike doJSON it reports the HTTP status separately so callers can tell an
// expired token (401) from a malformed request (403).
func quotaHTTP(base string, sa *storedAuth, path string, body []byte) (json.RawMessage, int, error) {
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	commonHeaders(req, sa.Region)
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	if sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	}
	if sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	}
	client := &http.Client{Timeout: quotaTimeout, Transport: sharedHTTPClient().Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if resp.StatusCode >= 400 || env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("upstream %d code=%d %s", resp.StatusCode, env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// fetchQuota reads the remaining credit pool for one credential.
//
// It never refreshes the access token. Upstream refresh rotates the refresh
// token, and a rotation that is not persisted through host.auth.save would
// silently destroy the credential the next time the host tries to use it. An
// expired token is therefore reported as an error so the operator re-logs-in
// from the management panel instead.
func fetchQuota(sa *storedAuth) quotaAccount {
	return fetchQuotaFrom(baseFor(sa.Region), sa)
}

// fetchQuotaFrom is fetchQuota against an explicit base URL, which is what the
// tests point at an httptest server.
func fetchQuotaFrom(base string, sa *storedAuth) quotaAccount {
	out := quotaAccount{
		ID:     providerName + "-" + accountIdentity(sa),
		Label:  quotaLabel(sa),
		UID:    sa.Account.UID,
		Region: normalizeRegion(sa.Region),
	}
	data, status, err := quotaHTTP(base, sa, pathResourceSummary, summaryRequestBody())
	if err != nil {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			out.Error = "登录已过期，请在 CPA 面板重新登录该账号"
		} else {
			out.Error = err.Error()
		}
		return out
	}
	var res summaryResponse
	if err := json.Unmarshal(data, &res); err != nil {
		out.Error = "parse failed: " + err.Error()
		return out
	}
	facts := freePackageFacts(base, sa)
	for _, pkg := range res.Packages {
		code := packageCode(pkg)
		p := quotaPackage{
			Name:  packageDisplayName(code, facts[code].name),
			Code:  code,
			Left:  numericValue(pkg["CycleRemainCapacity"]),
			Total: numericValue(pkg["CycleTotalCapacity"]),
			Free:  isFreePackageCode(code),
		}
		if fact, ok := facts[code]; ok {
			p.CycleEnd = fact.cycleEnd
			p.Refills = fact.refills
			if fact.slice != nil {
				p.HasSlice = true
				p.SliceLeft = fact.slice.left
				p.SliceTotal = fact.slice.total
			}
		}
		out.Packages = append(out.Packages, p)
		out.Left += p.Left
	}
	// The plan type is cosmetic; a failure to read it must not hide the
	// balance we just fetched.
	if planData, _, errPlan := quotaHTTP(base, sa, pathPaymentType, []byte("{}")); errPlan == nil {
		var plan struct {
			PaymentType string `json:"paymentType"`
		}
		if json.Unmarshal(planData, &plan) == nil {
			out.Plan = plan.PaymentType
		}
	}
	if out.Plan == "" {
		// api1 already knows whether this is a paying account; use it when the
		// dedicated route gave nothing.
		if res.IsPaidUser {
			out.Plan = "paid"
		} else {
			out.Plan = "free"
		}
	}
	sort.Slice(out.Packages, func(i, j int) bool {
		a, b := out.Packages[i], out.Packages[j]
		// The refilling package is the one an operator is watching, so it
		// leads; the rest of the free group follows, then the purchases.
		if a.HasSlice != b.HasSlice {
			return a.HasSlice
		}
		if a.Free != b.Free {
			return a.Free
		}
		if a.Left == b.Left {
			return a.Name < b.Name
		}
		return a.Left > b.Left
	})
	return out
}

// cachedAccountBalance memoises one balance per account for a short window.
//
// The host parses the same credential several times during a single reload
// (initial load, model discovery, ...), and every parse used to cost an
// upstream round trip. A minute of caching collapses those repeats while
// keeping a manual host refresh responsive.
func cachedAccountBalance(sa *storedAuth) quotaAccount {
	return cachedBalance(sa, fetchQuota)
}

// cachedBalance is cachedAccountBalance with the lookup injected, so tests can
// count how often upstream is actually reached.
func cachedBalance(sa *storedAuth, fetch func(*storedAuth) quotaAccount) quotaAccount {
	key := strings.TrimSpace(sa.Account.UID)
	if key == "" {
		key = accountIdentity(sa)
	}
	quotaMu.Lock()
	entry, ok := quotaBalanceCache[key]
	if ok && time.Since(entry.at) < quotaCacheTTL {
		quotaMu.Unlock()
		return entry.account
	}
	quotaMu.Unlock()

	account := fetch(sa)

	quotaMu.Lock()
	quotaBalanceCache[key] = quotaBalanceEntry{account: account, at: time.Now()}
	quotaMu.Unlock()
	return account
}

var (
	quotaBalanceCache = map[string]quotaBalanceEntry{}
)

type quotaBalanceEntry struct {
	account quotaAccount
	at      time.Time
}

// quotaLabel is the human-readable name shown next to a balance.
func quotaLabel(sa *storedAuth) string {
	if nickname := strings.TrimSpace(sa.Account.Nickname); nickname != "" {
		return nickname
	}
	if uid := strings.TrimSpace(sa.Account.UID); uid != "" {
		return uid
	}
	return accountIdentity(sa)
}

// ---------------------------------------------------------------------------
// Credential enumeration through the host
// ---------------------------------------------------------------------------

// hostRPC calls a host callback and returns the inner result payload.
func hostRPC(method string, request []byte) (json.RawMessage, error) {
	raw, err := hostCall(method, request)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("host %s: empty response", method)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("host %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("host %s: %s %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host %s: request failed", method)
	}
	return env.Result, nil
}

// ownCredentials lists every credential in the host store that belongs to this
// plugin and loads its stored JSON. The host holds credentials for every
// provider, so the list is filtered down to ours before anything is fetched.
func ownCredentials() ([]*storedAuth, error) {
	result, err := hostRPC(pluginabi.MethodHostAuthList, []byte(`{}`))
	if err != nil {
		return nil, err
	}
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		return nil, fmt.Errorf("host.auth.list: %w", err)
	}
	var out []*storedAuth
	for _, entry := range list.Files {
		if !ownCredential(entry) {
			continue
		}
		index := strings.TrimSpace(entry.AuthIndex)
		if index == "" {
			continue
		}
		req, _ := json.Marshal(map[string]string{"auth_index": index})
		result, err := hostRPC(pluginabi.MethodHostAuthGet, req)
		if err != nil {
			continue
		}
		var got struct {
			JSON json.RawMessage `json:"json"`
		}
		if json.Unmarshal(result, &got) != nil || len(got.JSON) == 0 {
			continue
		}
		sa, errParse := parseStored(got.JSON)
		if errParse != nil {
			continue
		}
		out = append(out, sa)
	}
	return out, nil
}

// ownCredential reports whether an entry belongs to this plugin instance.
//
// CN and Global are two separate plugins with two separate provider names, and
// each must only show its own accounts: matching the shared "workbuddy" type
// made the Global plugin list every CN credential as well. Both the provider
// key and the stored type are therefore compared against *this* plugin's name.
func ownCredential(entry pluginapi.HostAuthFileEntry) bool {
	if strings.EqualFold(strings.TrimSpace(entry.Provider), providerName) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(entry.Type), providerName)
}

// ---------------------------------------------------------------------------
// Snapshot assembly
// ---------------------------------------------------------------------------

// collectQuota returns the balance of every credential this plugin owns.
// Results are cached briefly; force re-reads upstream.
func collectQuota(force bool) quotaSnapshot {
	quotaMu.Lock()
	if !force && quotaCached != nil && time.Since(quotaCached.GeneratedAt) < quotaCacheTTL {
		snapshot := *quotaCached
		quotaMu.Unlock()
		return snapshot
	}
	quotaMu.Unlock()

	snapshot := quotaSnapshot{GeneratedAt: time.Now()}
	auths, err := ownCredentials()
	if err != nil {
		snapshot.Error = err.Error()
		quotaMu.Lock()
		quotaCached = &snapshot
		quotaMu.Unlock()
		return snapshot
	}
	if len(auths) == 0 {
		snapshot.Error = "host 里没有 workbuddy 凭据"
	}

	accounts := make([]quotaAccount, len(auths))
	var wg sync.WaitGroup
	for i, sa := range auths {
		wg.Add(1)
		go func(idx int, sa *storedAuth) {
			defer wg.Done()
			accounts[idx] = fetchQuota(sa)
		}(i, sa)
	}
	wg.Wait()
	snapshot.Accounts = accounts

	sort.Slice(snapshot.Accounts, func(i, j int) bool {
		return snapshot.Accounts[i].Label < snapshot.Accounts[j].Label
	})

	quotaMu.Lock()
	quotaCached = &snapshot
	quotaMu.Unlock()
	return snapshot
}

// invalidateQuotaCache drops a cached snapshot, for tests and for a forced
// refresh from the management UI.
func invalidateQuotaCache() {
	quotaMu.Lock()
	quotaCached = nil
	quotaMu.Unlock()
}

// ---------------------------------------------------------------------------
// Display helpers
// ---------------------------------------------------------------------------

// maskName hides all but the leading character of a name. The resource route is
// reachable without management authentication, so the page must not leak
// account identifiers to whoever can reach the panel host.
func maskName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	runes := []rune(name)
	if len(runes) == 1 {
		return string(runes) + "*"
	}
	return string(runes[0]) + strings.Repeat("*", len(runes)-1)
}

// shortID keeps a stable, non-identifying prefix of an id.
func shortID(id string) string {
	id = strings.TrimSpace(id)
	runes := []rune(id)
	if len(runes) <= 8 {
		return id
	}
	return string(runes[:8]) + "…"
}
