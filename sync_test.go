package priority_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	priority "github.com/Insulinocytus/cpa-auto-priority-plugin"
)

// A stateful management service at the HTTP boundary: only the real narrow
// update contract can mutate its credential store. No socket or account is used.
type managementStore struct {
	files          []map[string]any
	usage          map[string][]string
	upstreamStatus map[string]int
	cards          map[string][]string
	cardStatus     map[string]int
	cardQueryCount map[string]int
	queryCount     map[string]int
	writeCount     map[string]int
	writeFailure   map[string]bool
	key            string
	refresh        bool
	disabled       bool
	disableOnQuery bool
	snapshot       any
	probeStatus    int
	probeError     string
	selectedProxy  string
	accountID      string
}

func store(files ...map[string]any) *managementStore {
	return &managementStore{files: files, usage: map[string][]string{}, cards: map[string][]string{}, cardStatus: map[string]int{}, cardQueryCount: map[string]int{}, upstreamStatus: map[string]int{}, queryCount: map[string]int{}, writeCount: map[string]int{}, writeFailure: map[string]bool{}, key: "management-secret"}
}

func credential(name, provider string) map[string]any {
	return map[string]any{"id": "id-" + name, "name": name, "auth_index": name, "provider": provider, "type": provider, "source": "file", "runtime_only": false, "path": "/auth/" + name, "priority": 9, "disabled": false, "proxy_url": "socks5://account-proxy", "access_token": "old-token", "note": "preserve"}
}

func jsonResponse(status int, value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(data))}
}

func (s *managementStore) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "Bearer "+s.key {
		return jsonResponse(401, map[string]any{"error": "wrong management-secret"}), nil
	}
	switch req.Method + " " + req.URL.Path {
	case "GET /v8/management/plugins":
		return jsonResponse(200, map[string]any{"plugins": []any{map[string]any{"id": priority.PluginID, "effective_enabled": !s.disabled}}}), nil
	case "GET /v8/management/credentials":
		if s.snapshot != nil {
			return jsonResponse(200, s.snapshot), nil
		}
		response := jsonResponse(200, map[string]any{"observed_at": "2026-10-04T00:00:00Z", "files": s.files})
		if s.refresh {
			for _, file := range s.files {
				file["access_token"] = "concurrently-refreshed-token"
			}
		}
		return response, nil
	case "POST /v8/management/requests/api-call":
		var call struct {
			AuthIndex string            `json:"auth_index"`
			Method    string            `json:"method"`
			URL       string            `json:"url"`
			Header    map[string]string `json:"header"`
			ProxyURL  *string           `json:"proxy_url"`
		}
		if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
			return nil, err
		}
		isCard := call.URL == "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
		if call.Method != "GET" || (!isCard && call.URL != "https://chatgpt.com/backend-api/wham/usage") || call.Header["Authorization"] != "Bearer $TOKEN$" || call.ProxyURL != nil {
			return jsonResponse(400, map[string]any{"error": "invalid query contract"}), nil
		}
		for _, file := range s.files {
			if file["auth_index"] == call.AuthIndex {
				s.selectedProxy, _ = file["proxy_url"].(string)
				if token, ok := file["id_token"].(map[string]any); ok && call.Header["Chatgpt-Account-Id"] != token["chatgpt_account_id"] {
					return jsonResponse(400, map[string]any{"error": "wrong selected account"}), nil
				}
			}
		}
		s.accountID = call.Header["Chatgpt-Account-Id"]
		if isCard {
			if call.Header["OpenAI-Beta"] != "codex-1" || call.Header["Originator"] != "Codex Desktop" || call.Header["Accept"] != "application/json" {
				return jsonResponse(400, map[string]any{"error": "invalid card contract"}), nil
			}
			index := s.cardQueryCount[call.AuthIndex]
			s.cardQueryCount[call.AuthIndex]++
			responses := s.cards[call.AuthIndex]
			if len(responses) == 0 {
				responses = []string{`{"credits":[],"available_count":0,"applicable_available_count":0}`}
			}
			if index >= len(responses) {
				index = len(responses) - 1
			}
			status := s.cardStatus[call.AuthIndex]
			if status == 0 {
				status = 200
			}
			return jsonResponse(200, map[string]any{"status_code": status, "body": responses[index]}), nil
		}
		index := s.queryCount[call.AuthIndex]
		s.queryCount[call.AuthIndex]++
		if s.disableOnQuery {
			s.disabled = true
		}
		responses := s.usage[call.AuthIndex]
		if len(responses) == 0 {
			return nil, fmt.Errorf("unexpected query for %s", call.AuthIndex)
		}
		if index >= len(responses) {
			index = len(responses) - 1
		}
		status := s.upstreamStatus[call.AuthIndex]
		if status == 0 {
			status = 200
		}
		return jsonResponse(200, map[string]any{"status_code": status, "header": map[string]any{}, "body": responses[index]}), nil
	case "PATCH /v8/management/credentials/fields":
		var patch map[string]any
		if err := json.NewDecoder(req.Body).Decode(&patch); err != nil {
			return nil, err
		}
		name, _ := patch["name"].(string)
		var target map[string]any
		for _, file := range s.files {
			if file["id"] == name {
				target = file
				break
			}
		}
		if target == nil {
			for _, file := range s.files {
				if file["name"] == name {
					target = file
					break
				}
			}
		}
		if target == nil {
			return jsonResponse(404, map[string]any{"error": "removed"}), nil
		}
		if target["plugin_virtual"] == true {
			return jsonResponse(409, map[string]any{"error": "plugin virtual auth cannot be modified directly; edit or delete the source auth file"}), nil
		}
		if len(patch) == 1 && name != "" {
			if s.probeStatus != 0 {
				return jsonResponse(s.probeStatus, map[string]any{"error": s.probeError}), nil
			}
			return jsonResponse(400, map[string]any{"error": "no fields to update"}), nil
		}
		if len(patch) != 2 || patch["priority"] == nil {
			return jsonResponse(400, map[string]any{"error": "only name and priority accepted"}), nil
		}
		fileName := target["name"].(string)
		s.writeCount[fileName]++
		if s.writeFailure[fileName] {
			return jsonResponse(500, map[string]any{"error": "write failed with token old-token"}), nil
		}
		target["priority"] = int(patch["priority"].(float64))
		return jsonResponse(200, map[string]any{"status": "ok"}), nil
	default:
		return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL)
	}
}

