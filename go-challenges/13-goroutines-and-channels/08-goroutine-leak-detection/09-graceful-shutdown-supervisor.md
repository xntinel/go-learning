# Exercise 9: A Supervisor That Joins Every Background Loop On Shutdown

This is the capstone of the lifecycle theme. A real service runs several named
background loops — a metrics flusher, a session reaper, a heartbeat — and a correct
`Shutdown` must cancel them all and *join* them within a deadline, honestly reporting
any loop that refused to stop in time. This exercise builds that supervisor on
`sync.WaitGroup.Go` (Go 1.25) and proves it under `go.uber.org/goleak`.

This module is self-contained: its own `go mod init`, all code inline, its own demo
and tests. It imports `go.uber.org/goleak`.

## What you'll build

```text
supervisor/                  independent module: example.com/supervisor
  go.mod
  supervisor.go              type Supervisor; Add, Start, Shutdown, Wait
  cmd/
    demo/
      main.go                runnable demo: start three loops, shut down cleanly
  supervisor_test.go         join-all (goleak), timeout identifies stuck loop, idempotent
```

- Files: `supervisor.go`, `cmd/demo/main.go`, `supervisor_test.go`.
- Implement: `Add(name, loop)`, `Start(ctx)` (launch each loop via `sync.WaitGroup.Go`), `Shutdown(ctx)` (cancel the root context, join all loops within the deadline, return an error naming any stuck loop), and `Wait()`.
- Test: start then shutdown joins all loops with zero leaks; a too-short deadline against a stuck loop returns a timeout error identifying it; double shutdown is idempotent.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/supervisor/cmd/demo
cd ~/go-exercises/supervisor
go mod init example.com/supervisor
go get go.uber.org/goleak@v1.3.0
```

### Cancel, then join with a deadline, then tell the truth

`Shutdown` correctness is a superset of leak-freedom, as the concepts file put it.
Three moves, in order:

1. **Cancel.** `Start` derived a cancellable context from the caller's; `Shutdown`
   calls that cancel, signalling every loop at once. Cancellation is only a signal —
   each loop must be selecting on `ctx.Done()` for it to matter.
2. **Join with a deadline.** A background goroutine that does `wg.Wait()` and closes an
   `allDone` channel lets `Shutdown` wait for *all* loops to return, but bounded by the
   caller's `ctx`: `select { case <-allDone: return nil; case <-ctx.Done(): ... }`.
   Signalling stop without joining is the half-shutdown that still leaks; the join is
   what makes shutdown real.
3. **Tell the truth.** If the deadline fires first, `Shutdown` does *not* pretend
   success. It inspects each loop's done channel non-blockingly and returns an
   `errors.Join` of `ErrLoopStuck` errors naming exactly the loops that did not stop.
   An operator then knows precisely what is wedged, instead of seeing a green shutdown
   over a leaking process.

Each loop is launched with `sync.WaitGroup.Go` (Go 1.25), which does the `Add`, runs
the loop, and `Done`s on return — no `Add`/`Done` boilerplate and no Add-after-`go`
race. The loop names are sorted before building the error so the message is
deterministic.

An honest caveat this exercise does not hide: when a loop ignores its context and the
deadline fires, `Shutdown` reports it — but that loop *is still running*. `Shutdown`
surfacing the failure does not magically stop a loop that refuses to stop; that is a
bug in the loop, and the supervisor's job is to name it, not to paper over it.

Create `supervisor.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	// ErrAlreadyStarted is returned by Add or Start after Start has run.
	ErrAlreadyStarted = errors.New("supervisor: already started")
	// ErrDuplicateLoop is returned by Add for a name already registered.
	ErrDuplicateLoop = errors.New("supervisor: duplicate loop")
	// ErrLoopStuck names a loop that did not stop before the Shutdown deadline.
	ErrLoopStuck = errors.New("supervisor: loop did not stop in time")
)

// LoopFunc is a background loop. It must return when ctx is cancelled.
type LoopFunc func(ctx context.Context)

// Supervisor starts named background loops and joins them all on Shutdown.
type Supervisor struct {
	mu      sync.Mutex
	loops   map[string]LoopFunc
	dones   map[string]chan struct{}
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	started bool
	stopped bool
}

// New returns an empty Supervisor.
func New() *Supervisor {
	return &Supervisor{
		loops: make(map[string]LoopFunc),
		dones: make(map[string]chan struct{}),
	}
}

// Add registers a named loop. It must be called before Start.
func (s *Supervisor) Add(name string, loop LoopFunc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return ErrAlreadyStarted
	}
	if _, ok := s.loops[name]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateLoop, name)
	}
	s.loops[name] = loop
	return nil
}

// Start launches every registered loop under a context derived from parent.
func (s *Supervisor) Start(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return ErrAlreadyStarted
	}
	s.started = true
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	for name, loop := range s.loops {
		done := make(chan struct{})
		s.dones[name] = done
		s.wg.Go(func() {
			defer close(done)
			loop(ctx)
		})
	}
	return nil
}

