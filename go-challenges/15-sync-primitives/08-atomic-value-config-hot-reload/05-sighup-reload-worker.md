# Exercise 5: SIGHUP-Triggered Reload: the Unix Daemon Contract

`kill -HUP <pid>` is how operators have asked daemons to re-read their
configuration since long before Kubernetes — nginx, HAProxy, and most Go
sidecars honor it, and orchestrators send it on config rotation. This
exercise builds the worker that owns that contract: SIGHUP re-reads the
config source and swaps the snapshot, SIGTERM/SIGINT drive graceful
shutdown via `signal.NotifyContext`, and the signal channel is buffered so a
HUP arriving mid-reload is never dropped.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.
(Signals are Unix-specific: this module targets Linux and macOS.)

## What you'll build

```text
cfgsighup/                 independent module: example.com/cfgsighup
  go.mod
  config.go                type Config; Manager; Source func type
  worker.go                Worker: NewWorker (fail-fast), Start (arms SIGHUP, returns done chan)
  cmd/
    demo/
      main.go              runnable demo: kill -HUP self, reload lands; kill -TERM self, clean exit
  worker_test.go           self-signaling integration tests: HUP advances version, cancel stops worker
```

- Files: `config.go`, `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: `Worker.Start(ctx)` arming `signal.Notify(hup, syscall.SIGHUP)` on a 1-buffered channel *synchronously* before spawning the loop goroutine, releasing it with `signal.Stop` on exit, and reloading through a `Source` with last-good-config semantics.
- Test: send `syscall.Kill(os.Getpid(), syscall.SIGHUP)` after arming and wait (deadline-bounded) for the version to advance; cancel the context and prove the worker goroutine exits via its done channel; a failing source keeps the old snapshot and counts the error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cfgsighup/cmd/demo
cd ~/go-exercises/cfgsighup
go mod init example.com/cfgsighup
```

### The three signal traps this design defuses

Signal handling in Go looks trivial — `signal.Notify` and a channel — and
almost every first implementation has the same three bugs.

First, the unbuffered channel. `signal.Notify` performs a *non-blocking* send
for each incoming signal: if the channel cannot accept it at that instant,
the signal is dropped, silently. A worker that is mid-reload when the next
SIGHUP arrives will miss it forever with an unbuffered channel. A buffer of 1
is exactly right: one pending HUP is remembered, and because a reload always
reads the *current* config source, ten queued HUPs would do no more than one
— the buffered signal coalesces naturally.

Second, the arming race. If `Start` spawned a goroutine that called
`signal.Notify` asynchronously, a signal sent immediately after `Start`
returned could arrive before the handler is installed — and the default
action for SIGHUP is process termination. So `Start` calls `signal.Notify`
synchronously and only then spawns the loop: after `Start` returns, a HUP is
guaranteed to reach the channel, never the default action. The tests depend
on this ordering when they signal their own process.

Third, the leak on shutdown. The loop's exit path must run `signal.Stop`
(deregistering the channel; once no channels remain registered the default
disposition returns) and must be observable — `Start` returns a `done`
channel that closes when the goroutine has fully exited. That is what a
graceful-shutdown path waits on, and what the leak test asserts.

Shutdown itself uses the other half of the package: `signal.NotifyContext`
wraps SIGTERM/SIGINT into context cancellation, so the same `ctx.Done()`
select arm serves "orchestrator asked us to stop" and "parent context timed
out". The reload source is a `Source func() (*Config, error)` — in production
it re-reads a file or queries a control plane; in this module it is injected,
which keeps the signal plumbing testable and the failure policy explicit: a
source error increments a counter and keeps the last good snapshot, exactly
like the file reloader.

Create `config.go`:

```go
// Package cfgsighup implements the Unix daemon reload contract: SIGHUP
// re-reads the config source and atomically swaps the snapshot,
// SIGTERM/SIGINT (via context cancellation) stop the worker cleanly.
package cfgsighup

import "sync/atomic"

// Config is one immutable configuration snapshot.
type Config struct {
	MaxConnections int
	Version        int
}

// Source produces a fresh candidate config: re-read a file, query a
// control plane. Called once at startup and once per SIGHUP.
type Source func() (*Config, error)

// Manager publishes the current snapshot. Share by pointer; do not copy.
type Manager struct {
	ptr atomic.Pointer[Config]
}

// Get returns the current snapshot (read-only, non-nil after NewWorker).
func (m *Manager) Get() *Config {
	return m.ptr.Load()
}

// Version returns the current snapshot's version.
func (m *Manager) Version() int {
	return m.ptr.Load().Version
}
```