func synchronizer(t *testing.T, s *managementStore) *priority.Synchronizer {
	t.Helper()
	p, err := priority.New(priority.Config{ManagementURL: "http://127.0.0.1:8317", ManagementKey: s.key}, &http.Client{Transport: s}, func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Desensitized wham/usage shape; durations, not primary/secondary names,
// identify each quota period. Expected ranks come from issue #1's worked example.
func TestSyncMonthlyPrecedenceMissingLayerAndFailureIsolation(t *testing.T) {
	s := store(credential("B.json", "codex"), credential("D.json", "codex"), credential("A.json", "codex"), credential("C.json", "codex"), credential("unknown.json", "unknown"))
	s.usage["A.json"] = []string{`{"plan_type":"pro","rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-08T00:00:00Z","used_percent":100},"secondary_window":{"limit_window_seconds":2592000,"reset_at":"2026-10-20T00:00:00Z"}}}`}
	s.usage["C.json"] = []string{`{"plan_type":"team","rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_at":"2026-10-20T00:00:00Z"},"secondary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-12T00:00:00Z"}}}`}
	s.usage["B.json"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_at":"2026-10-20T00:00:00Z"},"secondary_window":null}}`}
	s.usage["D.json"] = []string{`{"error":"secret-access-token"}`}
	s.upstreamStatus["D.json"] = 503
	s.refresh = true
	before := map[string]map[string]any{}
	for _, file := range s.files {
		copy := map[string]any{}
		for k, v := range file {
			copy[k] = v
		}
		copy["access_token"] = "concurrently-refreshed-token"
		before[file["name"].(string)] = copy
	}
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"A.json": 2, "C.json": 1, "B.json": 0, "D.json": -1, "unknown.json": -1}
	for _, file := range s.files {
		name := file["name"].(string)
		before[name]["priority"] = want[name]
		if !reflect.DeepEqual(file, before[name]) {
			t.Errorf("unexpected business state for %s: %#v", name, file)
		}
	}
	if s.queryCount["D.json"] != 2 || s.queryCount["A.json"] != 1 {
		t.Fatalf("query retry isolation: %v", s.queryCount)
	}
	for _, result := range round.Results {
		if result.Priority != want[result.Name] || result.WriteStatus != "acknowledged" || result.Persistence != "unverified" {
			t.Errorf("incorrect result: %+v", result)
		}
	}
	encoded, _ := json.Marshal(round)
	for _, secret := range []string{"old-token", "concurrently-refreshed-token", "management-secret", "secret-access-token"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("result leaks secret")
		}
	}
}

// Synthetic responses in the verified wham shape exercise periods and encodings
// not present in the upstream fixture; no claim of real account observations.
func TestSyncLongestPeriodTiesOrderAndAbsoluteTime(t *testing.T) {
	for _, order := range [][]string{{"month-late", "month-early", "week", "tie", "relative", "seconds", "milliseconds", "other"}, {"other", "milliseconds", "seconds", "relative", "tie", "week", "month-early", "month-late"}} {
		s := store()
		for _, name := range order {
			provider := "codex"
			if name == "other" {
				provider = "claude"
			}
			s.files = append(s.files, credential(name, provider))
		}
		s.usage["month-late"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_at":"2027-01-02T00:00:00Z"},"secondary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-05T00:00:00Z"}}}`}
		s.usage["month-early"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_at":"2027-01-01T00:00:00Z"},"secondary_window":{"limit_window_seconds":604800,"reset_at":"2027-01-10T00:00:00Z"}}}`}
		s.usage["week"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-12-31T23:59:59Z"}}}`}
		s.usage["tie"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":2678400,"reset_at":"2026-12-31T19:00:00-05:00"},"secondary_window":{"limit_window_seconds":604800,"reset_at":"2027-01-10T00:00:00Z"}}}`}
		s.usage["relative"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"reset_after_seconds":7689600}}}`}
		s.usage["seconds"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1798761600}}}`}
		s.usage["milliseconds"] = []string{`{"rate_limit":{"secondary_window":{"limit_window_seconds":86400,"reset_at":"1798761600000"}}}`}
		_, err := synchronizer(t, s).Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"month-late": 0, "relative": 1, "seconds": 1, "milliseconds": 1, "month-early": 2, "tie": 2, "week": 3, "other": -1}
		for _, file := range s.files {
			if file["priority"] != want[file["name"].(string)] {
				t.Errorf("%s rank=%v want=%v", file["name"], file["priority"], want[file["name"].(string)])
			}
		}
	}
}

func TestSyncMalformedRetryAndConfirmedAbsence(t *testing.T) {
	valid := `{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1798761600}}}`
	for _, tc := range []struct {
		name               string
		responses          []string
		status             int
		priority, attempts int
		query              string
	}{
		{"recovered", []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":"bad","reset_at":1798761600}}}`, valid}, 200, 0, 2, "ok"},
		{"bad-reset", []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"token-value"}}}`}, 200, -1, 2, "quota_response_malformed"},
		{"bad-secondary", []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1798761600},"secondary_window":[]}}`}, 200, -1, 2, "quota_response_malformed"},
		{"null-short", []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":"604800","reset_at":1798761600},"secondary_window":null}}`}, 200, 0, 1, "ok"},
		{"no-times", []string{`{"rate_limit":{"primary_window":null,"secondary_window":null},"token_expires_at":"2026-10-05T00:00:00Z","subscription_end":1798761600}`}, 200, -1, 1, "no_reset_time"},
		{"null-time", []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":null,"reset_after_seconds":null}}}`}, 200, -1, 1, "no_reset_time"},
		{"bad-root", []string{`null`}, 200, -1, 2, "quota_response_malformed"},
		{"invalid-credentials", []string{`{"error":"secret"}`}, 401, -1, 1, "credentials_invalid"},
		{"upstream-failure", []string{valid}, 500, -1, 2, "upstream_http_failed"},
		{"ambiguous-period", []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1798761600},"secondary_window":{"limit_window_seconds":604800,"reset_at":1798848000}}}`}, 200, -1, 2, "quota_period_ambiguous"},
		{"duplicate-month", []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":2419200,"reset_at":1798761600},"secondary_window":{"limit_window_seconds":2678400,"reset_at":1798761600}}}`}, 200, 0, 1, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(credential("candidate", "codex"), credential("healthy", "codex"))
			s.usage["candidate"] = tc.responses
			s.upstreamStatus["candidate"] = tc.status
			s.usage["healthy"] = []string{valid}
			round, err := synchronizer(t, s).Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if s.files[0]["priority"] != tc.priority || s.files[1]["priority"] != 0 || s.queryCount["candidate"] != tc.attempts || s.queryCount["healthy"] != 1 || round.Results[0].QueryStatus != tc.query {
				t.Fatalf("state=%v queries=%v results=%+v", s.files, s.queryCount, round.Results)
			}
		})
	}
}

