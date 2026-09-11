package main

// Credit balance ("quota") lookup.
//
// The balance a workbuddy account can still spend is an account-level pool of
// credits, not a per-model allowance: the model catalog only carries a rate
// (models[].credits in /v3/config) and the pool is what every model bills
// against. The pool lives in the billing resource-package list served by
// /v2/billing/meter/get-user-resource, which is what this file reads.
//
// Two upstream quirks are load-bearing here:
//
//   - the route only accepts POST; a GET is answered 404 by the gateway;
//   - a "CLI/<ver> CodeBuddy/<ver>" User-Agent is mandatory, otherwise the
//     gateway answers 403 with code 10085, which reads like a permission
//     problem but is only a UA check. commonHeaders() already sends it.

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
	pathUserResource = "/v2/billing/meter/get-user-resource"
	pathPaymentType  = "/v2/billing/meter/get-payment-type"

	// quotaProductCode is the CodeBuddy billing product. It is the same on
	// both realms; without it the endpoint answers 403/10085.
	quotaProductCode = "p_tcaca"

	// quotaCacheTTL keeps a management page refresh from firing one upstream
	// round trip per credential on every click.
	quotaCacheTTL = 60 * time.Second

	// quotaTimeout bounds one balance lookup. Keep it short: auth.parse is on
	// the host's synchronous credential-loading path, and a slow billing
	// endpoint must not stall a startup or a credential rescan.
	quotaTimeout = 10 * time.Second
)

// quotaPackage is one billing resource package: a block of credits with its
// own validity window.
type quotaPackage struct {
	Name     string  `json:"name"`
	Code     string  `json:"code,omitempty"`
	Left     float64 `json:"left"`
	Total    float64 `json:"total"`
	CycleEnd string  `json:"cycle_end,omitempty"`
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

// resourceRequestBody builds the paging/validity window the endpoint expects.
// Without the time range the gateway treats the call as malformed.
func resourceRequestBody() []byte {
	const layout = "2006-01-02 15:04:05"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              quotaProductCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(layout),
		"PackageEndTimeRangeEnd":   now.AddDate(10, 0, 0).Format(layout),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// resourceResponse is the subset of the billing payload we render. The fields
// are PascalCase because the endpoint proxies a Tencent billing API verbatim.
//
// Accounts is decoded as raw maps rather than a typed struct: upstream mixes
// string and number for the same field (a live account returned
// DeductionEndTime as a millisecond epoch), and a strict struct turns any such
// drift into a failed lookup for the whole account. Reading field by field
// degrades one value instead of the whole page.
type resourceResponse struct {
	Response struct {
		Data struct {
			TotalDosage any              `json:"TotalDosage"`
			Accounts    []map[string]any `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
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
	data, status, err := quotaHTTP(base, sa, pathUserResource, resourceRequestBody())
	if err != nil {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			out.Error = "登录已过期，请在 CPA 面板重新登录该账号"
		} else {
			out.Error = err.Error()
		}
		return out
	}
	var res resourceResponse
	if err := json.Unmarshal(data, &res); err != nil {
		out.Error = "parse failed: " + err.Error()
		return out
	}
	for _, pkg := range res.Response.Data.Accounts {
		cycle := cycleEndValue(pkg["CycleEndTime"])
		if cycle == "" {
			cycle = cycleEndValue(pkg["DeductionEndTime"])
		}
		out.Packages = append(out.Packages, quotaPackage{
			Name:     packageName(pkg),
			Code:     packageCode(pkg),
			Left:     numericValue(pkg["CycleCapacityRemainPrecise"]),
			Total:    numericValue(pkg["CycleCapacitySizePrecise"]),
			CycleEnd: cycle,
		})
	}
	out.Left = numericValue(res.Response.Data.TotalDosage)
	if out.Left == 0 {
		for _, p := range out.Packages {
			out.Left += p.Left
		}
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
	sort.Slice(out.Packages, func(i, j int) bool {
		if out.Packages[i].Left == out.Packages[j].Left {
			return out.Packages[i].Name < out.Packages[j].Name
		}
		return out.Packages[i].Left > out.Packages[j].Left
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

	account := fetchQuota(sa)

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
