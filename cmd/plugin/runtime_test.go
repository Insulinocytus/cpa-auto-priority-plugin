package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	priority "github.com/Insulinocytus/cpa-auto-priority-plugin"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(body))}
}
func lifecycle(config string) []byte {
	raw, _ := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{[]byte(config)})
	return raw
}

const validConfig = "enabled: true\npriority: 1\nmanagement_url: http://127.0.0.1:8317\nmanagement_key: explicit-secret\n"

func TestStartupReconfigureOnceAndAuthenticatedStatus(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer explicit-secret" {
			t.Error("missing explicit management authentication")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v8/management/plugins":
			return response(200, `{"plugins":[{"id":"cpa-auto-priority","effective_enabled":true}]}`), nil
		case "GET /v8/management/credentials":
			return response(200, `{"observed_at":"2026-10-04T00:00:00Z","files":[{"id":"stable-id","name":"one.json","auth_index":"index","provider":"codex","source":"file","runtime_only":false,"path":"/auth/one.json"}]}`), nil
		case "POST /v8/management/requests/api-call":
			return response(200, `{"status_code":200,"body":"{\"rate_limit\":{\"primary_window\":{\"limit_window_seconds\":604800,\"reset_at\":1798761600}}}"}`), nil
		case "PATCH /v8/management/credentials/fields":
			var patch map[string]any
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if patch["name"] != "stable-id" {
				t.Error("field selector is not auth ID")
			}
			if len(patch) == 1 {
				return response(400, `{"error":"no fields to update"}`), nil
			}
			mu.Lock()
			writes++
			mu.Unlock()
			return response(200, `{"status":"ok"}`), nil
		default:
			t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
			return response(500, `{}`), nil
		}
	})}
	p, clock := newTestRuntime(client)
	defer p.stop()
	registered := p.handle("plugin.register", lifecycle(validConfig))
	var registrationEnvelope struct {
		OK     bool
		Result struct {
			SchemaVersion int `json:"schema_version"`
			Metadata      struct{ Name string }
			Capabilities  struct {
				Management bool `json:"management_api"`
			}
		}
	}
	if json.Unmarshal(registered, &registrationEnvelope) != nil || !registrationEnvelope.OK || registrationEnvelope.Result.SchemaVersion != 6 || registrationEnvelope.Result.Metadata.Name != priority.PluginID || !registrationEnvelope.Result.Capabilities.Management {
		t.Fatalf("invalid registration: %s", registered)
	}
	clock.Await(t)
	if err := p.configure(lifecycle(validConfig)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count := writes
	mu.Unlock()
	if count != 1 {
		t.Fatalf("identical reconfigure repeated startup writes: %d", count)
	}
	request, _ := json.Marshal(map[string]string{"Method": "GET", "Path": "/v0/management" + statusPath})
	var rpc struct {
		OK     bool
		Result struct {
			StatusCode int
			Body       []byte
		}
	}
	if json.Unmarshal(p.handle("management.handle", request), &rpc) != nil {
		t.Fatal("invalid status envelope")
	}
	var state status
	_ = json.Unmarshal(rpc.Result.Body, &state)
	if !rpc.OK || rpc.Result.StatusCode != 200 || state.Phase != "completed" || state.Round == nil || state.Round.Results[0].Priority != 0 || state.Round.Results[0].Persistence != "unverified" || bytes.Contains(rpc.Result.Body, []byte("explicit-secret")) {
		t.Fatalf("status=%s", rpc.Result.Body)
	}
}

