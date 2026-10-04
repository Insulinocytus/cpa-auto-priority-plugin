package priority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"
)

func (s *Synchronizer) codex(ctx context.Context, file authFile) ([]time.Time, string) {
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "User-Agent": "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"}
	if file.IDToken.AccountID != "" {
		headers["Chatgpt-Account-Id"] = file.IDToken.AccountID
	}
	call := struct {
		AuthIndex string            `json:"auth_index"`
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Header    map[string]string `json:"header"`
	}{file.Index, "GET", "https://chatgpt.com/backend-api/wham/usage", headers}
	var response struct {
		Status int    `json:"status_code"`
		Body   string `json:"body"`
	}
	if err := s.request(ctx, "POST", "requests/api-call", call, &response); err != nil {
		return nil, err.Error()
	}
	if response.Status == 401 {
		return nil, "credentials_invalid"
	}
	if response.Status < 200 || response.Status >= 300 {
		return nil, "upstream_http_failed"
	}
	observed := s.now()
	var usage struct {
		RateLimit json.RawMessage `json:"rate_limit"`
	}
	payload := bytes.TrimSpace([]byte(response.Body))
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
	durations := make([]int64, 0, len(periods))
	for duration := range periods {
		durations = append(durations, duration)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] > durations[j] })
	sequence := make([]time.Time, 0, len(durations))
	for _, duration := range durations {
		sequence = append(sequence, periods[duration])
	}
	return sequence, "ok"
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
	}
	value, err := number(raw)
	if err != nil || value <= 0 {
		return time.Time{}, errors.New("invalid_time")
	}
	if value >= 1e11 {
		value /= 1000
	}
	// Reject values outside RFC3339's positive Unix range before conversion.
	if value > 253402300799 {
		return time.Time{}, errors.New("invalid_time")
	}
	seconds, fraction := math.Modf(value)
	return time.Unix(int64(seconds), int64(math.Round(fraction*1e9))).UTC(), nil
}
