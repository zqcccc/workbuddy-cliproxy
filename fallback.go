package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// Upstream throttles one model at a time, for hours at a stretch, and the host
// only ever hands a plugin one credential per request. Waiting the limit out is
// therefore not an option: the model has to be swapped for one that can answer
// now. That is what this file does.
//
// Advertising does the first half: a throttled id is dropped from the
// credential's model list so the host prefers a credential that still serves
// it (see cooldown.go). This is the second half — when the request lands here
// anyway, either because no other credential has been asked yet or because
// every one of them is throttled too, another model answers the same prompt
// instead of the request failing.

const (
	// fallbackAttempts caps how many models one request may try. Two spares is
	// enough to ride out a throttled id without turning a dead end into a long
	// chain of doomed calls.
	fallbackAttempts = 3
)

// openUpstream posts one chat request to upstream.
func openUpstream(sa *storedAuth, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequest(http.MethodPost, baseFor(sa.Region)+pathChat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, sa)
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	return resp, nil
}

// sendWithFallback sends body upstream and, whenever upstream throttles the
// model, retries with the next model the account's catalog offers.
//
// Every 429 parks the id in the cooldown map, so discovery stops advertising
// it and the host favours a credential that can still serve it. The caller
// gets the first response that is not a throttle, and the model that produced
// it, so it can clear that model's cooldown rather than some other one's.
func sendWithFallback(sa *storedAuth, requested string, body []byte) (*http.Response, string, error) {
	model := cooldownModel(requested)
	tried := map[string]struct{}{}
	reason := ""

	// A model we already parked is not worth a request: spend the attempt on
	// one that can still answer.
	if cooldownRemaining(accountIdentity(sa), model) > 0 {
		tried[model] = struct{}{}
		next, ok := nextFallbackModel(sa, tried)
		if !ok {
			// Nothing left to try, and upstream already told us to stop. Asking
			// again would only deepen the throttle: a retrying client can turn
			// one 429 into dozens.
			return nil, model, fmt.Errorf("upstream 429: %s",
				firstNonEmpty(cooldownReason(accountIdentity(sa), model), "throttled, no other model available"))
		}
		hostLog("warn", "workbuddy: requested model is throttled, answering with another model", map[string]any{
			"uid":  sa.Account.UID,
			"from": model,
			"to":   next,
		})
		model = next
		tried[model] = struct{}{}
		body = setModelInBody(body, model)
	}

	for attempt := 0; ; attempt++ {
		resp, err := openUpstream(sa, body)
		if err != nil {
			return nil, model, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, model, nil
		}
		payload, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		markCooldown(sa, resp, model, payload)
		reason = truncate(string(payload), 200)
		// The host reports only "empty_stream" for a refused stream, so the
		// 429 and its reason would otherwise never reach a log.
		hostLog("warn", "workbuddy: upstream refused the request", map[string]any{
			"uid":    sa.Account.UID,
			"model":  model,
			"status": resp.StatusCode,
			"body":   reason,
		})
		if attempt+1 >= fallbackAttempts {
			break
		}
		next, ok := nextFallbackModel(sa, tried)
		if !ok {
			break
		}
		hostLog("warn", "workbuddy: falling back to another model", map[string]any{
			"uid":  sa.Account.UID,
			"from": model,
			"to":   next,
		})
		tried[model] = struct{}{}
		model = next
		tried[model] = struct{}{}
		body = setModelInBody(body, model)
	}
	return nil, model, fmt.Errorf("upstream 429: %s", firstNonEmpty(reason, "throttled on every candidate model"))
}

// nextFallbackModel picks the next model this account can still use: one the
// catalog serves, that this request has not tried, and that is not cooling on
// this credential.
//
// Everything in the pool is believed to cost nothing — the catalog prices it
// at zero, leaves its price blank, or the operator published it by hand — and
// the pick rotates across all of it. Ranking those groups and always taking
// the top one focused every throttled request onto a single spare model and
// pushed that model over its own burst limit in turn. Spreading is worth more
// than the ranking. Models the catalog actually prices are never chosen: a
// fallback exists to keep a request alive, not to start billing the account.
func nextFallbackModel(sa *storedAuth, tried map[string]struct{}) (string, bool) {
	catalog := cachedCatalog(sa)
	credential := accountIdentity(sa)
	var free, unknown []string
	seen := map[string]struct{}{}
	for _, m := range catalog {
		id := strings.TrimSpace(m.ID)
		if id == "" || isServiceModel(id) {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if _, skip := tried[id]; skip {
			continue
		}
		if cooldownRemaining(credential, id) > 0 {
			continue
		}
		seen[id] = struct{}{}
		switch {
		case m.isFree():
			free = append(free, id)
		case strings.TrimSpace(m.Credits) == "":
			unknown = append(unknown, id)
		}
	}
	// Last resort: ids the operator published by hand under extra_models.
	// They are declared rather than discovered, and cost nothing while the
	// trial they were published for lasts.
	var declared []string
	for _, spec := range configuredExtraModels() {
		id := strings.TrimSpace(spec.ID)
		if id == "" {
			continue
		}
		if _, skip := tried[id]; skip {
			continue
		}
		if cooldownRemaining(credential, id) > 0 {
			continue
		}
		declared = append(declared, id)
	}

	pool := make([]string, 0, len(free)+len(declared)+len(unknown))
	pool = append(pool, free...)
	pool = append(pool, declared...)
	pool = append(pool, unknown...)
	if id := pickRotating(pool); id != "" {
		return id, true
	}
	return "", false
}

// fallbackCursor rotates the choice inside one tier.
var fallbackCursor uint64

// pickRotating spreads repeated fallbacks across a whole tier.
//
// Always taking the first entry focuses every throttled request onto one
// model, which is how a fallback ends up pushing its own substitute over the
// burst limit: upstream answered 429 for hy3, so every caller landed on
// hy4-preview-f until that one refused too. Spreading the load keeps each
// candidate under its own limit. A cursor rather than a random pick keeps the
// rotation even and the tests deterministic.
func pickRotating(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	n := atomic.AddUint64(&fallbackCursor, 1) - 1
	return candidates[int(n%uint64(len(candidates)))]
}

// setModelInBody rewrites the model id of an outgoing chat payload.
func setModelInBody(payload []byte, model string) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	obj["model"] = model
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}