func TestSyncWriteFailureDoesNotRetryOrOverwriteCredentials(t *testing.T) {
	s := store(credential("fails", "codex"), credential("works", "codex"))
	s.refresh = true
	s.writeFailure["fails"] = true
	s.usage["fails"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1798761600}}}`}
	s.usage["works"] = s.usage["fails"]
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if round.Status != "write_failed" || round.Results[0].WriteStatus != "failed" || round.Results[1].WriteStatus != "acknowledged" || s.files[0]["priority"] != 9 || s.files[1]["priority"] != 0 || s.writeCount["fails"] != 1 {
		t.Fatalf("state=%v round=%+v writes=%v", s.files, round, s.writeCount)
	}
	data, _ := json.Marshal(round)
	if strings.Contains(string(data), "old-token") || strings.Contains(string(data), s.key) {
		t.Fatal("write result leaked response")
	}
}

func TestSyncExcludesVirtualAndConfigurationKeys(t *testing.T) {
	physical := credential("physical", "unknown")
	virtual := credential("virtual", "codex")
	virtual["plugin_virtual"] = true
	configKey := credential("config", "codex")
	configKey["source"] = "memory"
	delete(configKey, "path")
	s := store(virtual, configKey, physical)
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if virtual["priority"] != 9 || configKey["priority"] != 9 || physical["priority"] != -1 || len(round.Results) != 1 || round.Results[0].Name != "physical" {
		t.Fatalf("scope violated: %v %+v", s.files, round)
	}
}

func TestSyncReadinessAndDisableNeverWrite(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		snapshot                 any
		disabled, disableOnQuery bool
		wantError                bool
		wantStatus               string
	}{
		{"no-readiness-marker", map[string]any{"files": []any{}}, false, false, true, "failed"},
		{"ready-empty", map[string]any{"observed_at": "2026-10-04T00:00:00Z", "files": []any{}}, false, false, false, "empty"},
		{"disabled", nil, true, false, true, "failed"},
		{"disabled-during-query", nil, false, true, true, "not_enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(credential("file", "codex"))
			s.snapshot = tc.snapshot
			s.disabled = tc.disabled
			s.disableOnQuery = tc.disableOnQuery
			s.usage["file"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1798761600}}}`}
			round, err := synchronizer(t, s).Sync(context.Background())
			if (err != nil) != tc.wantError || round.Status != tc.wantStatus || s.files[0]["priority"] != 9 || len(s.writeCount) != 0 {
				t.Fatalf("unsafe readiness: %+v %v store=%v", round, err, s.files)
			}
		})
	}
}

