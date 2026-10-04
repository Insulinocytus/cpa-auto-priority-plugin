package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInvalidSchedulesDoNotStartHostWork(t *testing.T) {
	for _, config := range []string{
		"cron: '0 0 * *'\n",
		"cron: '0 0 0 * * *'\n",
		"cron: '@daily'\n",
		"cron: 'CRON_TZ=UTC 0 0 * * *'\n",
		"cron: 'TZ=UTC\t0\t0\t*\t*'\n",
		"cron: 'CRON_TZ=UTC\t0\t0\t*\t*'\n",
		"cron: '60 0 * * *'\n",
		"cron: '*/0 * * * *'\n",
		"cron: '0 0 30 2 *'\n",
		"cron: '0,,1 0 * * *'\n",
		"cron: '0 0 * * ?'\n",
		"timezone: 'Not/A_Timezone'\n",
	} {
		t.Run(strings.TrimSpace(config), func(t *testing.T) {
			p := newRuntime(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				t.Error("invalid configuration accessed host")
				return response(500, `{}`), nil
			})})
			defer p.stop()
			err := p.configure(lifecycle(validConfig + config))
			want := "invalid_cron"
			if strings.HasPrefix(config, "timezone:") {
				want = "invalid_timezone"
			}
			if err == nil || !strings.HasPrefix(err.Error(), want) || bytes.Contains([]byte(err.Error()), []byte("explicit-secret")) {
				t.Fatalf("configuration error=%v, want %s", err, want)
			}
		})
	}
}

func TestCronUsesCalendarAndTimezone(t *testing.T) {
	for _, tc := range []struct {
		name, extra          string
		start, first, second time.Time
	}{
		{"host midnight", "", time.Date(2026, 10, 4, 12, 34, 0, 0, time.Local), time.Date(2026, 10, 5, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 6, 0, 0, 0, 0, time.Local)},
		{"Shanghai weekday step", "cron: '*/15 9-10 * * MON-FRI'\ntimezone: Asia/Shanghai\n", instant("2026-10-04T23:59:00Z"), instant("2026-10-05T01:00:00Z"), instant("2026-10-05T01:15:00Z")},
		{"spring DST", "timezone: America/New_York\n", instant("2026-03-07T17:00:00Z"), instant("2026-03-08T05:00:00Z"), instant("2026-03-09T04:00:00Z")},
		{"fall DST", "timezone: America/New_York\n", instant("2026-10-31T16:00:00Z"), instant("2026-11-01T04:00:00Z"), instant("2026-11-02T05:00:00Z")},
		{"month and day list", "cron: '30 6 1,15 JAN,MAR *'\ntimezone: UTC\n", instant("2026-01-14T12:00:00Z"), instant("2026-01-15T06:30:00Z"), instant("2026-03-01T06:30:00Z")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			p, clock := newTestRuntime(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/v8/management/plugins":
					return response(200, `{"plugins":[{"id":"cpa-auto-priority","effective_enabled":true}]}`), nil
				case "/v8/management/credentials":
					reads++
					return response(200, `{"observed_at":"2026-10-04T00:00:00Z","files":[]}`), nil
				default:
					t.Errorf("unexpected operation: %s", r.URL.Path)
					return response(500, `{}`), nil
				}
			})})
			defer p.stop()
			clock.Set(tc.start)
			if err := p.configure(lifecycle(validConfig + tc.extra)); err != nil {
				t.Fatal(err)
			}
			first := clock.Await(t)
			if !first.at.Equal(tc.first) || reads != 1 || p.getStatus().Phase != "empty" {
				t.Fatalf("first schedule=%s reads=%d status=%+v", first.at, reads, p.getStatus())
			}
			if err := p.configure(lifecycle(validConfig + tc.extra)); err != nil {
				t.Fatal(err)
			}
			clock.Fire(first)
			second := clock.Await(t)
			if !second.at.Equal(tc.second) || reads != 2 {
				t.Fatalf("next schedule=%s reads=%d", second.at, reads)
			}
			p.handle("plugin.shutdown", nil)
			clock.Fire(second)
			if reads != 2 || p.getStatus().Phase != "stopped" {
				t.Fatal("shutdown allowed another round")
			}
		})
	}
}

func instant(value string) time.Time {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return at
}

