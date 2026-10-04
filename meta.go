package priority

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// metaDCA reads only dca_token from the persisted auth file. For Meta the host
// resolves $TOKEN$ to the LLM credential, which the quota endpoint rejects;
// the DCA token stays request-local and is never stored or reported.
func (s *Synchronizer) metaDCA(ctx context.Context, file authFile) (string, string) {
	var persisted struct {
		DCA string `json:"dca_token"`
	}
	if err := s.request(ctx, "GET", "credentials/download?name="+url.QueryEscape(file.Name), nil, &persisted); err != nil {
		return "", err.Error()
	}
	token := strings.TrimSpace(persisted.DCA)
	if len(token) <= len("dca:") || !strings.HasPrefix(token, "dca:") || strings.ContainsFunc(token, unicode.IsSpace) {
		return "", "missing_dca_token"
	}
	return token, "ok"
}

type metaWindow struct {
	Minutes json.RawMessage `json:"window_duration_mins"`
	Reset   json.RawMessage `json:"resets_at"`
}

func (s *Synchronizer) meta(ctx context.Context, file authFile, dca string) ([]time.Time, string) {
	headers := map[string]string{"Accept": "application/json", "Content-Type": "application/json", "Authorization": "Bearer " + dca, "x-api-version": "1.0.0"}
	payload, status := s.upstream(ctx, file.Index, "POST", "https://api.meta.ai/muse-code/key", headers, "{}")
	if status != "ok" {
		return nil, status
	}
	// Decode subs_usage alone: the body can carry api_key and PII.
	var response struct {
		Usage json.RawMessage `json:"subs_usage"`
	}
	if json.Unmarshal(payload, &response) != nil {
		return nil, "quota_response_malformed"
	}
	// A valid body without subs_usage (e.g. before the first request) is an
	// observation of unknown quota, neither zero remaining nor a reset.
	if len(response.Usage) == 0 || isNull(response.Usage) {
		return nil, "no_reset_time"
	}
	var usage struct {
		Window *metaWindow `json:"window"`
		Weekly *metaWindow `json:"weekly"`
	}
	if json.Unmarshal(response.Usage, &usage) != nil {
		return nil, "quota_response_malformed"
	}
	periods := map[int64]time.Time{}
	if usage.Weekly != nil && len(usage.Weekly.Reset) > 0 && !isNull(usage.Weekly.Reset) {
		reset, err := unixSeconds(usage.Weekly.Reset)
		if err != nil {
			return nil, "quota_response_malformed"
		}
		periods[7*86400] = reset
	}
	if usage.Window != nil && len(usage.Window.Reset) > 0 && !isNull(usage.Window.Reset) {
		reset, err := unixSeconds(usage.Window.Reset)
		// The rolling window's period is only known from its duration.
		minutes, minutesErr := number(usage.Window.Minutes)
		if err != nil || minutesErr != nil || minutes <= 0 || minutes != math.Trunc(minutes) || minutes >= float64(math.MaxInt64/60) {
			return nil, "quota_response_malformed"
		}
		if !addPeriod(periods, int64(minutes)*60, reset) {
			return nil, "quota_period_ambiguous"
		}
	}
	return ordered(periods)
}
