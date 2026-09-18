package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
		if next, ok := nextFallbackModel(sa, tried); ok {
			hostLog("warn", "workbuddy: requested model is throttled, answering with another model", map[string]any{
				"uid":  sa.Account.UID,
				"from": model,
				"to":   next,
			})
			model = next
			tried[model] = struct{}{}
			body = setModelInBody(body, model)
		}
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
// Free models come first, because a fallback exists to keep a request alive,
// not to start billing the account. Models the catalog prices but does not
// describe as free are never chosen. Unknown pricing ranks between the two:
// most catalogs carry a multiplier, but a bare entry is more likely free than
// metered, and a request that fails has no cost either.
func nextFallbackModel(sa *storedAuth, tried map[string]struct{}) (string, bool) {
	catalog := cachedCatalog(sa)
	credential := accountIdentity(sa)
	var free, unknown []string
	for _, m := range catalog {
		id := strings.TrimSpace(m.ID)
		if id == "" || isServiceModel(id) {
			continue
		}
		if _, skip := tried[id]; skip {
			continue
		}
		if cooldownRemaining(credential, id) > 0 {
			continue
		}
		switch {
		case m.isFree():
			free = append(free, id)
		case strings.TrimSpace(m.Credits) == "":
			unknown = append(unknown, id)
		}
	}
	if len(free) > 0 {
		return free[0], true
	}
	if len(unknown) > 0 {
		return unknown[0], true
	}
	return "", false
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