Create `worker.go`:

```go
package cfgsighup

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// Worker owns the SIGHUP contract for one process: each SIGHUP pulls a
// fresh config from the source and swaps the snapshot on success.
type Worker struct {
	mgr     Manager
	src     Source
	reloads atomic.Int64
	errs    atomic.Int64
}

// NewWorker loads the initial config synchronously and fails fast if the
// source cannot produce one: a daemon with no config should die visibly
// at boot, not idle with a nil snapshot.
func NewWorker(src Source) (*Worker, error) {
	cfg, err := src()
	if err != nil {
		return nil, fmt.Errorf("initial config: %w", err)
	}
	w := &Worker{src: src}
	w.mgr.ptr.Store(cfg)
	return w, nil
}

// Manager returns the manager serving the current snapshot.
func (w *Worker) Manager() *Manager {
	return &w.mgr
}

// Reloads reports successful SIGHUP reloads; Errors reports failed ones.
func (w *Worker) Reloads() int64 { return w.reloads.Load() }

// Errors reports reload attempts whose source returned an error.
func (w *Worker) Errors() int64 { return w.errs.Load() }

// Start arms the SIGHUP handler and spawns the worker loop. The handler
// is installed synchronously: once Start returns, a SIGHUP reaches the
// worker instead of the default action (process termination). The channel
// is buffered so a HUP arriving mid-reload is remembered, not dropped.
// The returned channel closes when the loop has exited and the handler
// has been released with signal.Stop.
func (w *Worker) Start(ctx context.Context) <-chan struct{} {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer signal.Stop(hup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				w.reload()
			}
		}
	}()
	return done
}

func (w *Worker) reload() {
	cfg, err := w.src()
	if err != nil {
		w.errs.Add(1)
		return // last good config keeps serving
	}
	w.mgr.ptr.Store(cfg)
	w.reloads.Add(1)
}
```

### The runnable demo

The demo is a daemon exercising its own contract: it arms the worker, sends
itself `SIGHUP` (exactly what `kill -HUP $(pidof demo)` would do from a
shell), watches the version advance, then sends itself `SIGTERM`, which
`signal.NotifyContext` converts into cancellation — the worker drains and
the process exits cleanly instead of dying on the default action.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"example.com/cfgsighup"
)

