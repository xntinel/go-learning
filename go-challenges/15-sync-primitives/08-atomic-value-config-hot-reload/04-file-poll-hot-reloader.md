# Exercise 4: File-Watching Hot Reloader with Last-Good-Config Semantics

A config file on disk is still the most common config source in production —
a ConfigMap mount, a deploy artifact, an operator-edited JSON. This exercise
builds the background goroutine that watches it: poll the file's mtime on a
ticker, parse and validate on change, and atomically swap the snapshot only
on success, so a malformed deploy keeps the fleet serving the last good
config instead of crashing it.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cfgfile/                   independent module: example.com/cfgfile
  go.mod
  config.go                type Config (JSON-tagged); validate; Manager
  reloader.go              Reloader: New (fail-fast initial load), Run (ticker poll loop)
  cmd/
    demo/
      main.go              runnable demo: live swap, then a corrupt write that is survived
  reloader_test.go         t.TempDir + t.Context tests: swap lands, corrupt file survived, clean shutdown
```

- Files: `config.go`, `reloader.go`, `cmd/demo/main.go`, `reloader_test.go`.
- Implement: `New(path, interval)` doing a fail-fast initial load, and `Run(ctx)` polling `os.Stat` mtime+size on a `time.Ticker`, swapping the snapshot only when parse and validation both succeed, counting failures in an `atomic.Int64`.
- Test: real files in `t.TempDir()`, the reloader running under `t.Context()`; a rewrite lands as a version bump, a corrupt rewrite leaves the old snapshot serving and bumps the error counter, cancellation stops the goroutine.
- Verify: `go test -count=1 -race ./...`

### Design: fail fast at boot, never fail at runtime

The reloader has two error policies, and the asymmetry is the heart of the
exercise. At startup (`New`), a bad config file is fatal: the process has
nothing to serve, and starting with a zero-value config would be worse than
not starting — the orchestrator will see the crash-loop and page someone.
At runtime (`Run`), a bad config file is an observable non-event: the last
good snapshot keeps serving, an `atomic.Int64` failure counter increments
(that is what your metrics scraper reads), and the reloader keeps polling so
the *next* good write recovers automatically. A config typo degrades to a
counter blip instead of an outage.

Change detection is `os.Stat` comparing both `ModTime` and size against the
last values seen. Polling mtime is deliberately low-tech: it works on every
OS and filesystem, survives the bind-mount tricks Kubernetes plays with
ConfigMap updates (where inotify watches can silently die because the inode
is swapped), and costs one stat per tick. Two details matter:

- The reloader remembers the mtime of a *failed* parse too. Otherwise every
  tick would re-read and re-count the same bad file, turning one bad deploy
  into a counter that climbs forever and drowns the signal. One bad write,
  one error increment.
- Filesystem timestamps can be coarse (a rewrite within the same granularity
  window keeps the old mtime), which is why size participates in the
  comparison and why the tests below force distinct mtimes with `os.Chtimes`
  instead of trusting the filesystem to be fast enough — determinism in tests
  must never depend on timestamp resolution.

The goroutine's lifecycle is context-driven: `Run(ctx)` blocks until
`ctx.Done()`, and the tests pass `t.Context()`, which the testing package
cancels automatically when the test ends — the loop needs no bespoke stop
channel. Only the reloader goroutine touches `lastMod`/`lastSize`, so they
need no synchronization; everything crossing goroutines (`the snapshot`, the
error counter) is atomic.

Create `config.go`:

```go
// Package cfgfile hot-reloads a JSON config file by polling its mtime and
// atomically swapping an immutable snapshot, with last-good-config
// semantics: a bad write is counted, never served and never fatal.
package cfgfile

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// ErrInvalidConfig reports a parsed config that failed validation.
var ErrInvalidConfig = errors.New("invalid config")

// Config is one immutable configuration snapshot, as stored on disk.
type Config struct {
	MaxConnections int `json:"max_connections"`
	TimeoutMillis  int `json:"timeout_millis"`
	Version        int `json:"version"`
}

func validate(c *Config) error {
	if c.MaxConnections <= 0 {
		return fmt.Errorf("%w: max_connections %d, want > 0", ErrInvalidConfig, c.MaxConnections)
	}
	if c.TimeoutMillis <= 0 {
		return fmt.Errorf("%w: timeout_millis %d, want > 0", ErrInvalidConfig, c.TimeoutMillis)
	}
	if c.Version <= 0 {
		return fmt.Errorf("%w: version %d, want > 0", ErrInvalidConfig, c.Version)
	}
	return nil
}

// Manager publishes the current snapshot. Share by pointer; do not copy.
type Manager struct {
	ptr atomic.Pointer[Config]
}

// Get returns the current snapshot (read-only, non-nil after New).
func (m *Manager) Get() *Config {
	return m.ptr.Load()
}

