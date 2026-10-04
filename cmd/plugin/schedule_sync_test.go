package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
)

// Stateful HTTP boundary: snapshots serialize current auth, narrow PATCH alone
// changes priority, and quota responses reflect the current upstream state.
type scheduledStore struct {
	mu               sync.Mutex
	files            []map[string]any
	usage            map[string]string
	queries, writes  map[string]int
	failWrite        string
	disabled         bool
	managementStatus int
}

func scheduledAuth(name, provider string) map[string]any {
	return map[string]any{"id": name, "name": name + ".json", "auth_index": name, "provider": provider, "source": "file", "runtime_only": false, "path": "/auth/" + name + ".json", "priority": 9, "access_token": "preserved-token", "disabled": false, "proxy_url": "socks5://account-proxy"}
}

func quota(reset string) string {
	return fmt.Sprintf(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":%q}}}`, reset)
}

func (s *scheduledStore) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer explicit-secret" {
		return response(401, `{}`), nil
	}
	jsonReply := func(code int, value any) (*http.Response, error) {
		raw, _ := json.Marshal(value)
		return response(code, string(raw)), nil
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /v8/management/plugins":
		if s.managementStatus != 0 {
			return response(s.managementStatus, `{"error":"explicit-secret"}`), nil
		}
		return jsonReply(200, map[string]any{"plugins": []any{map[string]any{"id": "cpa-auto-priority", "effective_enabled": !s.disabled}}})
	case "GET /v8/management/credentials":
		return jsonReply(200, map[string]any{"observed_at": "2026-10-04T00:00:00Z", "files": s.files})
	case "POST /v8/management/requests/api-call":
		var call struct {
			Index  string  `json:"auth_index"`
			Method string  `json:"method"`
			URL    string  `json:"url"`
			Proxy  *string `json:"proxy_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			return nil, err
		}
		if call.Method != "GET" || call.URL != "https://chatgpt.com/backend-api/wham/usage" || call.Proxy != nil {
			return nil, fmt.Errorf("invalid quota contract")
		}
		s.queries[call.Index]++
		return jsonReply(200, map[string]any{"status_code": 200, "body": s.usage[call.Index]})
	case "PATCH /v8/management/credentials/fields":
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			return nil, err
		}
		for _, auth := range s.files {
			if auth["id"] != patch["name"] {
				continue
			}
			if len(patch) == 1 {
				return response(400, `{"error":"no fields to update"}`), nil
			}
			value, ok := patch["priority"].(float64)
			if len(patch) != 2 || !ok {
				return nil, fmt.Errorf("not a narrow priority update")
			}
			id := auth["id"].(string)
			s.writes[id]++
			if id == s.failWrite {
				return response(500, `{}`), nil
			}
			auth["priority"] = int(value)
			return response(200, `{"status":"ok"}`), nil
		}
		return response(404, `{}`), nil
	default:
		return nil, fmt.Errorf("unexpected host operation: %s %s", r.Method, r.URL.Path)
	}
}

func TestCronRefreshesAuthAndQuotaAndPreservesRoundRules(t *testing.T) {
	a, b, removed := scheduledAuth("a", "codex"), scheduledAuth("b", "codex"), scheduledAuth("removed", "codex")
	s := &scheduledStore{files: []map[string]any{a, b, removed}, usage: map[string]string{"a": quota("2026-10-08T00:00:00Z"), "b": quota("2026-10-12T00:00:00Z"), "removed": quota("2026-10-14T00:00:00Z")}, queries: map[string]int{}, writes: map[string]int{}}
	p, clock := newTestRuntime(&http.Client{Transport: s})
	defer p.stop()
	if err := p.configure(lifecycle(validConfig)); err != nil {
		t.Fatal(err)
	}
	first := clock.Await(t)
	s.mu.Lock()
	if a["priority"] != 2 || b["priority"] != 1 || removed["priority"] != 0 {
		t.Errorf("startup ranks: %v %v %v", a["priority"], b["priority"], removed["priority"])
	}
	added, broken, unknown := scheduledAuth("added", "codex"), scheduledAuth("broken", "codex"), scheduledAuth("unknown", "future-provider")
	s.files = []map[string]any{b, added, a, broken, unknown}
	s.usage["a"] = quota("2026-10-20T00:00:00Z") // natural reset advanced; no cached prior time
	s.usage["added"] = quota("2026-10-10T00:00:00Z")
	s.usage["broken"] = `{"rate_limit":{"primary_window":"malformed"}}`
	s.failWrite = "unknown"
	before := map[string]map[string]any{}
	for _, file := range s.files {
		copy := map[string]any{}
		for k, v := range file {
			copy[k] = v
		}
		before[file["id"].(string)] = copy
	}
	s.mu.Unlock()
	clock.Fire(first)
	second := clock.Await(t)
	s.mu.Lock()
	want := map[string]int{"a": 0, "b": 1, "added": 2, "broken": -1, "unknown": 9}
	for _, file := range s.files {
		id := file["id"].(string)
		before[id]["priority"] = want[id]
		if !reflect.DeepEqual(file, before[id]) {
			t.Errorf("unexpected business state for %s: %v", id, file)
		}
	}
	if !reflect.DeepEqual(s.queries, map[string]int{"a": 2, "b": 2, "removed": 1, "added": 1, "broken": 2}) || !reflect.DeepEqual(s.writes, map[string]int{"a": 2, "b": 2, "removed": 1, "added": 1, "broken": 1, "unknown": 1}) {
		t.Errorf("round retry or stale membership: queries=%v writes=%v", s.queries, s.writes)
	}
	s.disabled = true
	s.mu.Unlock()
	state := p.getStatus()
	for _, result := range state.Round.Results {
		if result.Name == "unknown.json" && (result.Priority != -1 || result.WriteStatus != "failed") {
			t.Errorf("write failure hidden: %+v", result)
		}
	}
	clock.Fire(second)
	<-p.done
	if state := p.getStatus(); state.Error != "plugin_not_enabled" {
		t.Fatalf("disabled instance kept running: %+v", state)
	}
}

func TestCronManagementAuthenticationFailureStopsSchedule(t *testing.T) {
	for _, code := range []int{401, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := &scheduledStore{files: []map[string]any{}, queries: map[string]int{}, writes: map[string]int{}}
			p, clock := newTestRuntime(&http.Client{Transport: s})
			defer p.stop()
			if err := p.configure(lifecycle(validConfig)); err != nil {
				t.Fatal(err)
			}
			first := clock.Await(t)
			s.mu.Lock()
			s.managementStatus = code
			s.mu.Unlock()
			clock.Fire(first)
			<-p.done
			if state := p.getStatus(); state.Phase != "failed" || state.Error != "management_authentication_failed" {
				t.Fatalf("authentication failure did not stop schedule: %+v", state)
			}
		})
	}
}
