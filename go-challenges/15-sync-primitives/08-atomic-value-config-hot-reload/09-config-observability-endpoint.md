# Exercise 9: Config Introspection Endpoint: Version, Staleness, Last Error

Hot reload without observability is a guessing game: the on-call engineer
pushes a config and has no way to ask a pod "did it land?". This exercise
builds the operational half of the lesson — a `/debug/config` JSON endpoint
reporting the active version, the config source, time since the last
successful reload, and the last reload error, plus a readiness rule that
flips the pod unready when config goes stale.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cfgdebug/                  independent module: example.com/cfgdebug
  go.mod
  tracker.go               Tracker: one atomic reload-state snapshot; RecordSuccess/RecordFailure;
                           Status; DebugHandler; ReadyHandler; injected clock
  cmd/
    demo/
      main.go              runnable demo: a healthy pod, a failed push, then staleness flips readiness
  tracker_test.go          httptest + fake-clock tests for both endpoints and all transitions
```

- Files: `tracker.go`, `cmd/demo/main.go`, `tracker_test.go`.
- Implement: a `Tracker` publishing one `atomic.Pointer` snapshot of (config, last-reload time, last error), `RecordSuccess`/`RecordFailure` called by the reload path, a `Status` JSON view, `DebugHandler` and `ReadyHandler` (200/503 on the staleness rule), with the clock injected as a `func() time.Time` field.
- Test: httptest asserts the JSON body after simulated successful and failed reloads (deterministic via the fake clock); readiness flips 200 to 503 when staleness exceeds the threshold, when config was never loaded, and back to 200 on recovery.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cfgdebug/cmd/demo
cd ~/go-exercises/cfgdebug
go mod init example.com/cfgdebug
```

### One snapshot for the status, too

The tempting layout is three parallel atomics — a config pointer, an
`atomic.Int64` of last-reload nanos, an error pointer — but then
`/debug/config` reads three fields that can interleave with a reload and
report a chimera: the new version with the old error. The fix is the same
idiom this whole lesson teaches, applied to the status itself: bundle
`(config, lastReload, lastErr)` into one immutable `reloadState` and publish
it behind a single `atomic.Pointer`. The debug handler `Load`s once and
serializes a coherent view. `RecordSuccess` and `RecordFailure` derive the
next state from the current one — a Load-then-Store, safe here because
recording is called only from the single reload goroutine (the same
single-writer assumption every reloader in this chapter runs under; if you
ever have competing writers, exercise 6's CAS loop is the upgrade).

Semantics are chosen for 3 a.m. legibility:

- `seconds_since_reload` counts from the last *successful* reload. A failing
  reload loop must look increasingly stale, not freshly active — this is the
  number an alert should watch.
- `RecordFailure` keeps the served config and the last-success time,
  and sets `last_error`. `RecordSuccess` clears it. So `last_error` always
  answers "is the most recent attempt bad?" rather than accumulating
  history.
- Readiness is `ready = loaded AND fresh`: a pod that never loaded config or
  whose config is older than the threshold reports 503, which tells the
  orchestrator to stop routing to it — turning "my config pushes silently
  stopped landing on this pod" from an invisible drift into a visible,
  self-healing event.

The clock is a `now func() time.Time` field defaulting to `time.Now`. Tests
(and the demo) inject a fake and move it by hand, which makes "advance 10
minutes and the pod goes unready" an exact assertion instead of a sleep.

Create `tracker.go`:

```go
// Package cfgdebug exposes hot-reload health over HTTP: the active config
// version, time since the last successful reload, the last reload error,
// and a staleness-driven readiness signal.
package cfgdebug

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// Config is one immutable configuration snapshot.
type Config struct {
	MaxConnections int
	Version        int
}

// reloadState is one immutable observation of the reload pipeline. It is
// published as a whole so the debug endpoint never reports a mix of two
// reloads' fields.
type reloadState struct {
	cfg        *Config
	lastReload time.Time // zero: never successfully loaded
	lastErr    string    // empty: most recent attempt succeeded
}

// Tracker records reload outcomes and serves them for introspection.
// RecordSuccess and RecordFailure must be called from a single reload
// goroutine (they derive the next state from the current one). Share the
// Tracker by pointer; do not copy it.
type Tracker struct {
	source     string
	staleAfter time.Duration
	now        func() time.Time
	state      atomic.Pointer[reloadState]
}

// New returns a Tracker for the given config source (a path or URL,
// reported verbatim in the status). A pod whose last successful reload is
// older than staleAfter reports not-ready. now == nil means time.Now.
func New(source string, staleAfter time.Duration, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	t := &Tracker{source: source, staleAfter: staleAfter, now: now}
	t.state.Store(&reloadState{})
	return t
}

// Get returns the active config, or nil if none ever loaded.
func (t *Tracker) Get() *Config {
	return t.state.Load().cfg
}

// RecordSuccess publishes cfg as the active config, stamps the reload
// time, and clears the last error.
func (t *Tracker) RecordSuccess(cfg *Config) {
	t.state.Store(&reloadState{cfg: cfg, lastReload: t.now()})
}

// RecordFailure keeps the served config and last-success time and records
// the error: a bad push is an observable non-event.
func (t *Tracker) RecordFailure(err error) {
	cur := t.state.Load()
	t.state.Store(&reloadState{cfg: cur.cfg, lastReload: cur.lastReload, lastErr: err.Error()})
}

// Status is the JSON shape served at /debug/config.
type Status struct {
	Version            int     `json:"version"`
	Source             string  `json:"source"`
	SecondsSinceReload float64 `json:"seconds_since_reload"` // -1: never loaded
	LastError          string  `json:"last_error,omitempty"`
	Ready              bool    `json:"ready"`
}

// Status returns one coherent observation of the reload pipeline.
func (t *Tracker) Status() Status {
	s := t.state.Load() // one Load: version, time and error are from the same reload
	st := Status{Source: t.source, SecondsSinceReload: -1, LastError: s.lastErr}
	if s.cfg != nil {
		st.Version = s.cfg.Version
	}
	if !s.lastReload.IsZero() {
		age := t.now().Sub(s.lastReload)
		st.SecondsSinceReload = age.Seconds()
		st.Ready = age <= t.staleAfter
	}
	return st
}

// DebugHandler serves the Status as JSON: what an on-call engineer curls
// to answer "did my config push land on this pod?".
func (t *Tracker) DebugHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(t.Status()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// ReadyHandler serves the readiness rule: 200 while config is loaded and
// fresh, 503 when it never loaded or went stale.
func (t *Tracker) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := t.Status()
		if !st.Ready {
			http.Error(w, "config not ready", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
}
```

### The runnable demo

The demo drives a fake clock through an incident: a healthy reload at T+0, a
failed push at T+2m (error visible, still ready — the old config is fresh
enough), then silence until T+12m, where staleness crosses the 5-minute
threshold and readiness flips to 503.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/cfgdebug"
)

