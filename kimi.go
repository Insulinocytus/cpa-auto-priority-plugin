package priority

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strings"
	"time"
)

// The pinned host synthesizer treats these provider/type spellings as Kimi.
func isKimi(value string) bool {
	switch strings.ToLower(value) {
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
		return true
	}
	return false
}

func kimiDomain(value string) string {
	value = strings.ToLower(value)
	switch {
	case value == "ai", value == "kimi-ai", value == "kimi.ai", strings.HasSuffix(value, ".kimi.ai"):
		return "ai"
	case value == "com", value == "kimi", value == "kimi.com", strings.HasSuffix(value, ".kimi.com"):
		return "com"
	}
	return ""
}

func kimiURL(metadata map[string]json.RawMessage, file authFile) (string, string) {
	if nonNull(metadata["type"]) && !isKimi(rawString(metadata["type"])) {
		return "", "unsupported_quota_auth"
	}
	base, exists := metadata["base_url"]
	if !exists {
		base = metadata["base-url"]
	}
	if parsed, err := url.Parse(rawString(base)); err == nil && (parsed.Hostname() == "api.moonshot.cn" || parsed.Hostname() == "api.moonshot.ai") {
		return "", "unsupported_quota_auth"
	}
	domain := ""
	if explicit := rawString(metadata["domain"]); explicit != "" {
		domain = kimiDomain(explicit)
		if domain == "" {
			domain = "com" // Pinned host normalizes unknown explicit domains to com.
		}
	} else {
		if parsed, err := url.Parse(rawString(base)); err == nil {
			domain = kimiDomain(parsed.Hostname())
		}
		if domain == "" {
			domain = kimiDomain(rawString(metadata["type"]))
		}
		if domain == "" {
			domain = kimiDomain(file.Provider)
		}
	}
	if domain == "ai" {
		return "https://api.kimi.ai/coding/v1/usages", "ok"
	}
	return "https://api.kimi.com/coding/v1/usages", "ok"
}

func (s *Synchronizer) kimi(ctx context.Context, file authFile, endpoint string) ([]time.Time, string) {
	payload, status := s.quotaGET(ctx, file, endpoint, map[string]string{"Authorization": "Bearer $TOKEN$"})
	if status != "ok" {
		return nil, status
	}
	observed := s.now()
	root, valid := jsonObject(payload)
	if !valid {
		return nil, "quota_response_malformed"
	}
	periods := map[int64]time.Time{}
	addLimit := func(item map[string]json.RawMessage, fallback int64) string {
		detail := item
		if nonNull(item["detail"]) {
			var ok bool
			detail, ok = jsonObject(item["detail"])
			if !ok {
				return "quota_response_malformed"
			}
		}
		// Rows without a reset add no layer, so they need no period metadata.
		reset, found, ok := kimiReset(detail, observed)
		if !ok {
			return "quota_response_malformed"
		}
		if !found {
			return "ok"
		}
		duration, ok := kimiPeriod(item, detail, fallback)
		if !ok {
			return "quota_response_malformed"
		}
		if !found {
			return "ok"
		}
		if previous, exists := periods[duration]; exists && !previous.Equal(reset) {
			return "quota_period_ambiguous"
		}
		periods[duration] = reset
		return "ok"
	}
	if nonNull(root["limits"]) {
		var limits []json.RawMessage
		if json.Unmarshal(root["limits"], &limits) != nil {
			return nil, "quota_response_malformed"
		}
		for _, raw := range limits {
			item, ok := jsonObject(raw)
			if !ok {
				return nil, "quota_response_malformed"
			}
			if status := addLimit(item, 0); status != "ok" {
				return nil, status
			}
		}
	}
	if nonNull(root["usage"]) {
		usage, ok := jsonObject(root["usage"])
		if !ok {
			return nil, "quota_response_malformed"
		}
		// The legacy first-party CLI defines this summary as the weekly pool.
		// Explicit metadata still wins, and new monthly-only plans may omit it.
		if status := addLimit(usage, 7*86400); status != "ok" {
			return nil, status
		}
	}
	if nonNull(root["usages"]) {
		usages, ok := jsonObject(root["usages"])
		if !ok {
			return nil, "quota_response_malformed"
		}
		if nonNull(usages["limit_month_total"]) {
			month, ok := jsonObject(usages["limit_month_total"])
			if !ok {
				return nil, "quota_response_malformed"
			}
			if status := addLimit(month, monthLayer); status != "ok" {
				return nil, status
			}
		}
	}
	if len(periods) == 0 {
		return nil, "no_reset_time"
	}
	return periodSequence(periods), "ok"
}

func kimiPeriod(item, detail map[string]json.RawMessage, fallback int64) (int64, bool) {
	window := map[string]json.RawMessage{}
	if raw := bytes.TrimSpace(item["window"]); nonNull(raw) {
		if raw[0] == '{' {
			var ok bool
			window, ok = jsonObject(raw)
			if !ok {
				return 0, false
			}
		} else if _, ok := relativeSeconds(raw); !ok {
			return 0, false
		}
	}
	var duration, unit json.RawMessage
	for _, fields := range []map[string]json.RawMessage{window, item, detail} {
		if !nonNull(duration) && nonNull(fields["duration"]) {
			duration = fields["duration"]
		}
		if !nonNull(unit) && nonNull(fields["timeUnit"]) {
			unit = fields["timeUnit"]
		}
	}
	if nonNull(duration) {
		value, err := number(duration)
		factor := int64(0)
		switch strings.TrimPrefix(strings.ToUpper(rawString(unit)), "TIME_UNIT_") {
		case "SECOND", "SECONDS":
			factor = 1
		case "", "MINUTE", "MINUTES":
			factor = 60
		case "HOUR", "HOURS":
			factor = 3600
		case "DAY", "DAYS":
			factor = 86400
		case "WEEK", "WEEKS":
			factor = 7 * 86400
		}
		seconds := value * float64(factor)
		if err != nil || factor == 0 || seconds <= 0 || seconds != math.Trunc(seconds) || seconds >= float64(math.MaxInt64) {
			return 0, false
		}
		return quotaLayer(int64(seconds)), true
	}
	for _, fields := range []map[string]json.RawMessage{item, detail} {
		for _, key := range []string{"name", "title", "scope"} {
			switch strings.ToLower(rawString(fields[key])) {
			case "monthly", "monthly limit", "month":
				return monthLayer, true
			case "weekly", "weekly limit", "week":
				return 7 * 86400, true
			case "daily", "daily limit", "day":
				return 86400, true
			}
		}
	}
	return fallback, fallback > 0
}

func kimiReset(fields map[string]json.RawMessage, observed time.Time) (time.Time, bool, bool) {
	for _, key := range []string{"reset_at", "resetAt", "reset_time", "resetTime"} {
		if raw := fields[key]; nonNull(raw) {
			reset, err := instant(raw)
			return reset, true, err == nil
		}
	}
	for _, key := range []string{"reset_in", "resetIn", "ttl", "window"} {
		raw := bytes.TrimSpace(fields[key])
		if !nonNull(raw) || raw[0] == '{' {
			continue
		}
		seconds, ok := relativeSeconds(raw)
		if !ok {
			return time.Time{}, true, false
		}
		return observed.Add(seconds), true, true
	}
	return time.Time{}, false, true
}
