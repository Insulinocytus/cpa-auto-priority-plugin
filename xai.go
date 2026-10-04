package priority

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Grok CLI OAuth only. API keys have no verified natural-reset contract, and
// the official UI's paid-health generation is never a quota observation.
func xaiHeaders(metadata map[string]json.RawMessage) (map[string]string, string) {
	if kind := strings.ToLower(rawString(metadata["type"])); kind != "xai" || strings.ToLower(rawString(metadata["auth_kind"])) != "oauth" || nonNull(metadata["api_key"]) {
		return nil, "unsupported_quota_auth"
	}
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "x-xai-token-auth": "xai-grok-cli", "x-grok-client-version": "0.2.91", "accept": "*/*", "user-agent": "grok-pager/0.2.91 grok-shell/0.2.91 (macos; aarch64)"}
	// Same candidates as the official adapter; email/account never substitute.
	for _, key := range []string{"sub", "subject", "user_id", "userId"} {
		if id := rawString(metadata[key]); id != "" {
			headers["x-userid"] = id
			return headers, "ok"
		}
	}
	for _, nested := range []struct{ object, keys string }{{"oauth", "sub subject"}, {"user", "sub id"}} {
		if object, ok := jsonObject(metadata[nested.object]); ok {
			for _, key := range strings.Fields(nested.keys) {
				if id := rawString(object[key]); id != "" {
					headers["x-userid"] = id
					return headers, "ok"
				}
			}
		}
	}
	return headers, "ok"
}

func (s *Synchronizer) xai(ctx context.Context, file authFile, headers map[string]string) ([]time.Time, string) {
	payload, status := s.upstream(ctx, file.Index, "GET", "https://cli-chat-proxy.grok.com/v1/billing?format=credits", headers, "")
	if status != "ok" {
		return nil, status
	}
	root, valid := jsonObject(payload)
	if !valid {
		return nil, "quota_response_malformed"
	}
	config, valid := jsonObject(root["config"])
	if !valid {
		return nil, "quota_response_malformed"
	}
	raw := config["currentPeriod"]
	if !nonNull(raw) {
		raw = config["current_period"]
	}
	if !nonNull(raw) {
		return nil, "no_reset_time"
	}
	period, valid := jsonObject(raw)
	if !valid {
		return nil, "quota_response_malformed"
	}
	// Only the explicit usage week is a quota refresh. Monthly periods,
	// billingPeriodEnd, balances and subscription dates are billing data.
	if !strings.Contains(strings.ToLower(rawString(period["type"])), "weekly") || !nonNull(period["end"]) {
		return nil, "no_reset_time"
	}
	reset, err := instant(period["end"])
	if err != nil {
		return nil, "quota_response_malformed"
	}
	return []time.Time{reset}, "ok"
}
