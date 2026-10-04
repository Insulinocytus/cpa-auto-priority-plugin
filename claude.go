package priority

import (
	"context"
	"encoding/json"
	"time"
)

func (s *Synchronizer) claude(ctx context.Context, file authFile) ([]time.Time, string) {
	periods, status := retryOnce(ctx, func() (quotaPeriods, string) { return s.claudeUsage(ctx, file) })
	if status != "ok" {
		return nil, status
	}
	_, status = retryOnce(ctx, func() (struct{}, string) { return struct{}{}, s.claudeCards(ctx, file, periods) })
	if status != "ok" {
		return nil, status
	}
	return ordered(periods)
}

func (s *Synchronizer) claudeRequest(ctx context.Context, file authFile, query string) ([]byte, string) {
	return s.upstream(ctx, file.Index, "GET", "https://api.anthropic.com/api/oauth/usage"+query, map[string]string{
		"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json",
		"User-Agent": "claude-cli/2.1.280 (external, cli)", "anthropic-beta": "oauth-2025-04-20",
	}, "")
}

func (s *Synchronizer) claudeUsage(ctx context.Context, file authFile) (quotaPeriods, string) {
	body, status := s.claudeRequest(ctx, file, "")
	if status != "ok" {
		return nil, status
	}
	payload, ok := jsonObject(body)
	if !ok {
		return nil, "quota_response_malformed"
	}
	// seven_day is the generic all-model quota. Model/use-specific weekly
	// constraints are not additional periods or a representative account window.
	periods := quotaPeriods{}
	for _, windowPeriod := range [...]struct {
		key      string
		duration int64
	}{{"seven_day", 604800}, {"five_hour", 18000}} {
		raw := payload[windowPeriod.key]
		if !nonNull(raw) {
			continue
		}
		window, ok := jsonObject(raw)
		if !ok {
			return nil, "quota_response_malformed"
		}
		if !nonNull(window["resets_at"]) {
			continue
		}
		var text string
		if json.Unmarshal(window["resets_at"], &text) != nil {
			return nil, "quota_response_malformed"
		}
		reset, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return nil, "quota_response_malformed"
		}
		periods[windowPeriod.duration] = reset
	}
	if len(periods) == 0 {
		return nil, "no_reset_time"
	}
	return periods, "ok"
}

func (s *Synchronizer) claudeCards(ctx context.Context, file authFile, periods quotaPeriods) string {
	body, status := s.claudeRequest(ctx, file, "?cedar_ember=1&skip_spend=1")
	if status != "ok" {
		return status
	}
	var payload struct {
		Cards *struct {
			Eligible *bool `json:"eligible"`
			Grants   *[]struct {
				ID     string          `json:"id"`
				Total  *int64          `json:"resets_total"`
				Left   *int64          `json:"resets_left"`
				Starts json.RawMessage `json:"starts_at"`
				Ends   json.RawMessage `json:"ends_at"`
				Clears *[]string       `json:"clears"`
				Paused bool            `json:"paused"`
				// Decode for type validation only, not entitlement filtering.
				UsableNow        bool `json:"usable_now"`
				UseRequiresLimit bool `json:"use_requires_limit"`
			} `json:"grants"`
		} `json:"cedar_ember"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Cards == nil || payload.Cards.Eligible == nil || payload.Cards.Grants == nil {
		return "reset_card_response_malformed"
	}
	observed := s.now()
	seen := make(map[string]bool, len(*payload.Cards.Grants))
	expiries := quotaPeriods{}
	for _, grant := range *payload.Cards.Grants {
		if grant.ID == "" || seen[grant.ID] || grant.Total == nil || grant.Left == nil || *grant.Total < 0 || *grant.Left < 0 || *grant.Left > *grant.Total {
			return "reset_card_response_malformed"
		}
		seen[grant.ID] = true
		start, startOK := claudeOptionalTime(grant.Starts)
		end, endOK := claudeOptionalTime(grant.Ends)
		if !startOK || !endOK || (!start.IsZero() && !end.IsZero() && !end.After(start)) {
			return "reset_card_response_malformed"
		}
		if !*payload.Cards.Eligible || grant.Paused || *grant.Left == 0 || start.After(observed) || (!end.IsZero() && !end.After(observed)) {
			continue
		}
		if grant.Clears == nil {
			return "reset_card_applicability_unknown"
		}
		for _, window := range *grant.Clears {
			var duration int64
			switch window {
			case "seven_day":
				duration = 604800
			case "five_hour":
				duration = 18000
			case "seven_day_overage_included":
				// A distinct overage allowance, not the generic seven_day quota.
				continue
			default:
				return "reset_card_applicability_unknown"
			}
			if previous := expiries[duration]; !end.IsZero() && (previous.IsZero() || end.Before(previous)) {
				expiries[duration] = end
			}
		}
	}
	// Validate the entire response before applying, so a retry never retains a
	// card from a malformed first response. Redemption gates (usable_now,
	// use_requires_limit, at_limit, cooldown_until) do not revoke held rights.
	for duration, expiry := range expiries {
		applyCardExpiry(periods, duration, expiry)
	}
	return "ok"
}

// The pinned card parser accepts absent/null bounds; only ISO strings are dates.
func claudeOptionalTime(raw json.RawMessage) (time.Time, bool) {
	if !nonNull(raw) {
		return time.Time{}, true
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return time.Time{}, false
	}
	value, err := time.Parse(time.RFC3339Nano, text)
	return value, err == nil
}