func TestSyncUpstreamFixtureAndIgnoredSpecialPurposeWindows(t *testing.T) {
	// Extracted from official tests/codexQuota.test.ts at ee79a79, with
	// reset-credit fields removed. Not a captured response from our accounts.
	s := store(credential("fixture", "codex"), credential("later", "codex"))
	s.usage["fixture"] = []string{`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_after_seconds":601888,"reset_at":1785902974},"secondary_window":null},"code_review_rate_limit":null,"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","metered_feature":"codex_bengalfox","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":602111,"reset_at":1785903197},"secondary_window":null}}]}`}
	s.usage["later"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1785903000}},"code_review_rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_at":1}}}`}
	_, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.files[0]["priority"] != 1 || s.files[1]["priority"] != 0 {
		t.Fatalf("special scope affected ranks: %v", s.files)
	}
}

func TestSyncProbeFailsClosedAndIDDisambiguatesSharedNames(t *testing.T) {
	for _, code := range []int{200, 400, 409, 500} {
		s := store(credential("physical", "codex"))
		s.probeStatus = code
		s.probeError = "unexpected secret error"
		round, err := synchronizer(t, s).Sync(context.Background())
		if err == nil || err.Error() != "physical_auth_unconfirmed" || len(round.Results) != 0 || len(s.queryCount) != 0 || s.files[0]["priority"] != 9 {
			t.Fatalf("probe %d failed open: %+v %v", code, round, err)
		}
	}
	physical := credential("shared", "codex")
	virtual := credential("shared", "codex")
	virtual["id"] = "virtual-id"
	virtual["plugin_virtual"] = true
	s := store(virtual, physical)
	s.usage["shared"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1798761600}}}`}
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if virtual["priority"] != 9 || physical["priority"] != 0 || len(round.Results) != 1 {
		t.Fatalf("name collision: %v %+v", s.files, round)
	}
}

