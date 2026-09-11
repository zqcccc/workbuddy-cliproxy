package main

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Credentials are backed off when upstream asks us to (HTTP 429). While a
// credential cools down it advertises no models, so the host routes requests
// to another credential that does offer the requested id instead of hammering
// the one that just refused.
//
// The host only ever hands a plugin one credential per request, so suppressing
// the model list is the mechanism available here: the host drops the auth from
// the model registry (see registerResolvedModelsForAuth) and picks a different
// one for the same model id.

const (
	cooldownBase          = 60 * time.Second
	cooldownMax           = 30 * time.Minute
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
}

// noteIdentity records that a credential exists, so we can tell whether any
// other credential is still able to serve a model before we suppress one.
func noteIdentity(id string) {
	cdMu.Lock()
	lastSeen[id] = time.Now()
	cdMu.Unlock()
}

// markCooldown backs off one credential. Repeated failures grow the wait, and
// an upstream Retry-After wins when present.
func markCooldown(sa *storedAuth, resp *http.Response) {
	wait := cooldownBase
	if resp != nil {
		if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
		}
	}
	id := accountIdentity(sa)

	cdMu.Lock()
	st := cooling[id]
	st.failures++
	shift := st.failures - 1
	if shift > 5 {
		shift = 5
	}
	for i := 0; i < shift; i++ {
		wait *= 2
	}
	if wait > cooldownMax {
		wait = cooldownMax
	}
	st.until = time.Now().Add(wait)
	cooling[id] = st
	cdMu.Unlock()

	hostLog("warn", "workbuddy: credential cooling down after upstream backoff", map[string]any{
		"uid":      sa.Account.UID,
		"seconds":  int(wait.Seconds()),
		"failures": st.failures,
	})
}

// clearCooldown forgives a credential once it has served a request again.
func clearCooldown(sa *storedAuth) {
	id := accountIdentity(sa)
	cdMu.Lock()
	st, ok := cooling[id]
	if ok && st.failures > 0 {
		delete(cooling, id)
		cdMu.Unlock()
		return
	}
	cdMu.Unlock()
}

// cooldownRemaining reports how long a credential is still backed off for.
func cooldownRemaining(id string) time.Duration {
	cdMu.Lock()
	defer cdMu.Unlock()
	st, ok := cooling[id]
	if !ok {
		return 0
	}
	return time.Until(st.until)
}

// suppressModels reports whether this credential should stop advertising
// models because it is cooling down.
//
// It only suppresses when some other recently seen credential is still
// healthy. If every credential we know about is cooling, hiding the models
// would turn a real upstream error into a confusing "unknown model", so in
// that case we keep advertising and let the error surface.
func suppressModels(sa *storedAuth) bool {
	id := accountIdentity(sa)
	if cooldownRemaining(id) <= 0 {
		return false
	}
	cdMu.Lock()
	defer cdMu.Unlock()
	cutoff := time.Now().Add(-cooldownSeenStaleness)
	anyHealthy := false
	for other, seen := range lastSeen {
		if other == id || seen.Before(cutoff) {
			continue
		}
		if st, ok := cooling[other]; ok && time.Now().Before(st.until) {
			continue
		}
		anyHealthy = true
		break
	}
	return anyHealthy
}
