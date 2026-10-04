package priority_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	priority "github.com/Insulinocytus/cpa-auto-priority-plugin"
)

const (
	claudeURL      = "https://api.anthropic.com/api/oauth/usage"
	claudeCardsURL = claudeURL + "?cedar_ember=1&skip_spend=1"
)

// Synthetic dates in the pinned UI's ClaudeUsagePayload shape; no account capture.
// Issue #4 agrees the complete Synchronizer.Sync seam, not private parsers.
func TestSyncClaudeGenericPeriodsAndIndependentCodexRanks(t *testing.T) {
	for _, names := range [][]string{{"week", "short", "tie", "earlier", "special-only", "token-only", "codex"}, {"codex", "token-only", "special-only", "earlier", "tie", "short", "week"}} {
		s := store()
		for _, name := range names {
			provider := "claude"
			if name == "codex" {
				provider = "codex"
			}
			s.files = append(s.files, credential(name, provider))
		}
		s.usage["week"] = []string{`{"seven_day":{"utilization":99,"resets_at":"2026-10-10T00:00:00Z"},"five_hour":null,"seven_day_opus":{"resets_at":"2026-10-05T00:00:00Z"},"seven_day_sonnet":{"resets_at":"2026-10-30T00:00:00Z"}}`}
		s.usage["short"] = []string{`{"seven_day":{"utilization":1,"resets_at":"2026-10-10T00:00:00Z"},"five_hour":{"utilization":0,"resets_at":"2026-10-06T00:00:00Z"},"seven_day_oauth_apps":{"resets_at":"2026-10-01T00:00:00Z"},"seven_day_cowork":{"resets_at":"2026-10-20T00:00:00Z"},"iguana_necktie":{"resets_at":"2026-10-03T00:00:00Z"},"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable 5"}},"percent":10,"resets_at":"2026-10-02T00:00:00Z"}]}`}
		s.usage["tie"] = []string{`{"seven_day":{"resets_at":"2026-10-09T19:00:00-05:00"},"five_hour":{"resets_at":"2026-10-06T00:00:00Z"}}`}
		s.usage["earlier"] = []string{`{"seven_day":{"resets_at":"2026-10-09T00:00:00Z"},"five_hour":{"resets_at":"2026-10-30T00:00:00Z"}}`}
		s.usage["special-only"] = []string{`{"seven_day":null,"seven_day_opus":{"resets_at":"2026-10-05T00:00:00Z"},"seven_day_overage_included":{"resets_at":"2026-10-05T00:00:00Z"},"extra_usage":{"is_enabled":true,"monthly_limit":10000}}`}
		s.usage["token-only"] = []string{`{"expires_at":"2026-10-05T00:00:00Z","subscription_end":"2026-10-05T00:00:00Z"}`}
		s.usage["codex"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2027-01-01T00:00:00Z"}}}`}
		round, err := synchronizer(t, s).Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"week": 0, "short": 1, "tie": 1, "earlier": 2, "special-only": -1, "token-only": -1, "codex": 0}
		for i, file := range s.files {
			name := file["name"].(string)
			query := "ok"
			if want[name] == -1 {
				query = "no_reset_time"
			}
			if file["priority"] != want[name] || round.Results[i].Priority != want[name] || round.Results[i].QueryStatus != query || round.Results[i].WriteStatus != "acknowledged" {
				t.Fatalf("generic scope, period precedence or provider isolation: store=%v round=%+v", s.files, round)
			}
		}
	}
}

func claudeGrant(id, expiry string, clears ...string) map[string]any {
	return map[string]any{"id": id, "resets_total": 3, "resets_left": 2, "starts_at": "2026-10-01T00:00:00Z", "ends_at": expiry, "clears": clears, "paused": false, "usable_now": false, "use_requires_limit": true}
}

func claudeCards(grants ...map[string]any) string {
	if grants == nil {
		grants = []map[string]any{}
	}
	body, _ := json.Marshal(map[string]any{"cedar_ember": map[string]any{"eligible": true, "at_limit": false, "weekly_resets_at": "2026-10-04T01:00:00Z", "cooldown_until": "2026-10-05T00:00:00Z", "grants": grants}})
	return string(body)
}

