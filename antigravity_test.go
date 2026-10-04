package priority_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	dailyQuotaURL   = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	sandboxQuotaURL = "https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary"
)

func antigravity(name string) map[string]any {
	file := credential(name, "antigravity")
	file["project_id"] = "project-" + name
	return file
}

// Synthetic payloads in the official retrieveUserQuotaSummary shape
// (groups[].buckets[] with window/resetTime); group names follow the labels
// the management UI translates. Not captured from real accounts.
func TestSyncAntigravityGroupWindowsMergeAndProviderIsolation(t *testing.T) {
	s := store(antigravity("merged"), antigravity("same-times-other-amounts"), antigravity("weekly-only"), antigravity("earliest"), antigravity("conflicting-groups"), antigravity("no-project"), credential("codex", "codex"))
	delete(s.files[5], "project_id")
	s.usage["merged"] = []string{`{"groups":[
		{"displayName":"Gemini Models","description":"Models within this group: gemini-3-pro","buckets":[
			{"bucketId":"g-5h","displayName":"5 Hour Limit","window":"5h","remainingFraction":0,"resetTime":"2026-10-04T03:00:00Z"},
			{"bucketId":"g-week","displayName":"Weekly Limit","window":"weekly","remainingFraction":0.2,"resetTime":"2026-10-08T00:00:00Z"}]},
		{"displayName":"Claude and GPT Models","buckets":[
			{"window":"five_hour","remainingFraction":1,"resetTime":"2026-10-04T03:00:00Z"},
			{"window":"WEEK","remainingFraction":1,"resetTime":"2026-10-08T00:00:00Z"}]}]}`}
	s.usage["same-times-other-amounts"] = []string{`{"groups":[{"displayName":"Gemini Models","buckets":[
		{"window":"weekly","remaining_fraction":1,"reset_time":"2026-10-08T08:00:00+08:00"},
		{"window":"five-hour","remainingFraction":"0.9","resetTime":"2026-10-04T03:00:00Z"}]}]}`}
	s.usage["weekly-only"] = []string{`{"groups":[{"displayName":"Gemini Models","buckets":[{"window":"weekly","remainingFraction":0,"resetTime":"2026-10-08T00:00:00Z"}]}]}`}
	s.usage["earliest"] = []string{`{"groups":[{"displayName":"Gemini Models","buckets":[{"window":"week","resetTime":"2026-10-06T09:00:00+09:00"}]}]}`}
	s.usage["conflicting-groups"] = []string{`{"groups":[
		{"displayName":"Gemini Models","buckets":[{"window":"weekly","resetTime":"2026-10-07T00:00:00Z"}]},
		{"displayName":"Claude and GPT Models","buckets":[{"window":"weekly","resetTime":"2026-10-09T00:00:00Z"}]}]}`}
	s.usage["codex"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-12-01T00:00:00Z"}}}`}
	before := map[string]map[string]any{}
	for _, file := range s.files {
		copy := map[string]any{}
		for k, v := range file {
			copy[k] = v
		}
		before[file["name"].(string)] = copy
	}
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"earliest": 2, "merged": 1, "same-times-other-amounts": 1, "weekly-only": 0, "conflicting-groups": -1, "no-project": -1, "codex": 0}
	for _, file := range s.files {
		name := file["name"].(string)
		before[name]["priority"] = want[name]
		if !reflect.DeepEqual(file, before[name]) {
			t.Errorf("unexpected business state for %s: %#v", name, file)
		}
	}
	status := map[string]string{}
	for _, result := range round.Results {
		status[result.Name] = result.QueryStatus
	}
	if status["conflicting-groups"] != "quota_period_ambiguous" || status["no-project"] != "missing_project_id" || s.queryCount["no-project"] != 0 || s.queryCount["conflicting-groups"] != 2 || s.queryCount["merged"] != 1 {
		t.Fatalf("statuses=%v queries=%v", status, s.queryCount)
	}
	if s.selectedProxy != "socks5://account-proxy" {
		t.Fatalf("auth proxy not preserved: %q", s.selectedProxy)
	}
	encoded, _ := json.Marshal(round)
	for _, secret := range []string{"old-token", "management-secret", "project-"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("result leaks %s", secret)
		}
	}
}

func TestSyncAntigravityRetryEndpointsAndUnrankableData(t *testing.T) {
	valid := `{"groups":[{"displayName":"Gemini Models","buckets":[{"window":"weekly","resetTime":"2026-10-08T00:00:00Z"}]}]}`
	for _, tc := range []struct {
		name      string
		responses []string
		status    int
		priority  int
		urls      []string
		query     string
	}{
		{"recovered-on-second-endpoint", []string{`{"groups":[{"buckets":[{"window":"weekly","resetTime":"not-a-time"}]}]}`, valid}, 200, 0, []string{dailyQuotaURL, sandboxQuotaURL}, "ok"},
		{"two-failures", []string{`{"error":"secret-access-token"}`}, 503, -1, []string{dailyQuotaURL, sandboxQuotaURL}, "upstream_http_failed"},
		{"missing-groups", []string{`{"groups":null}`}, 200, -1, []string{dailyQuotaURL, sandboxQuotaURL}, "quota_response_malformed"},
		{"bad-buckets", []string{`{"groups":[{"buckets":{}}]}`}, 200, -1, []string{dailyQuotaURL, sandboxQuotaURL}, "quota_response_malformed"},
		{"invalid-credentials", []string{`{"error":"secret"}`}, 401, -1, []string{dailyQuotaURL}, "credentials_invalid"},
		{"unverified-window", []string{`{"groups":[{"buckets":[{"window":"daily","resetTime":"2026-10-05T00:00:00Z"},{"window":"weekly","resetTime":"2026-10-08T00:00:00Z"}]}]}`}, 200, -1, []string{dailyQuotaURL, sandboxQuotaURL}, "quota_period_unverified"},
		{"missing-window", []string{`{"groups":[{"buckets":[{"resetTime":"2026-10-05T00:00:00Z"}]}]}`}, 200, -1, []string{dailyQuotaURL, sandboxQuotaURL}, "quota_period_unverified"},
		{"empty-groups", []string{`{"groups":[]}`}, 200, -1, []string{dailyQuotaURL}, "no_reset_time"},
		{"no-reset-times", []string{`{"groups":[{"buckets":[{"window":"weekly","remainingFraction":1,"resetTime":""},{"window":"daily","remainingFraction":1}]}],"subscriptionEndTime":"2026-10-05T00:00:00Z"}`}, 200, -1, []string{dailyQuotaURL}, "no_reset_time"},
		{"unix-reset", []string{`{"groups":[{"buckets":[{"window":"weekly","resetTime":1791417600}]}]}`}, 200, 0, []string{dailyQuotaURL}, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(antigravity("candidate"), antigravity("healthy"))
			s.usage["candidate"] = tc.responses
			s.upstreamStatus["candidate"] = tc.status
			s.usage["healthy"] = []string{valid}
			round, err := synchronizer(t, s).Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if s.files[0]["priority"] != tc.priority || s.files[1]["priority"] != 0 || !reflect.DeepEqual(s.queryURLs["candidate"], tc.urls) || s.queryCount["healthy"] != 1 || round.Results[0].QueryStatus != tc.query {
				t.Fatalf("state=%v urls=%v results=%+v", s.files, s.queryURLs, round.Results)
			}
			encoded, _ := json.Marshal(round)
			if strings.Contains(string(encoded), "secret") {
				t.Fatal("result leaks upstream response")
			}
		})
	}
}
