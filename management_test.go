package main

import (
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// decodeEnvelope unwraps the {ok,result} envelope every handler returns.
func decodeEnvelope(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("handler failed: %+v", env.Error)
	}
	return env.Result
}

func TestManagementRegisterAdvertisesBalanceRoutes(t *testing.T) {
	raw, err := handleManagementRegister([]byte(`{"base_path":"/v0/management","resource_base_path":"/v0/resource/plugins/workbuddy"}`))
	if err != nil {
		t.Fatalf("handleManagementRegister: %v", err)
	}
	result := decodeEnvelope(t, raw)
	var resp pluginapi.ManagementRegistrationResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Resources) != 1 {
		t.Fatalf("resources = %d, want 1", len(resp.Resources))
	}
	if resp.Resources[0].Path != quotaResourcePath {
		t.Errorf("resource path = %q, want %q", resp.Resources[0].Path, quotaResourcePath)
	}
	if resp.Resources[0].Menu == "" {
		t.Error("resource needs a menu label to show up in the UI")
	}
	if len(resp.Routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(resp.Routes))
	}
	if !strings.HasPrefix(resp.Routes[0].Path, "/v0/management/") {
		t.Errorf("route path = %q, want it under /v0/management/", resp.Routes[0].Path)
	}
	// CN and Global are separate plugins registering separate routes; sharing
	// one path makes the host drop the lower-priority one.
	if !strings.Contains(resp.Routes[0].Path, providerName) {
		t.Errorf("route path = %q, want it scoped to provider %q", resp.Routes[0].Path, providerName)
	}
	if resp.Routes[0].Method != http.MethodGet {
		t.Errorf("route method = %q, want GET", resp.Routes[0].Method)
	}
}

func TestManagementRegisterKeepsCustomBasePath(t *testing.T) {
	if _, err := handleManagementRegister([]byte(`{"base_path":"/v0/console/"}`)); err != nil {
		t.Fatalf("handleManagementRegister: %v", err)
	}
	managementMu.Lock()
	got := managementBasePath
	managementMu.Unlock()
	if got != "/v0/console" {
		t.Errorf("managementBasePath = %q, want /v0/console", got)
	}
	// Restore the default so other tests are not order-dependent.
	managementMu.Lock()
	managementBasePath = "/v0/management"
	managementMu.Unlock()
}

func TestManagementHandleResourceRendersHTML(t *testing.T) {
	raw, err := handleManagement([]byte(`{"method":"GET","path":"/v0/resource/plugins/workbuddy/quota"}`))
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	result := decodeEnvelope(t, raw)
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Headers.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content type = %q, want text/html", ct)
	}
	if !strings.Contains(string(resp.Body), "<h1>"+html.EscapeString(quotaPageTitle())+"</h1>") {
		t.Errorf("body is not the balance page (want the %q heading)", quotaPageTitle())
	}
}

// TestQuotaMenuNameDistinguishesRealms is the regression test for "both
// plugins show a menu entry called 额度". CN and Global are separate .so
// files, so without the realm in the name the operator cannot tell the two
// sidebar entries apart.
func TestQuotaMenuNameDistinguishesRealms(t *testing.T) {
	// The realm must follow this binary, not a runtime config value.
	want := "workbuddy 额度"
	if normalizeRegion(buildRegion) == regionGlobal {
		want = "workbuddy 国际版额度"
	}
	if got := quotaMenuName(); got != want {
		t.Errorf("quotaMenuName() = %q, want %q for buildRegion %q", got, want, buildRegion)
	}
	if quotaPageTitle() != want {
		t.Errorf("quotaPageTitle() = %q, want it to match the menu entry %q", quotaPageTitle(), want)
	}
}

func TestPctOfClamps(t *testing.T) {
	cases := []struct {
		name        string
		left, total float64
		want        float64
	}{
		{"normal", 25, 100, 25},
		{"zero total", 5, 0, 0},
		{"over", 150, 100, 100},
		{"negative", -5, 100, 0},
	}
	for _, c := range cases {
		if got := pctOf(c.left, c.total); got != c.want {
			t.Errorf("%s: pctOf(%v,%v) = %v, want %v", c.name, c.left, c.total, got, c.want)
		}
	}
}

func TestManagementHandleManagementRouteReturnsJSON(t *testing.T) {
	raw, err := handleManagement([]byte(`{"method":"GET","path":"/v0/management/workbuddy/quota"}`))
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	result := decodeEnvelope(t, raw)
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ct := resp.Headers.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content type = %q, want application/json", ct)
	}
	var snapshot quotaSnapshot
	if err := json.Unmarshal(resp.Body, &snapshot); err != nil {
		t.Fatalf("body is not a quota snapshot: %v", err)
	}
	if snapshot.GeneratedAt.IsZero() {
		t.Error("snapshot timestamp is missing")
	}
}

func TestManagementHandleRejectsNonGET(t *testing.T) {
	raw, err := handleManagement([]byte(`{"method":"POST","path":"/v0/management/workbuddy/quota"}`))
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	result := decodeEnvelope(t, raw)
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestFormatCredits(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1165, "1165"},
		{0, "0"},
		{840.36, "840.36"},
		{100, "100"},
		{65.2, "65.20"},
	}
	for _, c := range cases {
		if got := formatCredits(c.in); got != c.want {
			t.Errorf("formatCredits(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRegistrationDeclaresManagementAPI(t *testing.T) {
	reg := wbRegistration()
	if !reg.Capabilities.ManagementAPI {
		t.Error("management_api must be set or the host never calls management.register")
	}
	// The host only routes the methods a plugin declares, so a capability
	// without its handler is a silent dead end.
	for _, method := range []string{"management.register", "management.handle"} {
		if _, err := handleMethod(method, []byte(`{}`)); err != nil {
			t.Errorf("%s: %v", method, err)
		}
	}
}