func TestSyncCurrentCollectionProxyAndNoQuotaAmountPreference(t *testing.T) {
	a := credential("a", "codex")
	a["id_token"] = map[string]any{"chatgpt_account_id": "account-selected"}
	s := store(a, credential("b", "codex"))
	p := synchronizer(t, s)
	s.usage["a"] = []string{`{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1798761600}}}`}
	s.usage["b"] = []string{`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1798761600}}}`}
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.files[0]["priority"] != 0 || s.files[1]["priority"] != 0 || s.selectedProxy != "socks5://account-proxy" {
		t.Fatalf("quota amount affected sorting or proxy lost: %v", s.files)
	}
	s.files = []map[string]any{a, credential("new", "codex")}
	s.usage["new"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1798848000}}}`}
	round, err := p.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Results) != 2 || round.Results[1].Name != "new" || a["priority"] != 1 || s.files[1]["priority"] != 0 || s.queryCount["b"] != 1 {
		t.Fatalf("stale collection: %+v %v", round, s.queryCount)
	}
	// Re-run with the account-header credential alone to observe auth selection.
	s.files = []map[string]any{a}
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.accountID != "account-selected" {
		t.Fatalf("wrong account header: %q", s.accountID)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSyncTransportErrorsDoNotRevealSecretsOrFollowRedirects(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		calls := 0
		client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if redirect {
				response := jsonResponse(302, map[string]any{})
				response.Header.Set("Location", "https://untrusted.example/?key=management-secret")
				return response, nil
			}
			return nil, fmt.Errorf("upstream token secret-access-token management-secret")
		})}
		p, err := priority.New(priority.Config{ManagementURL: "http://127.0.0.1:8317", ManagementKey: "management-secret"}, client, nil)
		if err != nil {
			t.Fatal(err)
		}
		round, err := p.Sync(context.Background())
		if err == nil || calls != 1 || len(round.Results) != 0 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe transport: %v %+v requests=%d", err, round, calls)
		}
	}
}

func TestSyncRelativeTimeUsesNewObservationEveryRound(t *testing.T) {
	s := store(credential("relative", "codex"), credential("absolute", "codex"))
	s.usage["relative"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_after_seconds":3600}}}`}
	s.usage["absolute"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-12-31T23:30:00Z"}}}`}
	now := time.Date(2026, 12, 31, 22, 0, 0, 0, time.UTC)
	p, err := priority.New(priority.Config{ManagementURL: "http://127.0.0.1:8317", ManagementKey: s.key}, &http.Client{Transport: s}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.files[0]["priority"] != 1 || s.files[1]["priority"] != 0 {
		t.Fatalf("first observation: %v", s.files)
	}
	now = time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)
	if _, err = p.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.files[0]["priority"] != 0 || s.files[1]["priority"] != 1 {
		t.Fatalf("relative time cached across year: %v", s.files)
	}
}

func TestConfigRejectsUnsafeManagementOriginsAndMissingAuthentication(t *testing.T) {
	for _, config := range []priority.Config{
		{ManagementURL: "http://remote.example", ManagementKey: "secret"},
		{ManagementURL: "https://user:secret@remote.example", ManagementKey: "secret"},
		{ManagementURL: "http://127.0.0.1:8317?key=secret", ManagementKey: "secret"},
		{ManagementURL: "http://127.0.0.1:8317/path", ManagementKey: "secret"},
		{ManagementURL: "http://127.0.0.1:8317", ManagementKey: ""},
		{ManagementURL: "http://127.0.0.1:8317", ManagementKey: "secret\r\nOther: secret"},
	} {
		_, err := priority.New(config, nil, nil)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe config validation: %v", err)
		}
	}
}

