package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Upstream throttling is per model, so the backoff is too. A 429 on one id
// used to hide every model the credential serves, which looked exactly like a
// dead account: the panel showed an empty model list while hy3 happily
// answered. Now only the throttled id steps aside, and only while another
// credential we have seen can still serve it.
//
// The host only ever hands a plugin one credential per request, so dropping an
// id from the advertised list is the mechanism available here: the host drops
// it from the model registry (see registerResolvedModelsForAuth) and picks a
// credential that still publishes the same id.

const (
	cooldownBase          = 60 * time.Second
	cooldownMax           = 30 * time.Minute
	// cooldownResetMax caps a wait derived from an upstream "reset at"
	// timestamp. Those can be hours away, which is longer than we are willing
	// to hide a model for: upstream extends and lifts these windows freely.
	cooldownResetMax      = 6 * time.Hour
	cooldownSeenStaleness = 2 * time.Hour
)

var (
	cdMu     sync.Mutex
	cooling  = map[string]cooldownState{}
	lastSeen = map[string]time.Time{}
)

type cooldownState struct {
	until    time.Time
	failures int
	model    string
}

// noteIdentity records that a credential exists, so we can tell whether any
// other credential is still able to serve a model before we suppress one.
func noteIdentity(id string) {
	cdMu.Lock()
	lastSeen[id] = time.Now()
	cdMu.Unlock()
}

// cooldownKey namespaces one model's backoff under its credential.
func cooldownKey(credential, model string) string {
	return credential + "\x00" + model
}

// cooldownModel reduces a requested id to the bare upstream id. Clients may
// send the routed form ("global/hy3"); upstream and the catalog only know
// "hy3".
func cooldownModel(model string) string {
	return strings.TrimSpace(stripModelPrefix(strings.TrimSpace(model)))
}

// markCooldown backs off one model on one credential.
//
// An explicit hint from upstream wins: Retry-After first, then the reset
// timestamp the 429 body carries, and only then our own exponential guess.
// Repeated failures grow the guess, never the hint.
func markCooldown(sa *storedAuth, resp *http.Response, model string, body []byte) {
	id := cooldownModel(model)
	wait := time.Duration(0)
	if resp != nil {
		if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
		}
	}
	if wait <= 0 {
		if reset, ok := parseResetAt(body); ok {
			if d := time.Until(reset); d > 0 {
				wait = d
			}
		}
	}

	credential := accountIdentity(sa)
	key := cooldownKey(credential, id)

	cdMu.Lock()
	st := cooling[key]
	st.failures++
	st.model = id
	if wait <= 0 {
		shift := st.failures - 1
		if shift > 5 {
			shift = 5
		}
		wait = cooldownBase
		for i := 0; i < shift; i++ {
			wait *= 2
		}
		if wait > cooldownMax {
			wait = cooldownMax
		}
	} else if wait > cooldownResetMax {
		wait = cooldownResetMax
	}
	st.until = time.Now().Add(wait)
	cooling[key] = st
	cdMu.Unlock()

	fields := map[string]any{
		"uid":      sa.Account.UID,
		"model":    id,
		"seconds":  int(wait.Seconds()),
		"failures": st.failures,
	}
	if resp != nil {
		fields["status"] = resp.StatusCode
	}
	if reason := upstreamReason(body); reason != "" {
		fields["reason"] = reason
	}
	hostLog("warn", "workbuddy: model cooling down after upstream backoff", fields)
}

// clearCooldown forgives one model once it has served a request again. Other
// ids keep whatever backoff they earned: a 429 names one model, not the
// account.
func clearCooldown(sa *storedAuth, model string) {
	id := cooldownModel(model)
	key := cooldownKey(accountIdentity(sa), id)
	cdMu.Lock()
	if st, ok := cooling[key]; ok && st.failures > 0 {
		delete(cooling, key)
		cdMu.Unlock()
		return
	}
	cdMu.Unlock()
}

// cooldownRemaining reports how long one model on this credential is still
// backed off for.
func cooldownRemaining(credential, model string) time.Duration {
	cdMu.Lock()
	defer cdMu.Unlock()
	st, ok := cooling[cooldownKey(credential, model)]
	if !ok {
		return 0
	}
	return time.Until(st.until)
}

// suppressModel reports whether this credential should stop advertising one
// model id because upstream throttled it.
//
// It only suppresses when some other recently seen credential is not backed
// off for the same id. If nobody else can serve it, hiding the model would
// turn a real upstream error into a confusing "unknown model", so in that case
// we keep advertising and let the error surface.
func suppressModel(sa *storedAuth, id string) bool {
	credential := accountIdentity(sa)
	if cooldownRemaining(credential, id) <= 0 {
		return false
	}
	cdMu.Lock()
	defer cdMu.Unlock()
	cutoff := time.Now().Add(-cooldownSeenStaleness)
	for other, seen := range lastSeen {
		if other == credential || seen.Before(cutoff) {
			continue
		}
		if st, ok := cooling[cooldownKey(other, id)]; ok && time.Now().Before(st.until) {
			continue
		}
		return true
	}
	return false
}

// resetAtPattern matches the reset timestamp CodeBuddy puts in its 429 body:
// "your usage will reset at 2026-09-18 15:17:07 UTC+8".
var resetAtPattern = regexp.MustCompile(`reset at\s+(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})\s*(?:UTC([+-]?\d{1,2}))?`)

// parseResetAt reads the moment upstream says the limit lifts. The offset is
// part of the message, so the answer does not depend on the host clock's zone.
func parseResetAt(body []byte) (time.Time, bool) {
	if len(body) == 0 {
		return time.Time{}, false
	}
	m := resetAtPattern.FindSubmatch(body)
	if m == nil {
		return time.Time{}, false
	}
	stamp := strings.Replace(strings.TrimSpace(string(m[1])), "T", " ", 1)
	offsetHours := 8
	if len(m[2]) > 0 {
		if h, err := strconv.Atoi(string(m[2])); err == nil {
			offsetHours = h
		}
	}
	zone := time.FixedZone("upstream", offsetHours*3600)
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", stamp, zone)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// upstreamReason pulls the human-readable msg out of an upstream error body so
// a log line says why the model was throttled instead of just that it was.
func upstreamReason(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var envelope struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Msg != "" {
		return truncate(envelope.Msg, 200)
	}
	return truncate(string(body), 200)
}
