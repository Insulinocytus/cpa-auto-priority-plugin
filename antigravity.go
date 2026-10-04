package priority

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// The official management UI tries these in order within one fetch (a third,
// production URL follows). Each attempt here is one request, so the two-attempt
// budget covers only the first two; no further fallback is stacked on top.
var antigravityQuotaURLs = [queryAttempts]string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// Only the window spellings the official UI maps to a period are proven.
var antigravityPeriods = map[string]int64{
	"5h": 5 * 3600, "five-hour": 5 * 3600, "five_hour": 5 * 3600,
	"weekly": 7 * 86400, "week": 7 * 86400,
}

func (s *Synchronizer) antigravity(ctx context.Context, file authFile, attempt int) ([]time.Time, string) {
	project := strings.TrimSpace(file.ProjectID)
	if project == "" {
		return nil, "missing_project_id"
	}
	body, _ := json.Marshal(struct {
		Project string `json:"project"`
	}{project})
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "User-Agent": "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"}
	payload, status := s.upstream(ctx, apiCall{AuthIndex: file.Index, Method: "POST", URL: antigravityQuotaURLs[attempt], Header: headers, Data: string(body)})
	if status != "ok" {
		return nil, status
	}
	// remainingFraction is quota amount, not a ranking input; the response
	// Date header only corrects display clocks and is never a reset time.
	var summary struct {
		Groups *[]struct {
			Buckets []struct {
				Window     string          `json:"window"`
				ResetTime  json.RawMessage `json:"resetTime"`
				ResetSnake json.RawMessage `json:"reset_time"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if json.Unmarshal(payload, &summary) != nil || summary.Groups == nil {
		return nil, "quota_response_malformed"
	}
	// Groups are model families sharing account periods. Equal resets in one
	// period merge; differing ones, or a family with no reset where another has
	// one, have no verified representative.
	periods := quotaPeriods{}
	untimed := map[int64]bool{}
	buckets := 0
	for _, group := range *summary.Groups {
		for _, bucket := range group.Buckets {
			buckets++
			// Official UI: resetTime ?? reset_time, so only absence falls back.
			raw := bucket.ResetTime
			if len(raw) == 0 || isNull(raw) {
				raw = bucket.ResetSnake
			}
			period, known := antigravityPeriods[strings.ToLower(strings.TrimSpace(bucket.Window))]
			var text string
			if len(raw) == 0 || isNull(raw) || (json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) == "") {
				if known {
					untimed[period] = true
				}
				continue
			}
			if !known {
				return nil, "quota_period_unverified"
			}
			reset, err := instant(raw)
			if err != nil {
				return nil, "quota_response_malformed"
			}
			if !periods.add(period, reset) {
				return nil, "quota_period_ambiguous"
			}
		}
	}
	if buckets == 0 {
		// The official UI moves on to its next endpoint for an empty summary.
		return nil, "no_quota_groups"
	}
	for period := range untimed {
		if _, timed := periods[period]; timed {
			return nil, "quota_period_ambiguous"
		}
	}
	if len(periods) == 0 {
		return nil, "no_reset_time"
	}
	return periodSequence(periods), "ok"
}