func main() {
	var version atomic.Int64
	source := func() (*cfgsighup.Config, error) {
		v := int(version.Add(1))
		return &cfgsighup.Config{MaxConnections: 100 * v, Version: v}, nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	w, err := cfgsighup.NewWorker(source)
	if err != nil {
		panic(err)
	}
	done := w.Start(ctx)
	m := w.Manager()
	fmt.Printf("serving v=%d max=%d\n", m.Version(), m.Get().MaxConnections)

	// An operator runs: kill -HUP <pid>. We do it to ourselves.
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		panic(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Version() != 2 {
		if time.Now().After(deadline) {
			panic("reload never landed")
		}
		time.Sleep(time.Millisecond)
	}
	fmt.Printf("SIGHUP reloaded v=%d max=%d\n", m.Version(), m.Get().MaxConnections)

	// The orchestrator stops us: kill -TERM <pid>.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		panic(err)
	}
	<-ctx.Done()
	<-done
	fmt.Println("SIGTERM received: worker stopped, exiting cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
serving v=1 max=100
SIGHUP reloaded v=2 max=200
SIGTERM received: worker stopped, exiting cleanly
```

### Tests

These are integration-style tests that signal their own process — safe
precisely because `Start` arms the handler before returning. They
deliberately do *not* run in parallel with each other: signal disposition is
process-global state, and interleaving arm/disarm across tests would make
delivery ambiguous. Each test sends a signal only while its worker is armed
and waits (deadline-bounded) for the observable effect before finishing, so
no signal is ever in flight when `signal.Stop` runs.

Create `worker_test.go`:

```go
package cfgsighup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func countingSource() Source {
	var n atomic.Int64
	return func() (*Config, error) {
		v := int(n.Add(1))
		return &Config{MaxConnections: 100 * v, Version: v}, nil
	}
}

func TestSighupTriggersReload(t *testing.T) {
	w, err := NewWorker(countingSource())
	if err != nil {
		t.Fatal(err)
	}
	w.Start(t.Context())
	m := w.Manager()
	if m.Version() != 1 {
		t.Fatalf("initial Version = %d", m.Version())
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "SIGHUP reload to land", func() bool { return m.Version() == 2 })

	if got := m.Get().MaxConnections; got != 200 {
		t.Fatalf("reloaded MaxConnections = %d, want 200", got)
	}
	if w.Reloads() != 1 || w.Errors() != 0 {
		t.Fatalf("Reloads=%d Errors=%d, want 1,0", w.Reloads(), w.Errors())
	}
}

func TestFailingSourceKeepsLastGood(t *testing.T) {
	var calls atomic.Int64
	src := func() (*Config, error) {
		if calls.Add(1) == 1 {
			return &Config{MaxConnections: 100, Version: 1}, nil
		}
		return nil, errors.New("control plane unreachable")
	}

	w, err := NewWorker(src)
	if err != nil {
		t.Fatal(err)
	}
	w.Start(t.Context())
	m := w.Manager()
	old := m.Get()

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reload error to be counted", func() bool { return w.Errors() == 1 })

	if m.Get() != old {
		t.Fatal("snapshot changed after failed reload; want last good config")
	}
	if w.Reloads() != 0 {
		t.Fatalf("Reloads = %d, want 0", w.Reloads())
	}
}

func TestNewWorkerFailsFastWithoutConfig(t *testing.T) {
	src := func() (*Config, error) { return nil, errors.New("no config anywhere") }
	if _, err := NewWorker(src); err == nil {
		t.Fatal("NewWorker succeeded with a failing source; want fail-fast error")
	}
}

func TestCancelStopsWorkerNoLeak(t *testing.T) {
	w, err := NewWorker(countingSource())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := w.Start(ctx)

	cancel()
	select {
	case <-done:
		// worker goroutine exited and signal.Stop ran
	case <-time.After(2 * time.Second):
		t.Fatal("worker goroutine leaked after context cancellation")
	}
}

func ExampleNewWorker() {
	src := func() (*Config, error) {
		return &Config{MaxConnections: 100, Version: 1}, nil
	}
	w, _ := NewWorker(src)
	cfg := w.Manager().Get()
	fmt.Println(cfg.MaxConnections, cfg.Version)
	// Output: 100 1
}
```

## Review

The correctness argument has three legs, each with its own test. Delivery:
because `Start` arms the handler synchronously, `TestSighupTriggersReload`
may signal immediately after it returns and the HUP must land — if this test
ever kills the test process instead, the arming race has been reintroduced.
Availability: `TestFailingSourceKeepsLastGood` asserts pointer identity on
the snapshot across a failed reload, the same last-good-config policy as the
file poller. Lifecycle: `TestCancelStopsWorkerNoLeak` proves cancellation
actually terminates the goroutine, observed through the `done` channel rather
than assumed.

Two things to keep straight when adapting this to a real daemon. The buffered
channel is not optional decoration — walk the timeline of a HUP arriving
during `w.reload()` and you will see the unbuffered version drops it, which
in production means "the operator's config push was ignored, silently, only
sometimes". And keep the SIGHUP worker's channel separate from the
`NotifyContext` used for SIGTERM/SIGINT: reload and shutdown are different
contracts with different consumers, and multiplexing them through one channel
reintroduces the ordering ambiguity this design removes. Verify with
`go test -count=1 -race ./...`.

## Resources

- [os/signal package](https://pkg.go.dev/os/signal) — Notify's non-blocking send rule, Stop, and NotifyContext.
- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the swap the signal triggers.
- [nginx: controlling nginx](https://nginx.org/en/docs/control.html) — the reference SIGHUP reload contract this module imitates.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-file-poll-hot-reloader.md](04-file-poll-hot-reloader.md) | Next: [06-validated-cas-versioned-updates.md](06-validated-cas-versioned-updates.md)