func TestSyncEquivalentFractionalInstantsShareRank(t *testing.T) {
	for _, tc := range []struct{ name, iso, milliseconds, seconds, scientific string }{
		{"millisecond", `"2027-01-01T00:00:00.123Z"`, "1798761600123", `"1798761600.123"`, "1798761600123e-3"},
		{"nanosecond", `"2027-01-01T00:00:00.123456789Z"`, "1798761600123.456789", `"1798761600.123456789"`, "1798761600.123456789e0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(credential("iso", "codex"), credential("milliseconds", "codex"), credential("seconds", "codex"), credential("scientific", "codex"))
			for name, reset := range map[string]string{"iso": tc.iso, "milliseconds": tc.milliseconds, "seconds": tc.seconds, "scientific": tc.scientific} {
				s.usage[name] = []string{fmt.Sprintf(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":%s}}}`, reset)}
			}
			_, err := synchronizer(t, s).Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range s.files {
				if file["priority"] != 0 {
					t.Fatalf("equivalent instant split into ranks: %v", s.files)
				}
			}
		})
	}
}

func TestSyncCardQueryFailureIsolation(t *testing.T) {
	valid := `{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-10T00:00:00Z"}}}`
	empty := `{"credits":[],"available_count":0,"applicable_available_count":0}`
	for _, tc := range []struct {
		name                   string
		responses              []string
		status                 int
		wantPriority, attempts int
		query                  string
	}{
		{"confirmed-empty", []string{empty}, 200, 0, 1, "ok"},
		{"incomplete-details", []string{`{"available_count":2,"credits":[{"reset_type":"codex_rate_limits","status":"available","granted_at":"2026-10-01T00:00:00Z","expires_at":"2026-10-05T00:00:00Z"}]}`}, 200, -1, 2, "reset_card_details_incomplete"},
		{"unknown-scope", []string{`{"available_count":1,"credits":[{"reset_type":"future_scope","status":"available","granted_at":"2026-10-01T00:00:00Z","expires_at":"2026-10-05T00:00:00Z"}]}`}, 200, -1, 2, "reset_card_applicability_unknown"},
		{"nonexpiring", []string{`{"available_count":1,"credits":[{"reset_type":"codex_rate_limits","status":"available","granted_at":"2026-10-01T00:00:00Z"}]}`}, 200, 0, 1, "ok"},
		{"recovered-malformed", []string{`{}`, empty}, 200, 0, 2, "ok"},
		{"malformed", []string{`{"credits":null}`}, 200, -1, 2, "reset_card_response_malformed"},
		{"failed", []string{`{"error":"sensitive-card-token"}`}, 503, -1, 2, "upstream_http_failed"},
		{"invalid-credentials", []string{empty}, 401, -1, 1, "credentials_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(credential("candidate", "codex"), credential("healthy", "codex"))
			s.usage["candidate"], s.usage["healthy"] = []string{valid}, []string{valid}
			s.cards["candidate"], s.cardStatus["candidate"] = tc.responses, tc.status
			round, err := synchronizer(t, s).Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if s.files[0]["priority"] != tc.wantPriority || s.files[1]["priority"] != 0 || round.Results[0].QueryStatus != tc.query || s.cardQueryCount["candidate"] != tc.attempts || s.cardQueryCount["healthy"] != 1 || s.queryCount["candidate"] != 1 || s.queryCount["healthy"] != 1 {
				t.Fatalf("state=%v results=%+v usage=%v cards=%v", s.files, round.Results, s.queryCount, s.cardQueryCount)
			}
			encoded, _ := json.Marshal(round)
			if strings.Contains(string(encoded), "sensitive-card-token") {
				t.Fatal("card query leaked upstream body")
			}
		})
	}
}

// Synthetic dates in the OpenAI backend-client reset-credit fixture shape.
func resetCredit(kind, status, granted string, expires any) map[string]any {
	return map[string]any{"id": "fixture-credit", "reset_type": kind, "status": status, "granted_at": granted, "expires_at": expires}
}

func resetCards(credits ...map[string]any) string {
	available := 0
	for _, credit := range credits {
		if credit["status"] == "available" {
			available++
		}
	}
	if credits == nil {
		credits = []map[string]any{}
	}
	body, _ := json.Marshal(map[string]any{"available_count": available, "applicable_available_count": 0, "credits": credits})
	return string(body)
}

