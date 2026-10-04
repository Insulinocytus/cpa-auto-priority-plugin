// Package priority synchronizes physical CLIProxyAPI auth-file priorities.
package priority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const PluginID = "cpa-auto-priority"

// ErrManagementAuthentication stops a round on management HTTP 401/403.
var ErrManagementAuthentication = errors.New("management_authentication_failed")

// Config uses an explicit management origin and its plaintext management key.
// The key is never included in observable errors or results.
type Config struct {
	ManagementURL string `yaml:"management_url"`
	ManagementKey string `yaml:"management_key"`
}

type Result struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Priority    int    `json:"priority"`
	QueryStatus string `json:"query_status"`
	WriteStatus string `json:"write_status"`
	Persistence string `json:"persistence"`
}

type Round struct {
	Status  string   `json:"status"`
	Results []Result `json:"results"`
}

// Synchronizer owns one instance's serial single-round entry point. HTTP and
// observation time are the only replaceable external boundaries.
type Synchronizer struct {
	config Config
	client *http.Client
	now    func() time.Time
	mu     sync.Mutex
}

func New(config Config, client *http.Client, now func() time.Time) (*Synchronizer, error) {
	u, err := url.Parse(config.ManagementURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid_management_url: expected HTTP(S) origin without credentials, path or query")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" && u.Hostname() != "localhost" {
		return nil, errors.New("invalid_management_url: remote management requires HTTPS")
	}
	if strings.TrimSpace(config.ManagementKey) == "" || strings.ContainsAny(config.ManagementKey, "\r\n") {
		return nil, errors.New("invalid_management_key")
	}
	config.ManagementURL = strings.TrimRight(config.ManagementURL, "/")
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	copy := *client
	// Even same-origin redirects can target a different management operation.
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if now == nil {
		now = time.Now
	}
	return &Synchronizer{config: config, client: &copy, now: now}, nil
}

type authFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Index       string `json:"auth_index"`
	Source      string `json:"source"`
	Path        string `json:"path"`
	RuntimeOnly *bool  `json:"runtime_only"`
	IDToken     struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"id_token"`
}

type managementHTTPError struct{ status int }

func (e *managementHTTPError) Error() string { return "management_http_failed" }

func (s *Synchronizer) request(ctx context.Context, method, path string, body any, out any) error {
	var encoded []byte
	if body != nil {
		encoded, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.config.ManagementURL+"/v8/management/"+path, bytes.NewReader(encoded))
	if err != nil {
		return errors.New("management_request_failed")
	}
	req.Header.Set("Authorization", "Bearer "+s.config.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return errors.New("management_transport_failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrManagementAuthentication
	}
	if out != nil {
		data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
		if err != nil || len(data) > 2*1024*1024 || json.Unmarshal(data, out) != nil {
			return errors.New("management_response_malformed")
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &managementHTTPError{response.StatusCode}
	}
	return nil
}

// The host removes capabilities on disable without notifying the library.
// Check its effective registration before work and before each narrow write.
func (s *Synchronizer) enabled(ctx context.Context) error {
	var listing struct {
		Plugins []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"effective_enabled"`
		} `json:"plugins"`
	}
	if err := s.request(ctx, "GET", "plugins", nil, &listing); err != nil {
		return err
	}
	for _, plugin := range listing.Plugins {
		if plugin.ID == PluginID && plugin.Enabled {
			return nil
		}
	}
	return errors.New("plugin_not_enabled")
}

// Pinned fields validation rejects virtual auth before checking for absent
// updates. A name-only probe changes no business field and persists nothing.
// List endpoints omit plugin_virtual, so list heuristics alone are unsafe.
func (s *Synchronizer) physical(ctx context.Context, name string) (bool, error) {
	var response struct {
		Error string `json:"error"`
	}
	err := s.request(ctx, "PATCH", "credentials/fields", struct {
		Name string `json:"name"`
	}{name}, &response)
	if errors.Is(err, ErrManagementAuthentication) {
		return false, err
	}
	var httpErr *managementHTTPError
	if errors.As(err, &httpErr) {
		if httpErr.status == 409 && response.Error == "plugin virtual auth cannot be modified directly; edit or delete the source auth file" {
			return false, nil
		}
		if httpErr.status == 400 && response.Error == "no fields to update" {
			return true, nil
		}
	}
	return false, errors.New("physical_auth_unconfirmed")
}

