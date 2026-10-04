package priority_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	priority "github.com/Insulinocytus/cpa-auto-priority-plugin"
)

// Synthetic times in the upstream Kimi usages shape, not account captures.
// The pre-agreed seam is the complete Synchronizer.Sync round (issue #1).
func TestSyncKimiMonthlyOnlyAndShortWindowPrecedence(t *testing.T) {
	s := store(credential("month-only.json", "kimi"), credential("month-short.json", "kimi"), credential("earlier.json", "kimi"))
	for _, file := range s.files {
		s.metadata[file["name"].(string)] = `{"type":"kimi","base_url":"https://api.kimi.com/coding/v1","api_key":"private-kimi-key"}`
	}
	s.usage["month-only.json"] = []string{`{"usages":{"limit_month_total":{"used_ratio":0.1,"reset_time":"2026-10-20T00:00:00Z"}},"limits":[]}`}
	s.usage["month-short.json"] = []string{`{"usages":{"limit_month_total":{"used_ratio":0.9,"reset_time":"2026-10-20T00:00:00Z"}},"limits":[{"window":{"duration":5,"timeUnit":"TIME_UNIT_HOUR"},"detail":{"limit":100,"remaining":0,"reset_at":"2026-10-05T00:00:00Z"}}]}`}
	s.usage["earlier.json"] = []string{`{"usages":{"limit_month_total":{"used_ratio":0.5,"reset_time":"2026-10-19T00:00:00Z"}},"limits":[{"window":{"duration":5,"timeUnit":"TIME_UNIT_HOUR"},"detail":{"limit":100,"remaining":100,"reset_at":"2026-10-25T00:00:00Z"}}]}`}
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 1, 2}
	for i, file := range s.files {
		if file["priority"] != want[i] || round.Results[i].Priority != want[i] || round.Results[i].QueryStatus != "ok" || round.Results[i].WriteStatus != "acknowledged" {
			t.Fatalf("monthly precedence or confirmed missing layer: store=%v round=%+v", s.files, round)
		}
	}
}

func TestSyncKimiMalformedPeriodDoesNotBecomeMissingLayer(t *testing.T) {
	s := store(credential("broken.json", "kimi"), credential("healthy.json", "kimi"))
	for _, file := range s.files {
		s.metadata[file["name"].(string)] = `{"type":"kimi"}`
	}
	s.usage["broken.json"] = []string{`{"usage":{"window":[],"reset_at":"2026-10-10T00:00:00Z"},"usages":{"limit_month_total":{"reset_time":"2026-10-20T00:00:00Z"}}}`}
	s.usage["healthy.json"] = []string{`{"usages":{"limit_month_total":{"reset_time":"2026-10-20T00:00:00Z"}}}`}
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.files[0]["priority"] != -1 || s.files[1]["priority"] != 0 || s.queryCount["broken.json"] != 2 || round.Results[0].QueryStatus != "quota_response_malformed" {
		t.Fatalf("malformed period must fail only this auth: store=%v round=%+v", s.files, round)
	}
}

func TestSyncKimiDoesNotUseMoonshotBalanceOrForeignCredentials(t *testing.T) {
	for _, metadata := range []string{
		`{"type":"kimi","base_url":"https://api.moonshot.cn/v1","api_key":"moonshot-secret"}`,
		`{"type":"codex","domain":"ai","access_token":"foreign-secret"}`,
	} {
		s := store(credential("unsupported.json", "kimi"))
		s.metadata["unsupported.json"] = metadata
		round, err := synchronizer(t, s).Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if s.files[0]["priority"] != -1 || len(s.queryCount) != 0 || round.Results[0].QueryStatus != "unsupported_quota_auth" {
			t.Fatalf("foreign auth must not reach quota endpoint: store=%v round=%+v", s.files, round)
		}
	}
}