func TestSyncClaudeHeldCardsBeforeLimitAndPerPeriodPrecedence(t *testing.T) {
	s := store(credential("both", "claude"), credential("weekly", "claude"), credential("short", "claude"), credential("equal", "claude"), credential("late", "claude"), credential("missing-short", "claude"), credential("codex", "codex"))
	for _, file := range s.files {
		name := file["name"].(string)
		s.usage[name] = []string{`{"seven_day":{"utilization":0,"resets_at":"2026-10-10T00:00:00Z"},"five_hour":{"utilization":0,"resets_at":"2026-10-06T00:00:00Z"}}`}
	}
	s.usage["missing-short"] = []string{`{"seven_day":{"resets_at":"2026-10-10T00:00:00Z"}}`}
	s.usage["codex"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2027-01-01T00:00:00Z"}}}`}
	s.cards["both"] = []string{claudeCards(claudeGrant("later", "2026-10-07T00:00:00Z", "seven_day"), claudeGrant("soon", "2026-10-05T00:00:00Z", "seven_day", "five_hour"))}
	s.cards["weekly"] = []string{claudeCards(claudeGrant("week", "2026-10-05T00:00:00Z", "seven_day"))}
	s.cards["short"] = []string{claudeCards(claudeGrant("five", "2026-10-05T00:00:00Z", "five_hour"))}
	s.cards["equal"] = []string{claudeCards(claudeGrant("equal", "2026-10-10T00:00:00Z", "seven_day"))}
	s.cards["late"] = []string{claudeCards(claudeGrant("late", "2026-10-11T00:00:00Z", "seven_day", "five_hour"))}
	s.cards["missing-short"] = s.cards["short"]
	s.refresh = true
	before := make([]map[string]any, len(s.files))
	for i, file := range s.files {
		before[i] = map[string]any{}
		for k, v := range file {
			before[i][k] = v
		}
		before[i]["access_token"] = "concurrently-refreshed-token"
	}
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []int{4, 3, 2, 1, 1, 0, 0}
	for i, file := range s.files {
		before[i]["priority"] = want[i]
		if !reflect.DeepEqual(file, before[i]) || round.Results[i].Priority != want[i] || round.Results[i].QueryStatus != "ok" || round.Results[i].WriteStatus != "acknowledged" || round.Results[i].Persistence != "unverified" {
			t.Fatalf("held-card scope, precedence or narrow write: store=%v round=%+v", s.files, round)
		}
	}
}

func TestSyncClaudeCardFailuresAndRetryDoNotReusePartialExpiry(t *testing.T) {
	valid := claudeCards(claudeGrant("held", "2026-10-05T00:00:00Z", "seven_day"))
	partial := claudeGrant("bad", "sensitive-invalid-date", "seven_day")
	for _, tc := range []struct {
		name                       string
		bodies                     []string
		httpStatus, rank, attempts int
		query                      string
	}{
		{"empty", []string{claudeCards()}, 200, 0, 1, "ok"},
		{"missing-block", []string{`{}`}, 200, -1, 2, "reset_card_response_malformed"},
		{"missing-grants", []string{`{"cedar_ember":{"eligible":true}}`}, 200, -1, 2, "reset_card_response_malformed"},
		{"null-grants", []string{`{"cedar_ember":{"eligible":true,"grants":null}}`}, 200, -1, 2, "reset_card_response_malformed"},
		{"malformed", []string{`<html>sensitive-card-body</html>`}, 200, -1, 2, "reset_card_response_malformed"},
		{"http-failure", []string{`{"error":"sensitive-card-body"}`}, 503, -1, 2, "upstream_http_failed"},
		{"invalid-token", []string{valid}, 401, -1, 1, "credentials_invalid"},
		{"recovered", []string{`{}`, valid}, 200, 1, 2, "ok"},
		{"partial-then-empty", []string{claudeCards(claudeGrant("first", "2026-10-05T00:00:00Z", "seven_day"), partial), claudeCards()}, 200, 0, 2, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(credential("candidate", "claude"), credential("healthy", "claude"), credential("codex", "codex"))
			s.usage["candidate"], s.usage["healthy"] = []string{`{"seven_day":{"resets_at":"2026-10-10T00:00:00Z"}}`}, []string{`{"seven_day":{"resets_at":"2026-10-09T00:00:00Z"}}`}
			s.usage["codex"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2027-01-01T00:00:00Z"}}}`}
			s.cards["candidate"], s.cardStatus["candidate"] = tc.bodies, tc.httpStatus
			round, err := synchronizer(t, s).Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			healthyRank := 0
			if tc.rank == 0 {
				healthyRank = 1
			}
			if s.files[0]["priority"] != tc.rank || s.files[1]["priority"] != healthyRank || s.files[2]["priority"] != 0 || round.Results[0].QueryStatus != tc.query || s.cardQueryCount["candidate"] != tc.attempts || s.queryCount["candidate"] != 1 || s.cardQueryCount["healthy"] != 1 {
				t.Fatalf("card failure/retry isolation: store=%v round=%+v usage=%v cards=%v", s.files, round, s.queryCount, s.cardQueryCount)
			}
			encoded, _ := json.Marshal(round)
			for _, secret := range []string{"sensitive-card-body", "sensitive-invalid-date", "management-secret", "old-token"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatal("result leaks secret")
				}
			}
		})
	}
}

