package priority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"time"
)

func (s *Synchronizer) codex(ctx context.Context, file authFile) ([]time.Time, string) {
	var periods map[int64]time.Time
	var status string
	for range 2 {
		periods, status = s.codexUsage(ctx, file)
		if !retryQuery(ctx, status) {
			break
		}
	}
	if status != "ok" {
		return nil, status
	}
	for range 2 {
		status = s.codexCards(ctx, file, periods)
		if !retryQuery(ctx, status) {
			break
		}
	}
	if status != "ok" {
		return nil, status
	}
	return periodSequence(periods), "ok"
}

func retryQuery(ctx context.Context, status string) bool {
	return ctx.Err() == nil && status != "ok" && status != "no_reset_time" && status != "credentials_invalid" && status != ErrManagementAuthentication.Error()
}

func (s *Synchronizer) codexRequest(ctx context.Context, file authFile, path string) ([]byte, string) {
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "User-Agent": "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"}
	if path == "rate-limit-reset-credits" {
		headers["Accept"] = "application/json"
		headers["OpenAI-Beta"] = "codex-1"
		headers["Originator"] = "Codex Desktop"
	}
	if file.IDToken.AccountID != "" {
		headers["Chatgpt-Account-Id"] = file.IDToken.AccountID
	}
	return s.quotaGET(ctx, file, "https://chatgpt.com/backend-api/wham/"+path, headers)
}

func (s *Synchronizer) codexUsage(ctx context.Context, file authFile) (map[int64]time.Time, string) {
	body, status := s.codexRequest(ctx, file, "usage")
	if status != "ok" {
		return nil, status
	}
	observed := s.now()
	var usage struct {
		RateLimit json.RawMessage `json:"rate_limit"`
	}
	payload := bytes.TrimSpace(body)
	if len(payload) == 0 || payload[0] != '{' || json.Unmarshal(payload, &usage) != nil {
		return nil, "quota_response_malformed"
	}
	if len(usage.RateLimit) == 0 || isNull(usage.RateLimit) {
		return nil, "no_reset_time"
	}
	var limits struct {
		Primary   json.RawMessage `json:"primary_window"`
		Secondary json.RawMessage `json:"secondary_window"`
	}
	if json.Unmarshal(usage.RateLimit, &limits) != nil {
		return nil, "quota_response_malformed"
	}
	// Only the account-wide rate_limit is comparable. Code review and named
	// additional_rate_limits are separate uses/models, not extra account periods.
	periods := map[int64]time.Time{}
	for _, raw := range []json.RawMessage{limits.Primary, limits.Secondary} {
		if len(raw) == 0 || isNull(raw) {
			continue
		}
		var window struct {
			Duration json.RawMessage `json:"limit_window_seconds"`
			Reset    json.RawMessage `json:"reset_at"`
			After    json.RawMessage `json:"reset_after_seconds"`
		}
		if json.Unmarshal(raw, &window) != nil {
			return nil, "quota_response_malformed"
		}
		durationValue, durationErr := number(window.Duration)
		if durationErr != nil || durationValue <= 0 || durationValue != math.Trunc(durationValue) || durationValue >= float64(math.MaxInt64) {
			return nil, "quota_response_malformed"
		}
		duration := quotaLayer(int64(durationValue))
		var reset time.Time
		var err error
		if len(window.Reset) > 0 && !isNull(window.Reset) {
			reset, err = instant(window.Reset)
		} else if len(window.After) > 0 && !isNull(window.After) {
			after, ok := relativeSeconds(window.After)
			if !ok {
				return nil, "quota_response_malformed"
			}
			reset = observed.Add(after)
		} else {
			continue
		}
		if err != nil {
			return nil, "quota_response_malformed"
		}
		// No upstream contract explains distinct resets for duplicate generic
		// periods. Do not guess which constraint is representative.
		if previous, exists := periods[duration]; exists && !reset.Equal(previous) {
			return nil, "quota_period_ambiguous"
		}
		periods[duration] = reset
	}
	if len(periods) == 0 {
		return nil, "no_reset_time"
	}
	return periods, "ok"
}