func TestSyncKimiCrossPlanTiesAndRelativeObservation(t *testing.T) {
	for _, order := range [][]string{{"month", "weekly", "short", "tie"}, {"tie", "short", "weekly", "month"}} {
		s := store()
		for _, name := range order {
			s.files = append(s.files, credential(name+".json", "kimi"))
			s.metadata[name+".json"] = `{"type":"kimi","domain":"ai","access_token":"private-token"}`
			s.expectedURL[name+".json"] = "https://api.kimi.ai/coding/v1/usages"
		}
		s.usage["month.json"] = []string{`{"limits":[{"window":{"duration":30,"timeUnit":"TIME_UNIT_DAY"},"detail":{"reset_at":"2027-01-01T00:00:00Z"}},{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"ttl":7200}}]}`}
		s.usage["weekly.json"] = []string{`{"usage":{"resetTime":"2026-12-31T19:00:00-05:00"}}`}
		s.usage["short.json"] = []string{`{"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"reset_in":"3600"}}]}`}
		s.usage["tie.json"] = []string{`{"limits":[{"duration":7,"timeUnit":"TIME_UNIT_DAY","reset_at":1798761600000}]}`}
		now := time.Date(2026, 12, 31, 22, 0, 0, 0, time.UTC)
		p, err := priority.New(priority.Config{ManagementURL: "http://127.0.0.1:8317", ManagementKey: s.key}, &http.Client{Transport: s}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			hour int
			want map[string]int
		}{
			{22, map[string]int{"month.json": 1, "weekly.json": 0, "tie.json": 0, "short.json": 2}},
			{23, map[string]int{"month.json": 1, "weekly.json": 0, "tie.json": 0, "short.json": 0}},
		} {
			now = time.Date(2026, 12, 31, tc.hour, 0, 0, 0, time.UTC)
			round, err := p.Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for i, file := range s.files {
				if file["priority"] != tc.want[file["name"].(string)] || round.Results[i].QueryStatus != "ok" {
					t.Fatalf("cross-plan rank or stale relative time: store=%v round=%+v", s.files, round)
				}
			}
		}
	}
}

// Synthetic payloads in the CPA adapter's credits shape (tests/xaiQuotaUnavailable
// fixture uses USAGE_PERIOD_TYPE_WEEKLY); billing-only fields must stay unsortable.
func TestSyncXaiWeeklyQuotaIsDistinctFromBillingAndHealth(t *testing.T) {
	s := store(credential("early.json", "xai"), credential("late.json", "xai"), credential("monthly-bill.json", "xai"), credential("bill-only.json", "xai"), credential("api-key.json", "xai"), credential("codex.json", "codex"))
	oauth := `{"type":"xai","auth_kind":"oauth","access_token":"xai-secret","sub":"user-%s"}`
	for _, name := range []string{"early", "late", "monthly-bill", "bill-only"} {
		s.metadata[name+".json"] = fmt.Sprintf(oauth, name)
		s.expectedUserID[name+".json"] = "user-" + name
	}
	s.metadata["api-key.json"] = `{"type":"xai","api_key":"xai-api-secret","base_url":"https://api.x.ai/v1"}`
	s.usage["early.json"] = []string{`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-01T00:00:00.000000+00:00","end":"2026-10-08T00:00:00.000000+00:00"},"creditUsagePercent":100,"billingPeriodEnd":"2026-10-05T00:00:00Z"}}`}
	s.usage["late.json"] = []string{`{"config":{"current_period":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-10-09T00:00:00Z"},"credit_usage_percent":0}}`}
	s.usage["monthly-bill.json"] = []string{`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","end":"2026-10-06T00:00:00Z"},"monthlyLimit":{"val":1000},"used":{"val":10}}}`}
	s.usage["bill-only.json"] = []string{`{"config":{"billingPeriodEnd":"2026-10-05T00:00:00Z","prepaidBalance":{"val":500},"subscriptionEnd":"2026-10-05T00:00:00Z","productUsage":[{"product":"chat","usagePercent":5}]}}`}
	s.usage["codex.json"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2027-01-01T00:00:00Z"}}}`}
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		priority int
		query    string
	}{"early.json": {1, "ok"}, "late.json": {0, "ok"}, "monthly-bill.json": {-1, "no_reset_time"}, "bill-only.json": {-1, "no_reset_time"}, "api-key.json": {-1, "unsupported_quota_auth"}, "codex.json": {0, "ok"}}
	for i, file := range s.files {
		name := file["name"].(string)
		if file["priority"] != want[name].priority || round.Results[i].QueryStatus != want[name].query || round.Results[i].WriteStatus != "acknowledged" {
			t.Fatalf("xai quota mixed with billing: store=%v round=%+v", s.files, round)
		}
	}
	if s.queryCount["api-key.json"] != 0 || s.queryCount["monthly-bill.json"] != 1 || s.selectedProxy != "socks5://account-proxy" {
		t.Fatalf("health probe, retry or proxy semantics violated: %v", s.queryCount)
	}
	encoded, _ := json.Marshal(round)
	for _, secret := range []string{"xai-secret", "xai-api-secret", "management-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("result leaks secret")
		}
	}
}

