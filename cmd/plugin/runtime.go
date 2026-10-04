package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	priority "github.com/Insulinocytus/cpa-auto-priority-plugin"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

const statusPath = "/auto-priority/status"

type pluginConfig struct {
	Enabled         bool   `yaml:"enabled"`
	Priority        int    `yaml:"priority"`
	Cron            string `yaml:"cron"`
	Timezone        string `yaml:"timezone"`
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
	now       func() time.Time
	wait      func(context.Context, time.Duration) bool
}

func newRuntime(client *http.Client) *pluginRuntime {
	return &pluginRuntime{client: client, now: time.Now, wait: waitFor, state: status{Phase: "not_configured"}}
}

func waitFor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
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
	if config.Cron == "" {
		config.Cron = "0 0 * * *"
	}
	fields := strings.Fields(config.Cron)
	if len(fields) != 5 || strings.Contains(config.Cron, "?") {
		return errors.New("invalid_cron: expected standard five fields")
	}
	for _, field := range fields {
		if strings.HasPrefix(field, ",") || strings.HasSuffix(field, ",") || strings.Contains(field, ",,") {
			return errors.New("invalid_cron: empty list item")
		}
	}
	schedule, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(config.Cron)
	if err != nil {
		return errors.New("invalid_cron: expected standard five fields")
	}
	location := time.Local
	if config.Timezone != "" {
		location, err = time.LoadLocation(config.Timezone)
		if err != nil {
			return errors.New("invalid_timezone")
		}
	}
	schedule.(*cron.SpecSchedule).Location = location
	if schedule.Next(p.now().In(location)).IsZero() {
		return errors.New("invalid_cron: no calendar occurrence")
	}
	syncer, err := priority.New(config.Config, p.client, p.now)
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
	go p.run(ctx, p.done, syncer, schedule.(*cron.SpecSchedule))
	return nil
}

func (p *pluginRuntime) run(ctx context.Context, done chan struct{}, syncer *priority.Synchronizer, schedule *cron.SpecSchedule) {
	defer close(done)
	started := false
	for {
		round, err := syncer.Sync(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			p.setStatus(status{Phase: round.Status, Round: &round})
		} else {
			p.setStatus(status{Phase: "failed", Error: err.Error(), Round: &round})
			// Management auth retries can ban the shared client IP. Disable
			// is not a reason to retain a periodic background worker either.
			if errors.Is(err, priority.ErrManagementAuthentication) || (started && errors.Is(err, priority.ErrPluginNotEnabled)) {
				return
			}
			if !started && len(round.Results) == 0 {
				p.setStatus(status{Phase: "waiting", Error: err.Error()})
				if !p.wait(ctx, time.Second) {
					return
				}
				continue
			}
		}
		started = true
		// One worker owns both triggers and Sync. Missed occurrences during
		// a long round are skipped, never queued or replayed.
		now := p.now().In(schedule.Location)
		next := schedule.Next(now)
		if next.IsZero() {
			p.setStatus(status{Phase: "failed", Error: "invalid_cron: no calendar occurrence"})
			return
		}
		if !p.wait(ctx, next.Sub(now)) || ctx.Err() != nil {
			return
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
		return rpcSuccess(map[string]any{"routes": []any{map[string]any{"Method": "GET", "Path": statusPath, "Description": "Last priority sync; persistence is unverified."}}})
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
				map[string]string{"Name": "cron", "Type": "string", "Description": "Standard five-field cron; default 0 0 * * * (calendar midnight)."},
				map[string]string{"Name": "timezone", "Type": "string", "Description": "IANA timezone override; default host system timezone."},
			},
		},
		"capabilities": map[string]bool{"management_api": true},
	}
}
