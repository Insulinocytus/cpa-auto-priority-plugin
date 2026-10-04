package priority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// quota is the single provider decision point: unknown providers are never
// queried, and every supported query needs the selected auth_index.
func (s *Synchronizer) quota(ctx context.Context, file authFile, uniqueName bool) ([]time.Time, string) {
	var query func() ([]time.Time, string)
	switch file.Provider {
	case "codex":
		if file.Index == "" {
			return nil, "missing_auth_index"
		}
		// Codex owns usage and card retries separately; never replay both.
		return s.codex(ctx, file)
	case "claude":
		if file.Index == "" {
			return nil, "missing_auth_index"
		}
		return s.claude(ctx, file)
	case "antigravity":
		if file.Index == "" {
			return nil, "missing_auth_index"
		}
		if strings.TrimSpace(file.ProjectID) == "" {
			return nil, "missing_project_id"
		}
		attempt := 0
		query = func() ([]time.Time, string) {
			sequence, status := s.antigravity(ctx, file, attempt)
			attempt++
			return sequence, status
		}
	case "devin":
		query = func() ([]time.Time, string) { return s.devin(ctx, file) }
	case "meta":
		var dca string
		query = func() ([]time.Time, string) { return s.meta(ctx, file, dca) }
		if file.Index != "" {
			// The DCA download is a separate necessary request; a succeeded
			// download is not repeated when only the quota call fails.
			var status string
			if dca, status = retryOnce(ctx, func() (string, string) { return s.metaDCA(ctx, file) }); status != "ok" {
				return nil, status
			}
		}
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com", "xai":
		if file.Index == "" {
			return nil, "missing_auth_index"
		}
		// Metadata download is by filename; never guess between duplicate names.
		if !uniqueName {
			return nil, "auth_metadata_ambiguous"
		}
		metadata, status := s.quotaMetadata(ctx, file)
		if status != "ok" {
			return nil, status
		}
		if isKimi(file.Provider) {
			endpoint, status := kimiURL(metadata, file)
			if status != "ok" {
				return nil, status
			}
			query = func() ([]time.Time, string) { return s.kimi(ctx, file, endpoint) }
		} else {
			headers, status := xaiHeaders(metadata)
			if status != "ok" {
				return nil, status
			}
			query = func() ([]time.Time, string) { return s.xai(ctx, file, headers) }
		}
	default:
		return nil, "unsupported_provider"
	}
	if file.Index == "" {
		return nil, "missing_auth_index"
	}
	return retryOnce(ctx, query)
}

// retryOnce repeats a failed necessary request once. Definitive outcomes and
// management authentication failures are never repeated.
func retryOnce[T any](ctx context.Context, query func() (T, string)) (T, string) {
	value, status := query()
	switch status {
	case "ok", "no_reset_time", "credentials_invalid", "missing_dca_token", ErrManagementAuthentication.Error():
		return value, status
	}
	if ctx.Err() != nil {
		return value, status
	}
	return query()
}

// optionalUnixSeconds reads a reset whose contract fixes the unit to Unix
// seconds, with no millisecond heuristic. Like the official parsers, absent,
// null and non-positive values are an unknown layer; non-numeric text is
// malformed rather than silently absent.
func optionalUnixSeconds(raw json.RawMessage) (time.Time, bool, error) {
	if len(raw) == 0 || isNull(raw) {
		return time.Time{}, false, nil
	}
	text := string(raw)
	if text[0] == '"' && json.Unmarshal(raw, &text) != nil {
		return time.Time{}, false, errors.New("invalid_time")
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return time.Time{}, false, errors.New("invalid_time")
	}
	if value <= 0 {
		return time.Time{}, false, nil
	}
	reset, err := unix(text, false)
	return reset, err == nil, err
}

// unix converts positive numeric text in Unix seconds, or in milliseconds at
// or above the official helper's 1e11 boundary when detection is requested.
func unix(text string, detectMilliseconds bool) (time.Time, error) {
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return time.Time{}, errors.New("invalid_time")
	}
	unit := int64(time.Second)
	if detectMilliseconds && value >= 1e11 {
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

// ordered lists reset instants from the longest quota period to the shortest.
func ordered(periods map[int64]time.Time) ([]time.Time, string) {
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

// Download is a filename lookup, unlike the ID-based narrow write. The caller
// rejects ambiguous names; tokens are always resolved from the selected index.
func (s *Synchronizer) quotaMetadata(ctx context.Context, file authFile) (map[string]json.RawMessage, string) {
	if strings.ContainsAny(file.Name, "/\\") || !strings.HasSuffix(strings.ToLower(file.Name), ".json") {
		return nil, "auth_metadata_unavailable"
	}
	var metadata map[string]json.RawMessage
	var err error
	for range 2 {
		err = s.request(ctx, "GET", "credentials/download?name="+url.QueryEscape(file.Name), nil, &metadata)
		if err == nil && metadata != nil {
			return metadata, "ok"
		}
		if err == ErrManagementAuthentication || ctx.Err() != nil {
			break
		}
	}
	if err == ErrManagementAuthentication {
		return nil, err.Error()
	}
	return nil, "auth_metadata_unavailable"
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return strings.TrimSpace(value)
}

// Official UI classifies 28–31 day windows as one monthly period.
const monthLayer = 31 * 86400

func quotaLayer(seconds int64) int64 {
	if seconds >= 28*86400 && seconds <= monthLayer {
		return monthLayer
	}
	return seconds
}

// relativeSeconds parses a non-negative countdown anchored by the caller.
func relativeSeconds(raw json.RawMessage) (time.Duration, bool) {
	seconds, err := number(raw)
	if err != nil || seconds < 0 || seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}

// upstream proxies one provider request through the selected auth. The host
// resolves $TOKEN$ and applies that auth's proxy; no proxy_url override is sent.
func (s *Synchronizer) upstream(ctx context.Context, index, method, url string, headers map[string]string, data string) ([]byte, string) {
	call := struct {
		AuthIndex string            `json:"auth_index"`
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Header    map[string]string `json:"header"`
		Data      string            `json:"data,omitempty"`
	}{index, method, url, headers, data}
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
	payload := bytes.TrimSpace([]byte(response.Body))
	return payload, "ok"
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	var value map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	return value, true
}

func nonNull(raw json.RawMessage) bool { return len(raw) > 0 && !isNull(raw) }