func TestSyncNewProviderFailuresOnlyAffectTheirAuth(t *testing.T) {
	duplicate := credential("same.json", "kimi")
	duplicate["id"] = "other-same"
	duplicate["auth_index"] = "same-other"
	s := store(credential("xai-down.json", "xai"), credential("xai-revoked.json", "xai"), credential("kimi-no-metadata.json", "kimi"), credential("same.json", "kimi"), duplicate, credential("recovered.json", "kimi"), credential("codex.json", "codex"))
	for _, name := range []string{"xai-down.json", "xai-revoked.json"} {
		s.metadata[name] = `{"type":"xai","auth_kind":"oauth"}`
		s.usage[name] = []string{`{"error":"secret-upstream-body"}`}
	}
	s.upstreamStatus["xai-down.json"] = 503
	s.upstreamStatus["xai-revoked.json"] = 401
	s.metadata["recovered.json"] = `{"type":"kimi"}`
	s.usage["recovered.json"] = []string{`not-json`, `{"usage":{"reset_in":60}}`}
	s.usage["codex.json"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2027-01-01T00:00:00Z"}}}`}
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"xai-down.json": "upstream_http_failed", "xai-revoked.json": "credentials_invalid", "kimi-no-metadata.json": "auth_metadata_unavailable", "same.json": "auth_metadata_ambiguous", "recovered.json": "ok", "codex.json": "ok"}
	for i, file := range s.files {
		name := file["name"].(string)
		priority := -1
		if want[name] == "ok" {
			priority = 0
		}
		if file["priority"] != priority || round.Results[i].QueryStatus != want[name] || round.Results[i].WriteStatus != "acknowledged" {
			t.Fatalf("failure crossed auth boundary: store=%v round=%+v", s.files, round)
		}
	}
	if s.queryCount["xai-down.json"] != 2 || s.queryCount["xai-revoked.json"] != 1 || s.metadataCount["kimi-no-metadata.json"] != 2 || s.metadataCount["same.json"] != 0 || s.queryCount["recovered.json"] != 2 {
		t.Fatalf("retry rule violated: queries=%v metadata=%v", s.queryCount, s.metadataCount)
	}
	if encoded, _ := json.Marshal(round); strings.Contains(string(encoded), "secret-upstream-body") {
		t.Fatal("result leaks upstream body")
	}
}

func TestSyncKimiAliasesShareHostGroupAndIgnoreTimelessRows(t *testing.T) {
	s := store(credential("dash.json", "kimi-ai"), credential("dot.json", "kimi.ai"), credential("com.json", "kimi"))
	for _, file := range s.files {
		s.metadata[file["name"].(string)] = `{}`
	}
	s.usage["dash.json"] = []string{`{"usages":{"limit_month_total":{"reset_time":"2026-10-20T00:00:00Z"}}}`}
	s.usage["dot.json"] = []string{`{"limits":[{"limit":100,"used":1}],"usages":{"limit_month_total":{"reset_time":"2026-10-19T00:00:00Z"}}}`}
	s.usage["com.json"] = []string{`{"usages":{"limit_month_total":{"reset_time":"2026-10-30T00:00:00Z"}}}`}
	s.expectedURL["dash.json"] = "https://api.kimi.ai/coding/v1/usages"
	s.expectedURL["dot.json"] = "https://api.kimi.ai/coding/v1/usages"
	s.expectedURL["com.json"] = "https://api.kimi.com/coding/v1/usages"
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.files[0]["priority"] != 0 || s.files[1]["priority"] != 1 || s.files[2]["priority"] != 0 || round.Results[1].QueryStatus != "ok" {
		t.Fatalf("host alias grouping or timeless row: store=%v round=%+v", s.files, round)
	}
}
