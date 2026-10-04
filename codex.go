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

func (s *Synchronizer) codex(ctx context.Context, file authFile, _ int) ([]time.Time, string) {
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "User-Agent": "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"}
	if file.IDToken.AccountID != "" {
		headers["Chatgpt-Account-Id"] = file.IDToken.AccountID
	}
	payload, status := s.upstream(ctx, apiCall{AuthIndex: file.Index, Method: "GET", URL: "https://chatgpt.com/backend-api/wham/usage", Header: headers})
	if status != "ok" {
		return nil, status
	}
	observed := s.now()
	var usage struct {
		RateLimit json.RawMessage `json:"rate_limit"`
	}
	if json.Unmarshal(payload, &usage) != nil {
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
	periods := quotaPeriods{}
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
		duration := int64(durationValue)
		// Official UI classifies 28–31 day windows as the same monthly period.
		if duration >= 28*86400 && duration <= 31*86400 {
			duration = 31 * 86400
		}
		var reset time.Time
		var err error
		if len(window.Reset) > 0 && !isNull(window.Reset) {
			reset, err = instant(window.Reset)
		} else if len(window.After) > 0 && !isNull(window.After) {
			seconds, parseErr := number(window.After)
			if parseErr != nil || seconds < 0 || seconds >= float64(math.MaxInt64)/float64(time.Second) {
				return nil, "quota_response_malformed"
			}
			reset = observed.Add(time.Duration(seconds * float64(time.Second)))
		} else {
			continue
		}
		if err != nil {
			return nil, "quota_response_malformed"
		}
		if !periods.add(duration, reset) {
			return nil, "quota_period_ambiguous"
		}
	}
	if len(periods) == 0 {
		return nil, "no_reset_time"
	}
	return periods.sequence(), "ok"
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