func main() {
	base := time.Date(2026, 7, 3, 3, 0, 0, 0, time.UTC)
	cur := base
	tr := cfgdebug.New("/etc/svc/config.json", 5*time.Minute, func() time.Time { return cur })

	curl := func(h http.Handler) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		body, _ := io.ReadAll(rec.Result().Body)
		return rec.Code, string(body)
	}

	tr.RecordSuccess(&cfgdebug.Config{MaxConnections: 100, Version: 1})
	_, body := curl(tr.DebugHandler())
	fmt.Print("T+0m  /debug/config: ", body)

	cur = base.Add(2 * time.Minute)
	tr.RecordFailure(errors.New("parse config: unexpected EOF"))
	_, body = curl(tr.DebugHandler())
	fmt.Print("T+2m  /debug/config: ", body)
	code, _ := curl(tr.ReadyHandler())
	fmt.Printf("T+2m  /readyz: %d\n", code)

	cur = base.Add(12 * time.Minute)
	code, _ = curl(tr.ReadyHandler())
	fmt.Printf("T+12m /readyz: %d (no successful reload for 12m > 5m threshold)\n", code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
T+0m  /debug/config: {"version":1,"source":"/etc/svc/config.json","seconds_since_reload":0,"ready":true}
T+2m  /debug/config: {"version":1,"source":"/etc/svc/config.json","seconds_since_reload":120,"last_error":"parse config: unexpected EOF","ready":true}
T+2m  /readyz: 200
T+12m /readyz: 503 (no successful reload for 12m > 5m threshold)
```

### Tests

Every test drives the fake clock, so assertions about elapsed time are exact
— no sleeps, no tolerance windows. The httptest assertions decode the JSON
body back into `Status` and compare fields, which also pins the JSON tags as
a wire contract. The readiness table walks the full lifecycle:
never-loaded, fresh, stale, recovered.

Create `tracker_test.go`:

```go
package cfgdebug

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTracker(staleAfter time.Duration) (*Tracker, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 7, 3, 3, 0, 0, 0, time.UTC)}
	return New("/etc/svc/config.json", staleAfter, clk.now), clk
}