func TestSyncValidResetCardsRankBeforeLimitAndPreserveAuth(t *testing.T) {
	grant := "2026-10-01T00:00:00Z"
	card := func(expiry string) map[string]any {
		return resetCredit("codex_rate_limits", "available", grant, expiry)
	}
	for _, names := range [][]string{{"early", "equal", "late", "multiple", "invalid", "empty", "shorter-natural"}, {"empty", "invalid", "multiple", "late", "equal", "shorter-natural", "early"}} {
		s := store()
		for _, name := range names {
			file := credential(name, "codex")
			file["id_token"] = map[string]any{"chatgpt_account_id": "account-" + name}
			s.files = append(s.files, file)
			s.usage[name] = []string{`{"credits":{"balance":"99999","has_credits":true},"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-10T00:00:00Z","used_percent":1},"secondary_window":{"limit_window_seconds":18000,"reset_at":"2026-10-06T00:00:00Z"}}}`}
		}
		s.cards["early"] = []string{resetCards(card("2026-10-05T00:00:00Z"))}
		s.cards["equal"] = []string{resetCards(card("2026-10-10T00:00:00Z"))}
		s.cards["late"] = []string{resetCards(card("2026-10-11T00:00:00Z"))}
		s.cards["multiple"] = []string{resetCards(card("2026-10-07T00:00:00Z"), card("2026-10-05T00:00:00Z"), card("2026-10-11T00:00:00Z"))}
		s.cards["invalid"] = []string{resetCards(
			card("2026-10-03T00:00:00Z"),
			card("2026-10-04T00:00:00Z"),
			resetCredit("codex_rate_limits", "available", "2026-10-05T00:00:00Z", "2026-10-08T00:00:00Z"),
			resetCredit("codex_rate_limits", "redeemed", grant, "2026-10-05T00:00:00Z"),
			resetCredit("codex_rate_limits", "redeeming", grant, "2026-10-05T00:00:00Z"),
			resetCredit("codex_rate_limits", "available", grant, nil),
		)}
		s.usage["shorter-natural"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-10T00:00:00Z"},"secondary_window":{"limit_window_seconds":18000,"reset_at":"2026-10-04T12:00:00Z"}}}`}
		s.cards["shorter-natural"] = s.cards["early"]
		s.refresh = true
		before := map[string]map[string]any{}
		for _, file := range s.files {
			copy := map[string]any{}
			for k, v := range file {
				copy[k] = v
			}
			copy["access_token"] = "concurrently-refreshed-token"
			before[file["name"].(string)] = copy
		}
		round, err := synchronizer(t, s).Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"early": 1, "multiple": 1, "shorter-natural": 2, "equal": 0, "late": 0, "invalid": 0, "empty": 0}
		for _, result := range round.Results {
			before[result.Name]["priority"] = want[result.Name]
			if result.Priority != want[result.Name] || result.QueryStatus != "ok" || result.WriteStatus != "acknowledged" || result.Persistence != "unverified" {
				t.Fatalf("unexpected card ranks: %+v", round.Results)
			}
		}
		for _, file := range s.files {
			if !reflect.DeepEqual(file, before[file["name"].(string)]) {
				t.Fatalf("card synchronization overwrote auth: %v", file)
			}
		}
	}
}