// Version returns the current snapshot's version.
func (m *Manager) Version() int {
	return m.ptr.Load().Version
}
```

Create `reloader.go`:

```go
package cfgfile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// Reloader polls a JSON config file and swaps the Manager's snapshot when
// the file changes and the new content parses and validates. Failures are
// counted, never served.
type Reloader struct {
	mgr      Manager
	path     string
	interval time.Duration
	errs     atomic.Int64

	// lastMod and lastSize are touched only by the Run goroutine.
	lastMod  time.Time
	lastSize int64
}

// New loads path synchronously and fails fast if the initial config is
// missing, malformed, or invalid: a process with nothing to serve should
// crash-loop visibly at boot, not run on a zero config.
func New(path string, interval time.Duration) (*Reloader, error) {
	r := &Reloader{path: path, interval: interval}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("initial stat: %w", err)
	}
	cfg, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("initial load: %w", err)
	}
	r.mgr.ptr.Store(cfg)
	r.lastMod, r.lastSize = fi.ModTime(), fi.Size()
	return r, nil
}

func load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Manager returns the manager serving the current snapshot.
func (r *Reloader) Manager() *Manager {
	return &r.mgr
}

// Errors reports how many changed-file reloads failed to parse or
// validate since startup. Expose this as a metric and alarm on it.
func (r *Reloader) Errors() int64 {
	return r.errs.Load()
}

// Run polls until ctx is cancelled. It never returns an error: runtime
// reload failures keep the last good snapshot serving and increment the
// error counter instead.
func (r *Reloader) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.pollOnce()
		}
	}
}

func (r *Reloader) pollOnce() {
	fi, err := os.Stat(r.path)
	if err != nil {
		// File temporarily missing mid-deploy: keep serving, try again.
		return
	}
	if fi.ModTime().Equal(r.lastMod) && fi.Size() == r.lastSize {
		return // unchanged
	}
	// Remember this write even if it fails: one bad deploy is one error,
	// not one error per tick.
	r.lastMod, r.lastSize = fi.ModTime(), fi.Size()

	cfg, err := load(r.path)
	if err != nil {
		r.errs.Add(1)
		return // last good config keeps serving
	}
	r.mgr.ptr.Store(cfg)
}
```

### The runnable demo

The demo is the whole operational story in five lines of output: serve v1,
watch a good rewrite land as v2, then survive a corrupt rewrite — still
serving v2, error counter at 1.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"example.com/cfgfile"
)

func write(path, content string, bump time.Duration) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(err)
	}
	// Force a distinct mtime so the demo never depends on filesystem
	// timestamp granularity.
	mod := time.Now().Add(bump)
	if err := os.Chtimes(path, mod, mod); err != nil {
		panic(err)
	}
}

func waitFor(cond func() bool) {
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			panic("condition not met within deadline")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func main() {
	dir, err := os.MkdirTemp("", "cfgfile-demo")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.json")

	write(path, `{"max_connections":100,"timeout_millis":5000,"version":1}`, 0)

	r, err := cfgfile.New(path, 5*time.Millisecond)
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	m := r.Manager()
	fmt.Printf("serving v=%d max=%d\n", m.Version(), m.Get().MaxConnections)

	write(path, `{"max_connections":200,"timeout_millis":3000,"version":2}`, time.Second)
	waitFor(func() bool { return m.Version() == 2 })
	fmt.Printf("reloaded v=%d max=%d\n", m.Version(), m.Get().MaxConnections)

	write(path, `{"max_connections": BROKEN`, 2*time.Second)
	waitFor(func() bool { return r.Errors() == 1 })
	fmt.Printf("corrupt write survived: still v=%d, reload errors=%d\n", m.Version(), r.Errors())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
serving v=1 max=100
reloaded v=2 max=200
corrupt write survived: still v=2, reload errors=1
```

### Tests

Every test writes real files into `t.TempDir()` (cleaned up automatically)
and runs the reloader under `t.Context()` (cancelled automatically). Because
reloads land asynchronously, assertions poll with a deadline — `waitFor` —
rather than sleeping a fixed amount, which is both faster and non-flaky. The
corrupt-write test is the availability contract: old snapshot still serving,
counter at exactly 1, and a subsequent good write recovers.

Create `reloader_test.go`:

```go
package cfgfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, path, content string, bump time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mod := time.Now().Add(bump)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func startReloader(t *testing.T, initial string) (*Reloader, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, initial, 0)
	r, err := New(path, 2*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go r.Run(t.Context())
	return r, path
}

func TestInitialLoadFailsFast(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{"malformed json", `{"max_connections": nope`},
		{"fails validation", `{"max_connections":0,"timeout_millis":1,"version":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.json")
			writeConfig(t, path, tc.content, 0)
			if _, err := New(path, time.Millisecond); err == nil {
				t.Fatal("New succeeded on a bad initial config; want fail-fast error")
			}
		})
	}
}