func TestSyncClaudeEntitlementValidityBoundariesAndUnknownScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		rank   int
		query  string
	}{
		{"active-at-start", func(g map[string]any) { g["starts_at"] = "2026-10-04T00:00:00Z" }, 1, "ok"},
		{"future", func(g map[string]any) { g["starts_at"] = "2026-10-05T00:00:00Z"; g["ends_at"] = "2026-10-06T00:00:00Z" }, 0, "ok"},
		{"expired-at-end", func(g map[string]any) { g["ends_at"] = "2026-10-04T00:00:00Z" }, 0, "ok"},
		{"expired", func(g map[string]any) { g["ends_at"] = "2026-10-03T00:00:00Z" }, 0, "ok"},
		{"paused", func(g map[string]any) { g["paused"] = true }, 0, "ok"},
		{"exhausted", func(g map[string]any) { g["resets_left"] = 0 }, 0, "ok"},
		{"no-expiry", func(g map[string]any) { g["ends_at"] = nil }, 0, "ok"},
		{"absent-expiry", func(g map[string]any) { delete(g, "ends_at") }, 0, "ok"},
		{"unbounded-start", func(g map[string]any) { g["starts_at"] = nil }, 1, "ok"},
		{"overage-not-generic", func(g map[string]any) { g["clears"] = []string{"seven_day_overage_included"} }, 0, "ok"},
		{"empty-scope", func(g map[string]any) { g["clears"] = []string{} }, 0, "ok"},
		{"monthly-unknown", func(g map[string]any) { g["clears"] = []string{"monthly"} }, -1, "reset_card_applicability_unknown"},
		{"missing-scope", func(g map[string]any) { delete(g, "clears") }, -1, "reset_card_applicability_unknown"},
		{"numeric-date", func(g map[string]any) { g["ends_at"] = 1798761600 }, -1, "reset_card_response_malformed"},
		{"bad-count", func(g map[string]any) { g["resets_left"] = 4 }, -1, "reset_card_response_malformed"},
		{"fractional-count", func(g map[string]any) { g["resets_left"] = 1.5 }, -1, "reset_card_response_malformed"},
		{"missing-count", func(g map[string]any) { delete(g, "resets_total") }, -1, "reset_card_response_malformed"},
		{"bad-pause", func(g map[string]any) { g["paused"] = "false" }, -1, "reset_card_response_malformed"},
		{"bad-usability", func(g map[string]any) { g["usable_now"] = "false" }, -1, "reset_card_response_malformed"},
		{"bad-limit-gate", func(g map[string]any) { g["use_requires_limit"] = 1 }, -1, "reset_card_response_malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(credential("candidate", "claude"), credential("natural", "claude"))
			s.usage["candidate"] = []string{`{"seven_day":{"resets_at":"2026-10-10T00:00:00Z"}}`}
			s.usage["natural"] = []string{`{"seven_day":{"resets_at":"2026-10-09T00:00:00Z"}}`}
			grant := claudeGrant("held", "2026-10-05T00:00:00Z", "seven_day")
			tc.mutate(grant)
			s.cards["candidate"] = []string{claudeCards(grant)}
			round, err := synchronizer(t, s).Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if s.files[0]["priority"] != tc.rank || round.Results[0].QueryStatus != tc.query {
				t.Fatalf("invalid entitlement handling: store=%v round=%+v", s.files, round)
			}
		})
	}
}