func TestStartupManagementAuthenticationFailureStopsAndReconfigureRecovers(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		code             int
		write            bool
	}{
		{"plugins-401", "/v8/management/plugins", `{"error":"explicit-secret"}`, http.StatusUnauthorized, false},
		{"plugins-403", "/v8/management/plugins", `{"error":"explicit-secret"}`, http.StatusForbidden, false},
		{"credentials-401", "/v8/management/credentials", "explicit-secret", http.StatusUnauthorized, false},
		{"credentials-403", "/v8/management/credentials", "", http.StatusForbidden, false},
		{"physical-401", "/v8/management/credentials/fields", `{"error":"explicit-secret"}`, http.StatusUnauthorized, false},
		{"physical-403", "/v8/management/credentials/fields", "explicit-secret", http.StatusForbidden, false},
		{"query-401", "/v8/management/requests/api-call", `{"error":"explicit-secret"}`, http.StatusUnauthorized, false},
		{"query-403", "/v8/management/requests/api-call", "", http.StatusForbidden, false},
		{"write-401", "/v8/management/credentials/fields", "explicit-secret", http.StatusUnauthorized, true},
		{"write-403", "/v8/management/credentials/fields", `{"error":"explicit-secret"}`, http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failures := 0
			retried := make(chan struct{})
			p, clock := newTestRuntime(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") == "Bearer corrected-secret" {
					if r.URL.Path == "/v8/management/plugins" {
						return response(200, `{"plugins":[{"id":"cpa-auto-priority","effective_enabled":true}]}`), nil
					}
					return response(200, `{"observed_at":"2026-10-04T00:00:00Z","files":[]}`), nil
				}
				if r.URL.Path == tc.path {
					if tc.write {
						var patch map[string]any
						if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
							t.Error(err)
						}
						if len(patch) == 1 {
							return response(400, `{"error":"no fields to update"}`), nil
						}
					}
					failures++
					if failures == 2 {
						close(retried)
					}
					return response(tc.code, tc.body), nil
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /v8/management/plugins":
					return response(200, `{"plugins":[{"id":"cpa-auto-priority","effective_enabled":true}]}`), nil
				case "GET /v8/management/credentials":
					return response(200, `{"observed_at":"2026-10-04T00:00:00Z","files":[{"id":"one-id","name":"one.json","auth_index":"one","provider":"codex","source":"file","runtime_only":false,"path":"/auth/one.json"},{"id":"two-id","name":"two.json","auth_index":"two","provider":"codex","source":"file","runtime_only":false,"path":"/auth/two.json"}]}`), nil
				case "PATCH /v8/management/credentials/fields":
					return response(400, `{"error":"no fields to update"}`), nil
				case "POST /v8/management/requests/api-call":
					return response(200, `{"status_code":200,"body":"{\"rate_limit\":{\"primary_window\":{\"limit_window_seconds\":604800,\"reset_at\":1798761600}}}"}`), nil
				default:
					t.Errorf("unexpected operation after authentication failure: %s %s", r.Method, r.URL.Path)
					return response(500, `{}`), nil
				}
			})})
			defer p.stop()
			if err := p.configure(lifecycle(validConfig)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.done:
			case <-retried:
				t.Fatal("repeated management authentication failure can trigger an IP ban")
			case <-time.After(3 * time.Second):
				t.Fatal("authentication failure did not terminate startup")
			}
			state := p.getStatus()
			if failures != 1 || state.Phase != "failed" || state.Error != "management_authentication_failed" {
				t.Fatalf("incorrect authentication failure result: attempts=%d status=%+v", failures, state)
			}
			corrected := string(bytes.ReplaceAll([]byte(validConfig), []byte("explicit-secret"), []byte("corrected-secret")))
			if err := p.configure(lifecycle(corrected)); err != nil {
				t.Fatal(err)
			}
			clock.Await(t)
			if state := p.getStatus(); state.Phase != "empty" {
				t.Fatalf("corrected management key did not recover startup: %+v", state)
			}
		})
	}
}

func TestShutdownCancelsAndJoinsPendingStartup(t *testing.T) {
	entered := make(chan struct{})
	left := make(chan struct{})
	p := newRuntime(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		close(left)
		return nil, r.Context().Err()
	})})
	if err := p.configure(lifecycle(validConfig)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("startup not entered")
	}
	p.handle("plugin.quiesce", []byte(`{}`))
	select {
	case <-left:
	default:
		t.Fatal("quiesce returned before request exited")
	}
	if p.getStatus().Phase != "stopped" {
		t.Fatal("not stopped")
	}
	// Shutdown is idempotent after quiesce; no released-host access remains.
	p.stop()
}

func TestInvalidReconfigurationPreservesRunningGeneration(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	p := newRuntime(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
		return nil, context.Canceled
	})})
	defer p.stop()
	if err := p.configure(lifecycle(validConfig)); err != nil {
		t.Fatal(err)
	}
	<-entered
	for _, config := range []string{"enabled: [\"explicit-secret\"]", validConfig + "cron: garbage\n", validConfig + "timezone: invalid/location\n", "enabled: true\nmanagement_url: http://remote.example\nmanagement_key: explicit-secret\n"} {
		if err := p.configure(lifecycle(config)); err == nil || bytes.Contains([]byte(err.Error()), []byte("explicit-secret")) {
			t.Fatalf("unsafe validation: %v", err)
		}
	}
	p.lifecycle.Lock()
	running := p.cancel != nil
	p.lifecycle.Unlock()
	if !running {
		t.Fatal("bad config stopped valid generation")
	}
}