func (s *Synchronizer) codexCards(ctx context.Context, file authFile, periods map[int64]time.Time) string {
	body, status := s.codexRequest(ctx, file, "rate-limit-reset-credits")
	if status != "ok" {
		return status
	}
	var payload struct {
		Available *int64 `json:"available_count"`
		Credits   *[]struct {
			Type    string          `json:"reset_type"`
			Status  string          `json:"status"`
			Granted json.RawMessage `json:"granted_at"`
			Expires json.RawMessage `json:"expires_at"`
		} `json:"credits"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Available == nil || *payload.Available < 0 || payload.Credits == nil {
		return "reset_card_response_malformed"
	}
	observed := s.now()
	var earliest time.Time
	available := int64(0)
	for _, credit := range *payload.Credits {
		if credit.Type == "" || credit.Status == "" {
			return "reset_card_response_malformed"
		}
		if credit.Status != "available" {
			continue
		}
		available++
		if credit.Type != "codex_rate_limits" {
			return "reset_card_applicability_unknown"
		}
		granted, err := instant(credit.Granted)
		if err != nil {
			return "reset_card_response_malformed"
		}
		// The backend's optional expiry means no expiration, not a sorting instant.
		if len(credit.Expires) == 0 || isNull(credit.Expires) {
			continue
		}
		expires, err := instant(credit.Expires)
		if err != nil || !expires.After(granted) {
			return "reset_card_response_malformed"
		}
		if granted.After(observed) || !expires.After(observed) {
			continue
		}
		if earliest.IsZero() || expires.Before(earliest) {
			earliest = expires
		}
	}
	if available < *payload.Available {
		return "reset_card_details_incomplete"
	}
	if available > *payload.Available {
		return "reset_card_response_malformed"
	}
	// OpenAI's full banked reset covers the generic weekly and five-hour
	// windows. No monthly or model-specific entitlement contract is published.
	// applicable_available_count is not a held-card count; no at-limit gate.
	applyCardExpiry(periods, 604800, earliest)
	applyCardExpiry(periods, 18000, earliest)
	return "ok"
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func number(raw json.RawMessage) (float64, error) {
	text := string(raw)
	if len(text) > 0 && text[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return 0, errors.New("invalid_time")
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, errors.New("invalid_time")
	}
	return value, nil
}

func instant(raw json.RawMessage) (time.Time, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed, nil
		}
	} else {
		text = string(raw)
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return time.Time{}, errors.New("invalid_time")
	}
	unit := int64(time.Second)
	if value >= 1e11 {
		unit = int64(time.Millisecond)
	}
	if value*float64(unit)/float64(time.Second) > 253402300799 {
		return time.Time{}, errors.New("invalid_time")
	}
	// The usual integral Unix value needs no arbitrary-precision allocation.
	if integer, err := strconv.ParseInt(text, 10, 64); err == nil {
		if unit == int64(time.Millisecond) {
			return time.UnixMilli(integer).UTC(), nil
		}
		return time.Unix(integer, 0).UTC(), nil
	}
	// Decimal/exponent values must retain the original digits: float64 epoch
	// seconds cannot represent nanosecond precision, even with FormatFloat.
	exact, ok := new(big.Rat).SetString(text)
	if !ok {
		return time.Time{}, errors.New("invalid_time")
	}
	if unit == int64(time.Millisecond) {
		exact.Quo(exact, big.NewRat(1000, 1))
	}
	var seconds, nanos big.Int
	seconds.QuoRem(exact.Num(), exact.Denom(), &nanos)
	nanos.Mul(&nanos, big.NewInt(int64(time.Second)))
	nanos.Quo(&nanos, exact.Denom())
	return time.Unix(seconds.Int64(), nanos.Int64()).UTC(), nil
}
