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

func waitDone(t *testing.T, p *pluginRuntime) {
	t.Helper()
	p.lifecycle.Lock()
	done := p.done
	p.lifecycle.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("startup failed to complete")
	}
}

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
	p := newRuntime(client)
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
	waitDone(t, p)
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
	for _, config := range []string{"enabled: [\"explicit-secret\"]", validConfig + "cron: garbage\n", "enabled: true\nmanagement_url: http://remote.example\nmanagement_key: explicit-secret\n"} {
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
