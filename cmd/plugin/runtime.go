package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	priority "github.com/Insulinocytus/cpa-auto-priority-plugin"
	"gopkg.in/yaml.v3"
)

const statusPath = "/auto-priority/status"

type pluginConfig struct {
	Enabled         bool `yaml:"enabled"`
	Priority        int  `yaml:"priority"`
	priority.Config `yaml:",inline"`
}

type status struct {
	Phase string          `json:"phase"`
	Error string          `json:"error,omitempty"`
	Round *priority.Round `json:"round,omitempty"`
}

type pluginRuntime struct {
	lifecycle sync.Mutex
	stateMu   sync.Mutex
	config    pluginConfig
	cancel    context.CancelFunc
	done      chan struct{}
	state     status
	client    *http.Client
}

func newRuntime(client *http.Client) *pluginRuntime {
	return &pluginRuntime{client: client, state: status{Phase: "not_configured"}}
}

func (p *pluginRuntime) configure(raw []byte) error {
	var request struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if json.Unmarshal(raw, &request) != nil {
		return errors.New("invalid_lifecycle_request")
	}
	var config pluginConfig
	decoder := yaml.NewDecoder(bytes.NewReader(request.ConfigYAML))
	decoder.KnownFields(true)
	if decoder.Decode(&config) != nil {
		return errors.New("invalid_plugin_config")
	}
	syncer, err := priority.New(config.Config, p.client, nil)
	if err != nil {
		return err
	}
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	if p.cancel != nil && config == p.config {
		return nil
	}
	p.stopLocked()
	p.config = config
	if !config.Enabled {
		p.setStatus(status{Phase: "disabled"})
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	p.setStatus(status{Phase: "starting"})
	go p.startup(ctx, p.done, syncer)
	return nil
}

func (p *pluginRuntime) startup(ctx context.Context, done chan struct{}, syncer *priority.Synchronizer) {
	defer close(done)
	for {
		round, err := syncer.Sync(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			p.setStatus(status{Phase: round.Status, Round: &round})
			return
		}
		// Readiness polling is not a provider/write retry. Once a round has
		// produced auth results, never rerun it automatically after an error.
		if len(round.Results) > 0 {
			p.setStatus(status{Phase: "failed", Error: err.Error(), Round: &round})
			return
		}
		p.setStatus(status{Phase: "waiting", Error: err.Error()})
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *pluginRuntime) stopLocked() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel = nil
		p.done = nil
	}
}

func (p *pluginRuntime) stop() {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.stopLocked()
	p.setStatus(status{Phase: "stopped"})
}

func (p *pluginRuntime) setStatus(state status) {
	p.stateMu.Lock()
	p.state = state
	p.stateMu.Unlock()
}
func (p *pluginRuntime) getStatus() status {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.state
}

type envelope struct {
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *rpcError `json:"error,omitempty"`
}
type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func rpcFailure(message string) []byte {
	raw, _ := json.Marshal(envelope{Error: &rpcError{Code: "plugin_error", Message: message}})
	return raw
}
func rpcSuccess(value any) []byte {
	raw, _ := json.Marshal(envelope{OK: true, Result: value})
	return raw
}

func (p *pluginRuntime) handle(method string, raw []byte) []byte {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := p.configure(raw); err != nil {
			return rpcFailure(err.Error())
		}
		return rpcSuccess(registration())
	case "plugin.quiesce", "plugin.shutdown":
		p.stop()
		return rpcSuccess(struct{}{})
	case "management.register":
		return rpcSuccess(map[string]any{"routes": []any{map[string]any{"Method": "GET", "Path": statusPath, "Description": "Last startup priority sync; persistence is unverified."}}})
	case "management.handle":
		var request struct {
			Method string
			Path   string
		}
		if json.Unmarshal(raw, &request) != nil {
			return rpcFailure("invalid_management_request")
		}
		code := http.StatusOK
		var body []byte
		if request.Method != "GET" || request.Path != "/v0/management"+statusPath {
			code = http.StatusNotFound
			body = []byte(`{"error":"not_found"}`)
		} else {
			body, _ = json.Marshal(p.getStatus())
		}
		return rpcSuccess(struct {
			StatusCode int
			Headers    http.Header
			Body       []byte
		}{code, http.Header{"Content-Type": []string{"application/json"}}, body})
	default:
		return rpcFailure("unknown_method")
	}
}

func registration() any {
	// Metadata/ConfigFields use the upstream Go wire types' PascalCase keys;
	// only the surrounding RPC registration fields are snake_case.
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]any{
			"Name": priority.PluginID, "Version": "0.1.0", "Author": "Insulinocytus", "GitHubRepository": "https://github.com/Insulinocytus/cpa-auto-priority-plugin",
			"ConfigFields": []any{
				map[string]string{"Name": "management_url", "Type": "string", "Description": "This host's management origin; HTTPS is required off loopback."},
				map[string]string{"Name": "management_key", "Type": "string", "Description": "Explicit management key; protect the host configuration file."},
			},
		},
		"capabilities": map[string]bool{"management_api": true},
	}
}
