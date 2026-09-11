package main

// Management API surface: the plugin registers a "额度" (credit balance) entry
// in the CPA management UI plus a JSON route for programmatic reads.
//
//   - resource route  /v0/resource/plugins/<pluginID>/quota  -> HTML page.
//     These routes are browser-navigable and, per the SDK, are *not*
//     management-authenticated, so the page masks account names and ids.
//   - management route /v0/management/.../workbuddy/quota    -> full JSON.
//     This one is behind the management auth the host already enforces.

import (
	"encoding/json"
	"html"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const quotaResourcePath = "/quota"

// handleManagementRegister advertises the balance routes. The host calls this
// once after plugin.register when the plugin declares management_api.
func handleManagementRegister(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// The SDK struct carries no json tag, so the field serialises as "BasePath";
	// accept the snake_case spelling too in case a host version differs.
	var loose struct {
		BasePath string `json:"base_path"`
	}
	_ = json.Unmarshal(raw, &loose)
	base := strings.TrimRight(strings.TrimSpace(req.BasePath), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(loose.BasePath), "/")
	}
	if base == "" {
		base = "/v0/management"
	}
	managementMu.Lock()
	managementBasePath = base
	managementMu.Unlock()

	return okEnvelope(pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{{
			Method: http.MethodGet,
			// The path must be per-plugin: CN and Global are two plugins, and
			// registering the same path from both makes the host skip the
			// lower-priority one ("conflicts with a higher-priority plugin").
			Path:        base + "/" + providerName + "/quota",
			Description: "workbuddy 各账号剩余积分(JSON,需管理鉴权)",
		}},
		Resources: []pluginapi.ResourceRoute{{
			Path:        quotaResourcePath,
			Menu:        "额度",
			Description: "每个 workbuddy 账号的剩余积分（浏览器可直接打开）",
		}},
	})
}

// handleManagement serves both routes registered above.
func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodGet) {
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed,
			Headers: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
			Body:    []byte("method not allowed")})
	}
	force := strings.TrimSpace(req.Query.Get("refresh")) != ""
	snapshot := collectQuota(force)
	hostLog("info", "workbuddy: balance page served", map[string]any{
		"path":     req.Path,
		"accounts": len(snapshot.Accounts),
		"refresh":  force,
	})

	// Resource routes are public; hand them the masked HTML page. Everything
	// under /v0/management is already authenticated by the host, so it gets
	// the complete JSON.
	if isResourcePath(req.Path) {
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       []byte(renderQuotaHTML(snapshot, true)),
		})
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	})
}

// isResourcePath reports whether a request path is the browser-navigable
// resource copy rather than the authenticated management route.
func isResourcePath(path string) bool {
	return strings.Contains(path, "/resource/")
}