func TestSyncClaudeNextRoundObservesIneligibilityAndConsumption(t *testing.T) {
	s := store(credential("held", "claude"), credential("natural", "claude"))
	s.usage["held"] = []string{`{"seven_day":{"resets_at":"2026-10-10T00:00:00Z"}}`}
	s.usage["natural"] = []string{`{"seven_day":{"resets_at":"2026-10-09T00:00:00Z"}}`}
	grant := claudeGrant("held", "2026-10-05T00:00:00Z", "seven_day")
	s.cards["held"] = []string{claudeCards(grant)}
	p := synchronizer(t, s)
	for _, tc := range []struct {
		body string
		rank int
	}{
		{claudeCards(grant), 1},
		{strings.Replace(claudeCards(grant), `"eligible":true`, `"eligible":false`, 1), 0},
		{claudeCards(grant), 1},
		{strings.Replace(claudeCards(grant), `"resets_left":2`, `"resets_left":0`, 1), 0},
	} {
		s.cards["held"] = []string{tc.body}
		round, err := p.Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if s.files[0]["priority"] != tc.rank || round.Results[0].QueryStatus != "ok" {
			t.Fatalf("stale grant state: store=%v round=%+v", s.files, round)
		}
	}
}

func TestSyncClaudeUsageFailuresAreIsolatedFromCards(t *testing.T) {
	valid := `{"seven_day":{"resets_at":"2026-10-10T00:00:00Z"}}`
	for _, tc := range []struct {
		name                              string
		bodies                            []string
		httpStatus, attempts, cards, rank int
		query                             string
	}{
		{"recovered", []string{`{"seven_day":[]}`, valid}, 200, 2, 1, 0, "ok"},
		{"bad-date", []string{`{"seven_day":{"resets_at":"sensitive-bad-date"}}`}, 200, 2, 0, -1, "quota_response_malformed"},
		{"bad-window", []string{`{"seven_day":[],"five_hour":{"resets_at":"2026-10-05T00:00:00Z"}}`}, 200, 2, 0, -1, "quota_response_malformed"},
		{"invalid", []string{valid}, 401, 1, 0, -1, "credentials_invalid"},
		{"unavailable", []string{valid}, 503, 2, 0, -1, "upstream_http_failed"},
		{"no-natural", []string{`{"seven_day":null,"five_hour":{"resets_at":null}}`}, 200, 1, 0, -1, "no_reset_time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(credential("candidate", "claude"), credential("healthy", "claude"))
			s.usage["candidate"], s.upstreamStatus["candidate"] = tc.bodies, tc.httpStatus
			s.usage["healthy"] = []string{valid}
			round, err := synchronizer(t, s).Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if s.files[0]["priority"] != tc.rank || s.files[1]["priority"] != 0 || round.Results[0].QueryStatus != tc.query || s.queryCount["candidate"] != tc.attempts || s.cardQueryCount["candidate"] != tc.cards {
				t.Fatalf("usage failure isolation: store=%v round=%+v usage=%v cards=%v", s.files, round, s.queryCount, s.cardQueryCount)
			}
		})
	}
}

func TestSyncClaudeManagementAuthenticationStopsRound(t *testing.T) {
	for _, endpoint := range []string{claudeURL, claudeCardsURL} {
		for _, code := range []int{401, 403} {
			s := store(credential("candidate", "claude"), credential("unqueried", "codex"))
			s.usage["candidate"] = []string{`{"seven_day":{"resets_at":"2026-10-10T00:00:00Z"}}`}
			failures := 0
			client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/v8/management/requests/api-call" {
					data, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					req.Body = io.NopCloser(bytes.NewReader(data))
					var call struct {
						URL string `json:"url"`
					}
					if err := json.Unmarshal(data, &call); err != nil {
						return nil, err
					}
					if call.URL == endpoint {
						failures++
						return jsonResponse(code, map[string]any{"error": "sensitive-management-token"}), nil
					}
				}
				return s.RoundTrip(req)
			})}
			p, err := priority.New(priority.Config{ManagementURL: "http://127.0.0.1:8317", ManagementKey: s.key}, client, func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) })
			if err != nil {
				t.Fatal(err)
			}
			round, err := p.Sync(context.Background())
			if err != priority.ErrManagementAuthentication || round.Status != "failed" || failures != 1 || len(s.writeCount) != 0 || s.queryCount["unqueried"] != 0 || s.files[0]["priority"] != 9 || s.files[1]["priority"] != 9 {
				t.Fatalf("management refusal must stop the round: err=%v round=%+v", err, round)
			}
		}
	}
}