func TestValidationErrorIsSentinel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"max_connections":-1,"timeout_millis":1,"version":1}`, 0)
	_, err := New(path, time.Millisecond)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want errors.Is(ErrInvalidConfig)", err)
	}
}

func TestRewriteSwapsSnapshot(t *testing.T) {
	t.Parallel()

	r, path := startReloader(t, `{"max_connections":100,"timeout_millis":5000,"version":1}`)
	m := r.Manager()
	if m.Version() != 1 {
		t.Fatalf("initial Version = %d", m.Version())
	}

	writeConfig(t, path, `{"max_connections":200,"timeout_millis":3000,"version":2}`, time.Second)
	waitFor(t, "version 2 to land", func() bool { return m.Version() == 2 })

	got := m.Get()
	if got.MaxConnections != 200 || got.TimeoutMillis != 3000 {
		t.Fatalf("swapped snapshot = %+v", got)
	}
	if r.Errors() != 0 {
		t.Fatalf("Errors = %d after clean reloads", r.Errors())
	}
}

func TestCorruptRewriteKeepsLastGood(t *testing.T) {
	t.Parallel()

	r, path := startReloader(t, `{"max_connections":100,"timeout_millis":5000,"version":1}`)
	m := r.Manager()
	old := m.Get()

	writeConfig(t, path, `{"max_connections": BROKEN`, time.Second)
	waitFor(t, "reload error to be counted", func() bool { return r.Errors() >= 1 })

	if m.Get() != old {
		t.Fatal("snapshot changed after corrupt write; want last good config")
	}
	if r.Errors() != 1 {
		t.Fatalf("Errors = %d; one bad write must count exactly once", r.Errors())
	}

	// The next good write recovers automatically.
	writeConfig(t, path, `{"max_connections":300,"timeout_millis":1000,"version":3}`, 2*time.Second)
	waitFor(t, "version 3 to land", func() bool { return m.Version() == 3 })
}

func TestInvalidRewriteKeepsLastGood(t *testing.T) {
	t.Parallel()

	r, path := startReloader(t, `{"max_connections":100,"timeout_millis":5000,"version":1}`)
	m := r.Manager()

	// Parses fine, fails validation: same policy as corrupt JSON.
	writeConfig(t, path, `{"max_connections":0,"timeout_millis":1,"version":2}`, time.Second)
	waitFor(t, "validation error to be counted", func() bool { return r.Errors() >= 1 })

	if m.Version() != 1 {
		t.Fatalf("Version = %d after invalid write; want 1", m.Version())
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"max_connections":1,"timeout_millis":1,"version":1}`, 0)
	r, err := New(path, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func ExampleNew() {
	dir, _ := os.MkdirTemp("", "cfgfile-example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"max_connections":100,"timeout_millis":5000,"version":1}`), 0o644)

	r, _ := New(path, time.Second)
	cfg := r.Manager().Get()
	fmt.Println(cfg.MaxConnections, cfg.Version)
	// Output: 100 1
}
```

One more test pins the per-write (not per-tick) error accounting.

Append to `reloader_test.go`:

```go
func TestErrorsCountsPerBadWriteNotPerTick(t *testing.T) {
	t.Parallel()

	r, path := startReloader(t, `{"max_connections":1,"timeout_millis":1,"version":1}`)

	writeConfig(t, path, `not json at all`, time.Second)
	waitFor(t, "first error", func() bool { return r.Errors() >= 1 })

	// Give the poller many more ticks over the same bad file; the counter
	// must not climb.
	time.Sleep(30 * time.Millisecond)
	if got := r.Errors(); got != 1 {
		t.Fatalf("Errors = %d after one bad write; want exactly 1", got)
	}
}
```

## Review

The invariant to hold in your head: the snapshot pointer only ever moves from
one *validated* config to another. Every failure path — stat error, read
error, parse error, validation error — leaves the pointer alone, which is why
`TestCorruptRewriteKeepsLastGood` can assert pointer identity (`m.Get() !=
old` compares the pointers) rather than field equality. The per-write error
accounting is the operational subtlety most first implementations get wrong:
if a failed parse does not consume the mtime, the counter climbs every tick
and the metric becomes unreadable; `TestErrorsCountsPerBadWriteNotPerTick`
pins the correct behavior.

The tests never sleep-and-hope: they poll a condition with a hard deadline.
That pattern (`waitFor`) is how you test any asynchronous swap without
flakiness — a fixed sleep is either too short on a loaded CI machine or
wastefully long everywhere else. And note what fail-fast means here: `New`
returning an error on a bad *initial* config is not a violation of
last-good-config semantics — there is no last good config yet, and a visible
crash-loop at boot is the correct signal. Verify with
`go test -count=1 -race ./...`.

## Resources

- [os.Stat and fs.FileInfo](https://pkg.go.dev/os#Stat) — ModTime and Size, the change-detection inputs.
- [encoding/json.Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — parsing the candidate config.
- [time.NewTicker](https://pkg.go.dev/time#NewTicker) — the poll loop's clock; always Stop it.
- [testing.T.Context](https://pkg.go.dev/testing#T.Context) — the auto-cancelled context driving the reloader in tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-atomic-value-legacy-adapter.md](03-atomic-value-legacy-adapter.md) | Next: [05-sighup-reload-worker.md](05-sighup-reload-worker.md)