func TestCronSkipsMissedRoundsAndShutdownJoinsBeforeHostRelease(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	cancelled := make(chan struct{})
	leave := make(chan struct{})
	var mu sync.Mutex
	var leaveOnce sync.Once
	queries, active, maxActive, afterRelease := 0, 0, 0, 0
	hostReleased := false
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		if hostReleased {
			afterRelease++
		}
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v8/management/plugins":
			return response(200, `{"plugins":[{"id":"cpa-auto-priority","effective_enabled":true}]}`), nil
		case "GET /v8/management/credentials":
			return response(200, `{"observed_at":"2026-10-04T00:00:00Z","files":[{"id":"one","name":"one.json","auth_index":"one","provider":"codex","source":"file","runtime_only":false,"path":"/auth/one.json"}]}`), nil
		case "PATCH /v8/management/credentials/fields":
			var patch map[string]any
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if len(patch) == 1 {
				return response(400, `{"error":"no fields to update"}`), nil
			}
			return response(200, `{"status":"ok"}`), nil
		case "POST /v8/management/requests/api-call":
			var call struct {
				URL string `json:"url"`
			}
			if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
				return nil, err
			}
			if call.URL == "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" {
				return response(200, `{"status_code":200,"body":"{\"available_count\":0,\"credits\":[]}"}`), nil
			}
			mu.Lock()
			queries++
			n := queries
			active++
			maxActive = max(maxActive, active)
			mu.Unlock()
			defer func() { mu.Lock(); active--; mu.Unlock() }()
			if n == 2 {
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			if n == 3 {
				entered <- struct{}{}
				<-r.Context().Done()
				close(cancelled)
				<-leave
				return nil, r.Context().Err()
			}
			return response(200, `{"status_code":200,"body":"{\"rate_limit\":{\"primary_window\":{\"limit_window_seconds\":604800,\"reset_at\":1798761600}}}"}`), nil
		default:
			return nil, errors.New("unexpected host access")
		}
	})}
	p, clock := newTestRuntime(client)
	defer func() { leaveOnce.Do(func() { close(leave) }); p.stop() }()
	clock.Set(instant("2026-10-04T00:00:10Z"))
	if err := p.configure(lifecycle(validConfig + "cron: '* * * * *'\ntimezone: UTC\n")); err != nil {
		t.Fatal(err)
	}
	clock.Fire(clock.Await(t))
	<-entered
	clock.Set(instant("2026-10-04T00:03:30Z"))
	close(release)
	next := clock.Await(t)
	if !next.at.Equal(instant("2026-10-04T00:04:00Z")) {
		t.Fatalf("missed triggers queued or not skipped: %s", next.at)
	}
	mu.Lock()
	if queries != 2 || maxActive != 1 {
		t.Errorf("rounds reentered: queries=%d maxActive=%d", queries, maxActive)
	}
	mu.Unlock()
	clock.Fire(next)
	<-entered
	stopped := make(chan struct{})
	go func() { p.handle("plugin.quiesce", nil); close(stopped) }()
	<-cancelled
	select {
	case <-stopped:
		t.Error("quiesce returned while host request still active")
	default:
	}
	leaveOnce.Do(func() { close(leave) })
	<-stopped
	mu.Lock()
	hostReleased = true
	if active != 0 || queries != 3 {
		t.Errorf("shutdown state: active=%d queries=%d", active, queries)
	}
	mu.Unlock()
	clock.Set(instant("2026-10-05T00:00:00Z"))
	p.stop()
	mu.Lock()
	defer mu.Unlock()
	if afterRelease != 0 || p.getStatus().Phase != "stopped" {
		t.Fatal("worker accessed released host")
	}
}

func TestReadinessThenReconfigureAndDisableOwnSingleGeneration(t *testing.T) {
	ready := false
	reads := 0
	p, clock := newTestRuntime(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v8/management/plugins" {
			if !ready {
				return response(503, `{}`), nil
			}
			return response(200, `{"plugins":[{"id":"cpa-auto-priority","effective_enabled":true}]}`), nil
		}
		reads++
		return response(200, `{"observed_at":"2026-10-04T00:00:00Z","files":[]}`), nil
	})})
	defer p.stop()
	clock.Set(instant("2026-10-04T12:00:00Z"))
	if err := p.configure(lifecycle(validConfig)); err != nil {
		t.Fatal(err)
	}
	readiness := clock.Await(t)
	if !readiness.at.Equal(instant("2026-10-04T12:00:01Z")) || reads != 0 || p.getStatus().Phase != "waiting" {
		t.Fatal("unready host was treated as initial sync")
	}
	ready = true
	clock.Fire(readiness)
	old := clock.Await(t)
	if reads != 1 {
		t.Fatalf("initial sync count=%d", reads)
	}
	changed := validConfig + "cron: '0 14 * * *'\ntimezone: UTC\n"
	if err := p.configure(lifecycle(changed)); err != nil {
		t.Fatal(err)
	}
	current := clock.Await(t)
	if reads != 2 || !current.at.Equal(instant("2026-10-04T14:00:00Z")) {
		t.Fatalf("cutover schedule=%s reads=%d", current.at, reads)
	}
	close(old.fire) // cancelled generation cannot perform a stale trigger
	if err := p.configure(lifecycle(changed)); err != nil {
		t.Fatal(err)
	}
	clock.Fire(current)
	next := clock.Await(t)
	if reads != 3 || !next.at.Equal(instant("2026-10-05T14:00:00Z")) {
		t.Fatalf("duplicate generation: schedule=%s reads=%d", next.at, reads)
	}
	disabled := strings.Replace(changed, "enabled: true", "enabled: false", 1)
	if err := p.configure(lifecycle(disabled)); err != nil {
		t.Fatal(err)
	}
	clock.Fire(next)
	if reads != 3 || p.getStatus().Phase != "disabled" {
		t.Fatal("disabled configuration retained a background generation")
	}
}