func getStatus(t *testing.T, tr *Tracker) Status {
	t.Helper()
	rec := httptest.NewRecorder()
	tr.DebugHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/debug/config status = %d", rec.Code)
	}
	var st Status
	if err := json.NewDecoder(rec.Result().Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

func getReady(t *testing.T, tr *Tracker) int {
	t.Helper()
	rec := httptest.NewRecorder()
	tr.ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code
}

func TestDebugEndpointAfterSuccess(t *testing.T) {
	t.Parallel()

	tr, clk := newTracker(5 * time.Minute)
	tr.RecordSuccess(&Config{MaxConnections: 100, Version: 3})
	clk.advance(2 * time.Minute)

	st := getStatus(t, tr)
	if st.Version != 3 || st.Source != "/etc/svc/config.json" {
		t.Fatalf("status = %+v", st)
	}
	if st.SecondsSinceReload != 120 {
		t.Fatalf("SecondsSinceReload = %v, want 120", st.SecondsSinceReload)
	}
	if st.LastError != "" || !st.Ready {
		t.Fatalf("healthy status = %+v", st)
	}
}

func TestDebugEndpointAfterFailure(t *testing.T) {
	t.Parallel()

	tr, clk := newTracker(5 * time.Minute)
	tr.RecordSuccess(&Config{Version: 3})
	clk.advance(time.Minute)
	tr.RecordFailure(errors.New("parse config: unexpected EOF"))
	clk.advance(time.Minute)

	st := getStatus(t, tr)
	if st.Version != 3 {
		t.Fatalf("Version = %d; a failed reload must keep serving v3", st.Version)
	}
	if st.LastError != "parse config: unexpected EOF" {
		t.Fatalf("LastError = %q", st.LastError)
	}
	// Staleness counts from the last SUCCESS, not the failed attempt.
	if st.SecondsSinceReload != 120 {
		t.Fatalf("SecondsSinceReload = %v, want 120 (from last success)", st.SecondsSinceReload)
	}
}

func TestRecordSuccessClearsLastError(t *testing.T) {
	t.Parallel()

	tr, _ := newTracker(5 * time.Minute)
	tr.RecordSuccess(&Config{Version: 1})
	tr.RecordFailure(errors.New("boom"))
	tr.RecordSuccess(&Config{Version: 2})

	st := getStatus(t, tr)
	if st.LastError != "" || st.Version != 2 {
		t.Fatalf("after recovery: %+v", st)
	}
}

func TestNeverLoaded(t *testing.T) {
	t.Parallel()

	tr, _ := newTracker(5 * time.Minute)
	st := getStatus(t, tr)
	if st.Ready || st.Version != 0 || st.SecondsSinceReload != -1 {
		t.Fatalf("never-loaded status = %+v", st)
	}
	if tr.Get() != nil {
		t.Fatal("Get before any load must be nil")
	}
}

func TestReadinessLifecycle(t *testing.T) {
	t.Parallel()

	tr, clk := newTracker(5 * time.Minute)

	steps := []struct {
		name string
		act  func()
		want int
	}{
		{"never loaded", func() {}, http.StatusServiceUnavailable},
		{"fresh after first load", func() {
			tr.RecordSuccess(&Config{Version: 1})
		}, http.StatusOK},
		{"still fresh at threshold", func() {
			clk.advance(5 * time.Minute)
		}, http.StatusOK},
		{"stale past threshold", func() {
			clk.advance(time.Second)
		}, http.StatusServiceUnavailable},
		{"failure does not refresh", func() {
			tr.RecordFailure(errors.New("still broken"))
		}, http.StatusServiceUnavailable},
		{"recovers on next success", func() {
			tr.RecordSuccess(&Config{Version: 2})
		}, http.StatusOK},
	}

	for _, s := range steps {
		s.act()
		if got := getReady(t, tr); got != s.want {
			t.Fatalf("%s: /readyz = %d, want %d", s.name, got, s.want)
		}
	}
}

func ExampleTracker_Status() {
	clk := time.Date(2026, 7, 3, 3, 0, 0, 0, time.UTC)
	tr := New("/etc/svc/config.json", 5*time.Minute, func() time.Time { return clk })
	tr.RecordSuccess(&Config{Version: 7})

	st := tr.Status()
	fmt.Println(st.Version, st.Ready, st.SecondsSinceReload)
	// Output: 7 true 0
}
```

## Review

The design decision that carries this module is publishing the *status* as
one immutable snapshot. If you split it into independent atomics, every
individual read is safe yet the endpoint can still emit a state that never
existed — version from reload N, error from reload N-1. The single-pointer
bundle makes `Status()` coherent by construction, at the cost of documenting
the single-writer assumption on `RecordSuccess`/`RecordFailure` (their
Load-then-Store derivation is exactly the pattern that loses updates under
concurrent writers — fine for one reload goroutine, a bug for two).

The readiness semantics repay a close read: `TestReadinessLifecycle` pins
that a *failed* reload does not refresh the staleness clock and that the
boundary is inclusive (fresh at exactly the threshold, stale one second
past). Getting either wrong produces the worst kind of health check — one
that reports ready while the pod drifts, or flaps at the boundary. And note
the fake clock is a plain injected `func() time.Time`, not an interface: one
field, zero abstraction tax, full determinism in tests. Verify with
`go test -count=1 -race ./...`.

## Resources

- [encoding/json: Encoder](https://pkg.go.dev/encoding/json#Encoder) — streaming the status JSON to the response.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — recorder-driven endpoint tests.
- [Kubernetes: liveness and readiness probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/) — the consumer of the 200/503 contract built here.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-feature-flag-http-middleware.md](08-feature-flag-http-middleware.md) | Next: [10-rwmutex-vs-atomic-benchmark.md](10-rwmutex-vs-atomic-benchmark.md)
