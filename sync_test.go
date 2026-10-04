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
	return &managementStore{files: files, usage: map[string][]string{}, upstreamStatus: map[string]int{}, queryCount: map[string]int{}, writeCount: map[string]int{}, writeFailure: map[string]bool{}, key: "management-secret"}
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
		if call.Method != "GET" || call.URL != "https://chatgpt.com/backend-api/wham/usage" || call.Header["Authorization"] != "Bearer $TOKEN$" || call.ProxyURL != nil {
			return jsonResponse(400, map[string]any{"error": "invalid query contract"}), nil
		}
		for _, file := range s.files {
			if file["auth_index"] == call.AuthIndex {
				s.selectedProxy, _ = file["proxy_url"].(string)
			}
		}
		s.accountID = call.Header["Chatgpt-Account-Id"]
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