// Shutdown cancels the root context and joins all loops. If ctx expires before
// every loop returns, it returns an errors.Join naming each loop that is still
// running. It is idempotent.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	cancel := s.cancel
	dones := s.dones
	s.mu.Unlock()

	cancel()

	allDone := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
		return nil
	case <-ctx.Done():
	}

	// Deadline hit: name exactly the loops that have not returned.
	names := make([]string, 0, len(dones))
	for name := range dones {
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []error
	for _, name := range names {
		select {
		case <-dones[name]:
		default:
			errs = append(errs, fmt.Errorf("%w: %q", ErrLoopStuck, name))
		}
	}
	return errors.Join(errs...)
}

// Wait blocks until every loop has returned. It is useful in tests to join a
// loop that was released after a timed-out Shutdown.
func (s *Supervisor) Wait() {
	s.wg.Wait()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/supervisor"
)

func main() {
	sup := supervisor.New()
	for _, name := range []string{"flusher", "reaper", "heartbeat"} {
		_ = sup.Add(name, func(ctx context.Context) { <-ctx.Done() })
	}
	_ = sup.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sup.Shutdown(ctx); err != nil {
		fmt.Println("shutdown error:", err)
		return
	}
	fmt.Println("all loops stopped cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
all loops stopped cleanly
```

### The tests

`TestMain` installs `goleak.VerifyTestMain`. `TestJoinsAllLoops` starts three
ctx-respecting loops and asserts `Shutdown` returns nil with no leaked goroutine.
`TestTimeoutIdentifiesStuckLoop` registers one good loop and one that ignores its
context, gives `Shutdown` a short deadline, and asserts the returned error `Is`
`ErrLoopStuck` and names the stuck loop — then releases it and `Wait`s so the suite
leaves nothing behind. `TestDoubleShutdown` checks idempotency, and
`TestAddAfterStart` checks the guard.

Create `supervisor_test.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestJoinsAllLoops(t *testing.T) {
	sup := New()
	for _, name := range []string{"a", "b", "c"} {
		if err := sup.Add(name, func(ctx context.Context) { <-ctx.Done() }); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sup.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestTimeoutIdentifiesStuckLoop(t *testing.T) {
	release := make(chan struct{})
	sup := New()
	if err := sup.Add("good", func(ctx context.Context) { <-ctx.Done() }); err != nil {
		t.Fatalf("Add good: %v", err)
	}
	// The stuck loop ignores ctx: it only returns when released.
	if err := sup.Add("stuck", func(ctx context.Context) { <-release }); err != nil {
		t.Fatalf("Add stuck: %v", err)
	}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := sup.Shutdown(ctx)
	if !errors.Is(err, ErrLoopStuck) {
		t.Fatalf("Shutdown error = %v, want ErrLoopStuck", err)
	}
	if !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("error did not name the stuck loop: %v", err)
	}
	if strings.Contains(err.Error(), "good") {
		t.Fatalf("error wrongly named the good loop: %v", err)
	}

	// Release the stuck loop and join, so the package leaves no goroutine behind.
	close(release)
	sup.Wait()
}

func TestDoubleShutdown(t *testing.T) {
	sup := New()
	if err := sup.Add("a", func(ctx context.Context) { <-ctx.Done() }); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestAddAfterStart(t *testing.T) {
	sup := New()
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sup.Shutdown(context.Background()) })

	if err := sup.Add("late", func(ctx context.Context) { <-ctx.Done() }); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("Add after Start error = %v, want ErrAlreadyStarted", err)
	}
}
```

## Review

The supervisor is correct when `Shutdown` cancels, joins with a deadline, and reports
honestly. `TestJoinsAllLoops` under `VerifyTestMain` proves the clean path leaks
nothing; `TestTimeoutIdentifiesStuckLoop` proves the honest-failure path names the one
stuck loop and not the good one. The `Wait()` after releasing the stuck loop is what
keeps the reproduction clean without hiding the real failure mode.

The mistakes to avoid: never let `Shutdown` return success without joining — a signalled
but un-joined loop is still running. Never let a loop ignore its context; cancellation
cannot force it to stop, and `Shutdown` can only report the leak, not fix it. Launch
loops with `wg.Go` rather than manual `Add`/`Done`. And make the stuck-loop report
deterministic (sorted names, per-loop done inspection) so operators get a stable,
actionable message. Run under `-race`; the maps are mutex-guarded and each loop closes
its own done channel.

## Resources

- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 launch-and-join method.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining the per-loop stuck errors into one.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain`.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the root cancellation the loops watch.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-errgroup-bounded-fanout.md](08-errgroup-bounded-fanout.md) | Next: [10-pprof-goroutine-dump-endpoint.md](10-pprof-goroutine-dump-endpoint.md)