// renderQuotaHTML builds the balance page. Every value is escaped: account
// names and package names come from upstream and are attacker-influenced if a
// credential ever points somewhere unexpected.
func renderQuotaHTML(snapshot quotaSnapshot, masked bool) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>workbuddy 额度</title><style>`)
	b.WriteString(pageCSS)
	b.WriteString(`</style></head><body><main>`)
	b.WriteString(`<h1>workbuddy 额度</h1>`)
	b.WriteString(`<p class="meta">更新时间 ` + html.EscapeString(snapshot.GeneratedAt.Local().Format("2006-01-02 15:04:05")) +
		` · 缓存 60 秒 · <a href="?refresh=1">立即刷新</a></p>`)

	if snapshot.Error != "" {
		b.WriteString(`<p class="err">` + html.EscapeString(snapshot.Error) + `</p>`)
	}
	if len(snapshot.Accounts) == 0 && snapshot.Error == "" {
		b.WriteString(`<p class="empty">还没有 workbuddy 凭据。先在 CPA 面板登录一个账号。</p>`)
	}

	for _, acc := range snapshot.Accounts {
		label := acc.Label
		uid := acc.UID
		if masked {
			label = maskName(label)
			uid = shortID(uid)
		}
		b.WriteString(`<section class="card"><header><h2>` + html.EscapeString(label) + `</h2>`)
		badges := []string{acc.Region}
		if acc.Plan != "" {
			badges = append(badges, acc.Plan)
		}
		if uid != "" {
			badges = append(badges, uid)
		}
		for _, badge := range badges {
			b.WriteString(`<span class="badge">` + html.EscapeString(badge) + `</span>`)
		}
		b.WriteString(`</header>`)
		if acc.Error != "" {
			b.WriteString(`<p class="err">` + html.EscapeString(acc.Error) + `</p></section>`)
			continue
		}
		b.WriteString(`<p class="total">剩余 <strong>` + formatCredits(acc.Left) + `</strong> credits</p>`)
		// Free packages refill on a cycle, so they are the ones an operator
		// watches; listing them apart from the one-off purchases is the whole
		// point of reading api3 at all.
		var free, paid []quotaPackage
		for _, pkg := range acc.Packages {
			if pkg.Free {
				free = append(free, pkg)
			} else {
				paid = append(paid, pkg)
			}
		}
		renderPackageTable(&b, "免费包（周期刷新）", free, true)
		renderPackageTable(&b, "付费/赠送包", paid, false)
		b.WriteString(`</section>`)
	}
	b.WriteString(`<p class="foot">额度是账户级积分池，模型只决定消耗倍率（/v3/config 的 credits 字段），`)
	b.WriteString(`不存在按模型的额度。`)
	b.WriteString(`「本期剩余」是该免费包当前刷新周期的余量；上游不为部分账号下发该明细时显示「未下发」，`)
	b.WriteString(`此时「剩余 / 总量」为整个周期的值。`)
	b.WriteString(`过期凭据请在面板重新登录；本页不会主动刷新 token，以免轮换后的 refresh token 丢失。</p>`)
	b.WriteString(`</main></body></html>`)
	return b.String()
}

// renderPackageTable writes one package group. showSlice adds the current
// refill period, which is only meaningful for the refilling packages.
func renderPackageTable(b *strings.Builder, title string, pkgs []quotaPackage, showSlice bool) {
	if len(pkgs) == 0 {
		return
	}
	b.WriteString(`<h3>` + html.EscapeString(title) + `</h3>`)
	b.WriteString(`<table><thead><tr><th>资源包</th>`)
	if showSlice {
		b.WriteString(`<th>本期剩余</th>`)
	}
	b.WriteString(`<th>剩余 / 总量</th><th>周期截止</th></tr></thead><tbody>`)
	for _, pkg := range pkgs {
		b.WriteString(`<tr><td>` + html.EscapeString(pkg.Name) + `</td>`)
		if showSlice {
			b.WriteString(`<td class="num">`)
			if pkg.HasSlice {
				b.WriteString(formatCredits(pkg.SliceLeft) + ` / ` + formatCredits(pkg.SliceTotal))
			} else {
				b.WriteString(`<span class="quiet">未下发</span>`)
			}
			b.WriteString(`</td>`)
		}
		b.WriteString(`<td class="num">` + formatCredits(pkg.Left) + ` / ` + formatCredits(pkg.Total) +
			`</td><td>` + html.EscapeString(pkg.CycleEnd) + `</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
}

// formatCredits renders a whole number without decimals and a fractional one
// with two, so a balance reads "1165" and "840.36" rather than "1165.0".
func formatCredits(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

const pageCSS = `
:root{color-scheme:light}
body{margin:0;padding:24px;background:#f6f7f9;color:#1f2328;
 font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
main{max-width:960px;margin:0 auto}
h1{font-size:20px;margin:0 0 4px}
h2{font-size:15px;margin:0}
h3{font-size:13px;font-weight:500;color:#374151;margin:14px 0 4px}
.quiet{color:#9ca3af}
.meta,.foot{color:#6b7280;font-size:12px}
.err{color:#b42318;background:#fef3f2;border:1px solid #fecdc9;border-radius:8px;padding:8px 12px}
.empty{color:#6b7280}
.card{background:#fff;border:1px solid #e5e7eb;border-radius:12px;padding:16px;margin:16px 0}
.card header{display:flex;align-items:center;gap:8px;flex-wrap:wrap;margin-bottom:8px}
.badge{background:#eef2ff;color:#3730a3;border-radius:999px;padding:2px 10px;font-size:12px}
.total{margin:0 0 12px}
.total strong{font-size:22px}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:6px 8px;border-bottom:1px solid #f0f1f3;font-size:13px}
th{color:#6b7280;font-weight:500}
td.num{font-variant-numeric:tabular-nums}
`