func TestSyncResetCardsCannotCrossPeriodsOrCreateMissingLayer(t *testing.T) {
	s := store(credential("month-earlier", "codex"), credential("month-card", "codex"), credential("month-natural", "codex"), credential("month-only-card", "codex"), credential("month-only", "codex"), credential("day-card", "codex"))
	s.usage["month-earlier"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_at":"2026-10-19T00:00:00Z"},"secondary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-30T00:00:00Z"}}}`}
	s.usage["month-card"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_at":"2026-10-20T00:00:00Z"},"secondary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-10T00:00:00Z"}}}`}
	s.usage["month-natural"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_at":"2026-10-20T00:00:00Z"},"secondary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-08T00:00:00Z"}}}`}
	s.usage["month-only-card"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_at":"2026-10-20T00:00:00Z"}}}`}
	s.usage["month-only"] = s.usage["month-only-card"]
	s.usage["day-card"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":86400,"reset_at":"2026-10-20T00:00:00Z"}}}`}
	for _, name := range []string{"month-card", "month-only-card", "day-card"} {
		s.cards[name] = []string{resetCards(resetCredit("codex_rate_limits", "available", "2026-10-01T00:00:00Z", "2026-10-05T00:00:00Z"))}
	}
	round, err := synchronizer(t, s).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"month-earlier": 3, "month-card": 2, "month-natural": 1, "month-only-card": 0, "month-only": 0, "day-card": 0}
	for _, result := range round.Results {
		if result.Priority != want[result.Name] || result.QueryStatus != "ok" {
			t.Fatalf("cross-period card assignment: %+v", round.Results)
		}
	}
}

func TestSyncMissingResetCardDataIsNotConfirmedAbsence(t *testing.T) {
	for _, body := range []string{
		`{"available_count":1,"credits":[]}`,
		`{"credits":[]}`,
		`{"available_count":1,"credits":[{"status":"available","granted_at":"2026-10-01T00:00:00Z","expires_at":"2026-10-05T00:00:00Z"}]}`,
		`{"available_count":1,"credits":[{"reset_type":"codex_rate_limits","granted_at":"2026-10-01T00:00:00Z","expires_at":"2026-10-05T00:00:00Z"}]}`,
		`{"available_count":1,"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":"2026-10-05T00:00:00Z"}]}`,
		`{"available_count":1,"credits":[{"reset_type":"codex_rate_limits","status":"available","granted_at":"2026-10-01T00:00:00Z","expires_at":"invalid-sensitive-date"}]}`,
	} {
		s := store(credential("candidate", "codex"), credential("healthy", "codex"))
		s.usage["candidate"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-10T00:00:00Z"}}}`}
		s.usage["healthy"] = s.usage["candidate"]
		s.cards["candidate"] = []string{body}
		round, err := synchronizer(t, s).Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if s.files[0]["priority"] != -1 || s.files[1]["priority"] != 0 || s.cardQueryCount["candidate"] != 2 || s.queryCount["candidate"] != 1 || round.Results[0].QueryStatus == "ok" {
			t.Fatalf("missing card data accepted: %s results=%+v", body, round.Results)
		}
	}
}

func TestSyncRefreshesResetCardsWithoutQuotaAmountPreference(t *testing.T) {
	s := store(credential("card", "codex"), credential("natural", "codex"))
	s.usage["card"] = []string{`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-10T00:00:00Z","used_percent":0}}}`}
	s.usage["natural"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-08T00:00:00Z"}}}`}
	s.cards["card"] = []string{resetCards(resetCredit("codex_rate_limits", "available", "2026-10-04T00:00:00Z", "2026-10-05T00:00:00Z"))}
	p := synchronizer(t, s)
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.files[0]["priority"] != 1 || s.files[1]["priority"] != 0 {
		t.Fatalf("card before limit: %v", s.files)
	}
	s.usage["card"] = []string{`{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-10T00:00:00Z","used_percent":100}}}`}
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.files[0]["priority"] != 1 || s.files[1]["priority"] != 0 {
		t.Fatalf("quota amount changed card ranking: %v", s.files)
	}
	s.cards["card"] = []string{resetCards(resetCredit("codex_rate_limits", "redeemed", "2026-10-04T00:00:00Z", "2026-10-05T00:00:00Z"))}
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.files[0]["priority"] != 0 || s.files[1]["priority"] != 1 {
		t.Fatalf("consumed card remained cached: %v", s.files)
	}
}

func TestSyncCardManagementAuthenticationStopsBeforeWrites(t *testing.T) {
	for _, code := range []int{401, 403} {
		s := store(credential("candidate", "codex"), credential("unqueried", "codex"))
		s.usage["candidate"] = []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":"2026-10-10T00:00:00Z"}}}`}
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
				if call.URL == "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" {
					failures++
					return jsonResponse(code, map[string]any{"error": "sensitive-management-token"}), nil
				}
			}
			return s.RoundTrip(req)
		})}
		p, err := priority.New(priority.Config{ManagementURL: "http://127.0.0.1:8317", ManagementKey: s.key}, client, nil)
		if err != nil {
			t.Fatal(err)
		}
		round, err := p.Sync(context.Background())
		if err != priority.ErrManagementAuthentication || failures != 1 || len(s.writeCount) != 0 || s.queryCount["candidate"] != 1 || s.queryCount["unqueried"] != 0 || s.files[0]["priority"] != 9 || s.files[1]["priority"] != 9 {
			t.Fatalf("management authentication failure continued work: err=%v round=%+v", err, round)
		}
	}
}
