// Live quota probes (WP-4.3).
//
// The local ledger in quota.go is authoritative: it counts what this harness
// actually sent. Some providers additionally publish a running total, some do
// not. This file is an explicitly best-effort reader for the ones that do:
//
//   - there is no registered or standardized endpoint. We try
//     <base_url>/usage and then <base_url>/user/balance, both with the
//     provider's own credentials, and accept whatever numeric shape comes back.
//   - any non-2xx, timeout or unrecognizable body is an error. Callers must
//     treat a probe failure as "no live data" and keep the local ledger; a probe
//     must never block a model turn or zero out recorded usage.
//   - the probe does not look at core.mode. Egress policy belongs to the
//     command that asks for a probe, not to a read-only helper.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kintsugi/internal/config"
)

// LiveUsage is one provider-reported snapshot. Used/Limit mirror the local
// ledger's vocabulary (tokens consumed vs cap) so a caller can render them side
// by side; a balance-shaped endpoint has no "used", so it reports the available
// amount as Limit with Used = 0 and says so in Source.
type LiveUsage struct {
	Provider string // best effort: the base_url host (callers may overwrite with the configured name)
	Used     int64
	Limit    int64
	Reset    string // provider-reported reset, verbatim (RFC3339, period name, unix seconds…)
	Source   string // endpoint (+ "#shape") the numbers came from
}

// probeEndpoints are tried in order. Order matters only for reporting: the
// first endpoint that yields numbers wins.
var probeEndpoints = []string{"/usage", "/user/balance"}

// Probe asks a provider for its own usage numbers. Best effort by design: see
// the package comment for the contract callers must honor.
func Probe(ctx context.Context, p config.Provider, timeout time.Duration) (LiveUsage, error) {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		return LiveUsage{}, fmt.Errorf("live quota probe: provider has no base_url")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	label := base
	if u := parseHost(base); u != "" {
		label = u
	}

	key, keyErr := p.APIKey()

	var errs []error
	for _, ep := range probeEndpoints {
		usage, err := probeOne(cctx, p, base+ep, key)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", base+ep, err))
			if cctx.Err() != nil {
				break // deadline or cancel: the next endpoint cannot do better
			}
			continue
		}
		usage.Provider = label
		// Provenance: endpoint plus the shape we recognized in its body, e.g.
		// "https://api.x/usage#tokens".
		usage.Source = base + ep + "#" + usage.Source
		if keyErr != nil {
			// Numbers arrived despite a missing key (loopback / open
			// endpoint) — that is a fact worth keeping, not an error.
			usage.Source += " (no key set)"
		}
		return usage, nil
	}
	if keyErr != nil {
		errs = append(errs, keyErr)
	}
	if len(errs) == 0 {
		return LiveUsage{Provider: label}, fmt.Errorf("live quota probe %s: no endpoint answered", base)
	}
	return LiveUsage{Provider: label}, fmt.Errorf("live quota probe %s: %w", base, errors.Join(errs...))
}

// probeOne performs a single GET and tries to make sense of the body.
func probeOne(ctx context.Context, p config.Provider, url, key string) (LiveUsage, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return LiveUsage{}, err
	}
	// Same auth style as provider.setAuth (replicated on purpose: the provider
	// package does not export it and this file must not import the wire code).
	if key != "" {
		if p.Wire == "anthropic-messages" {
			req.Header.Set("x-api-key", key)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 0} // the context carries the deadline
	resp, err := client.Do(req)
	if err != nil {
		return LiveUsage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return LiveUsage{}, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(b)), 200))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return LiveUsage{}, err
	}
	return parseLiveUsage(body)
}

// parseLiveUsage accepts the shapes providers actually emit:
//
//	{"used": 12, "limit": 100, "reset": "2026-11-01"}
//	{"used_tokens": 12, "limit_tokens": 100, "resets_at": "..."}
//	{"balance": 88, "credits": 88}            (balance == what is left)
//
// Unknown fields are ignored; nested objects ({"data": {...}}) are searched one
// level deep.
func parseLiveUsage(body []byte) (LiveUsage, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return LiveUsage{}, fmt.Errorf("parse: %w", err)
	}
	fields := flattenObjects(doc)

	used, hasUsed := pickInt(fields, "used_tokens", "used", "tokens_used", "usage_tokens", "consumed_tokens", "usedtokens")
	limit, hasLimit := pickInt(fields, "limit_tokens", "limit", "tokens_limit", "quota", "total_tokens", "max_tokens", "credittotal")
	balance, hasBalance := pickInt(fields, "balance", "credits", "credit_balance", "remaining", "remaining_tokens", "available")
	reset := pickStr(fields, "resets_at", "reset", "resets", "reset_at", "period_end", "expires_at", "renew_at")

	out := LiveUsage{Reset: reset}
	switch {
	case hasUsed || hasLimit:
		out.Used, out.Limit = used, limit
		if !hasUsed && hasLimit && hasBalance {
			out.Used = limit - balance // balance-shaped body with a cap
		}
		out.Source = "tokens"
		if !hasUsed {
			out.Source = "tokens#limit-only"
		}
	case hasBalance:
		// No cap published: report what is left as the cap so the number is
		// not silently lost.
		out.Limit = balance
		out.Source = "balance"
	default:
		return LiveUsage{}, fmt.Errorf("parse: no usage or balance field in response")
	}
	if out.Limit == 0 && out.Used == 0 {
		return LiveUsage{}, fmt.Errorf("parse: usage fields are zero")
	}
	return out, nil
}

// flattenObjects returns every key/value pair from the top level plus one level
// of nested objects, in a stable order (outer keys first).
func flattenObjects(doc any) map[string]any {
	out := map[string]any{}
	add := func(m map[string]any) {
		for k, v := range m {
			if _, taken := out[k]; !taken {
				out[k] = v
			}
		}
	}
	if m, ok := doc.(map[string]any); ok {
		add(m)
		for _, key := range []string{"data", "usage", "quota", "account", "result", "limits", "balance"} {
			if nested, ok := m[key].(map[string]any); ok {
				add(nested)
			}
		}
	}
	return out
}

func pickInt(fields map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		v, ok := fields[k]
		if !ok {
			continue
		}
		if n, ok := toInt(v); ok {
			return n, true
		}
	}
	return 0, false
}

func toInt(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(t, ",", ""))
		if s == "" {
			return 0, false
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

func pickStr(fields map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := fields[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

func parseHost(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