func (s *Synchronizer) Sync(ctx context.Context) (Round, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	round := Round{Status: "failed", Results: []Result{}}
	if ctx.Err() != nil {
		return round, errors.New("sync_cancelled")
	}
	if err := s.enabled(ctx); err != nil {
		return round, err
	}
	var snapshot struct {
		Files      *[]authFile `json:"files"`
		ObservedAt time.Time   `json:"observed_at"`
	}
	if err := s.request(ctx, "GET", "credentials", nil, &snapshot); err != nil {
		return round, err
	}
	if snapshot.Files == nil || snapshot.ObservedAt.IsZero() {
		return round, errors.New("auth_snapshot_malformed")
	}
	if len(*snapshot.Files) == 0 {
		// No claim that an empty snapshot proves auth initialization completed.
		round.Status = "empty"
		return round, nil
	}
	files := make([]authFile, 0, len(*snapshot.Files))
	ids := make(map[string]bool)
	for _, file := range *snapshot.Files {
		if file.RuntimeOnly == nil || *file.RuntimeOnly || file.Source != "file" || file.Path == "" {
			continue
		}
		if strings.TrimSpace(file.ID) == "" || strings.TrimSpace(file.Name) == "" || ids[file.ID] {
			return round, errors.New("auth_snapshot_malformed")
		}
		ids[file.ID] = true
		physical, err := s.physical(ctx, file.ID)
		if err != nil {
			return round, err
		}
		if !physical {
			continue
		}
		file.Provider = strings.ToLower(strings.TrimSpace(file.Provider))
		files = append(files, file)
	}
	if len(files) == 0 {
		round.Status = "empty"
		return round, nil
	}
	sequences := make([][]time.Time, len(files))
	groups := make(map[string][]int)
	for i, file := range files {
		result := Result{Name: file.Name, Provider: file.Provider, Priority: -1, QueryStatus: "unsupported_provider", WriteStatus: "not_attempted", Persistence: "unverified"}
		sequences[i], result.QueryStatus = s.quota(ctx, file)
		if result.QueryStatus == ErrManagementAuthentication.Error() {
			round.Results = append(round.Results, result)
			return round, ErrManagementAuthentication
		}
		if len(sequences[i]) > 0 {
			groups[file.Provider] = append(groups[file.Provider], i)
		}
		round.Results = append(round.Results, result)
	}
	for _, group := range groups {
		// Worst first: compact ranks begin at zero, equal sequences share a rank.
		sort.Slice(group, func(i, j int) bool { return compare(sequences[group[i]], sequences[group[j]]) > 0 })
		rank := 0
		for j, index := range group {
			if j > 0 && compare(sequences[group[j-1]], sequences[index]) != 0 {
				rank++
			}
			round.Results[index].Priority = rank
		}
	}
	round.Status = "completed"
	for i := range round.Results {
		result := &round.Results[i]
		if ctx.Err() != nil {
			round.Status = "cancelled"
			return round, errors.New("sync_cancelled")
		}
		if err := s.enabled(ctx); err != nil {
			round.Status = "not_enabled"
			return round, err
		}
		patch := struct {
			Name     string `json:"name"`
			Priority int    `json:"priority"`
		}{files[i].ID, result.Priority}
		var response struct {
			Status string `json:"status"`
		}
		err := s.request(ctx, "PATCH", "credentials/fields", patch, &response)
		if err != nil || response.Status != "ok" {
			result.WriteStatus = "failed"
			round.Status = "write_failed"
			if errors.Is(err, ErrManagementAuthentication) {
				return round, err
			}
		} else {
			result.WriteStatus = "acknowledged"
		}
	}
	return round, nil
}

// Earlier instants win; if the common prefix ties, more known layers win.
func compare(a, b []time.Time) int {
	for i := range min(len(a), len(b)) {
		if a[i].Before(b[i]) {
			return -1
		}
		if a[i].After(b[i]) {
			return 1
		}
	}
	if len(a) > len(b) {
		return -1
	}
	if len(a) < len(b) {
		return 1
	}
	return 0
}
