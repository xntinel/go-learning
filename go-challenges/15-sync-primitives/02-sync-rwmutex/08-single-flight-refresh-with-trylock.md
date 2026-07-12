# Exercise 8: Non-blocking JWKS refresh — TryLock as a single-flight guard

A service verifying JWTs caches the issuer's JWKS document and re-fetches it
periodically — but when the fetch is slow, request goroutines must keep
verifying against the cached keys, not queue behind the refresh. This exercise
builds that refresher with the two disciplines from the concepts file: never
hold the value lock across slow work, and use `TryLock` for its one honest job —
skip-if-busy single-flight.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
jwksrefresh/                 independent module: example.com/jwksrefresh
  go.mod                     module example.com/jwksrefresh
  jwksrefresh.go             type Refresher; Get, Refresh (TryLock single-flight), LastError, Loads
  cmd/
    demo/
      main.go                runnable demo: stale served during a stalled refresh
  jwksrefresh_test.go        channel-stalled loader, single-flight and last-good tests
```

Files: `jwksrefresh.go`, `cmd/demo/main.go`, `jwksrefresh_test.go`.
Implement: `Refresher` holding a `*Document` behind an `RWMutex`; `Get` under `RLock`; `Refresh` guarded by a dedicated `refreshMu.TryLock()` — the loser returns `ErrRefreshInFlight` immediately, the winner runs the slow `Loader` with no value lock held and publishes under a brief `Lock`; loader failures keep the last-good document and are recorded.
Test: N concurrent `Refresh` calls against a channel-stalled loader invoke the loader exactly once while N-1 callers get `ErrRefreshInFlight` without blocking; `Get` during the stall serves the previous document; a loader error preserves the last-good document and surfaces via `LastError`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/02-sync-rwmutex/08-single-flight-refresh-with-trylock/cmd/demo
cd go-solutions/15-sync-primitives/02-sync-rwmutex/08-single-flight-refresh-with-trylock
```

### Two mutexes because there are two resources

The mistake this design refuses to make is using one lock for two different
things. There is the *document* — read on every request, replaced rarely — and
there is the *right to refresh* — held for the full duration of a network
round-trip. Guard both with the same lock and you get the classic outage: the
refresher takes `Lock`, the upstream IdP has a slow day, and every request
goroutine in the process piles up behind a three-second HTTP call.

So the `Refresher` carries two mutexes with disjoint jobs. The `RWMutex` guards
the `doc` and `lastErr` fields and is only ever held for nanoseconds — `Get`
reads the pointer under `RLock`; the winner of a refresh swaps it under a brief
`Lock`. The plain `refreshMu sync.Mutex` guards the *activity* of refreshing,
is held across the whole slow load, and — critically — is never taken by `Get`.
Readers cannot queue behind a refresh because they never touch the lock the
refresh is holding.

### TryLock's one honest job

`Refresh` opens with `if !r.refreshMu.TryLock() { return ErrRefreshInFlight }`.
This is single-flight: when five goroutines decide simultaneously that the
document is stale, one wins the lock and performs the fetch; the other four get
an immediate, non-blocking "already being handled" and go back to serving
requests with the current keys. Serving slightly-stale keys for another second
is the correct trade for an auth path — JWKS rotation is designed around
overlapping validity, and blocking request handlers on a network call is not.

Note what this is *not*: it is not a spin loop, and it is not "TryLock, else
give up forever". The loser returns a sentinel the caller can act on (retry on
its next tick, count a metric). Callers that need "wait for the fresh result"
semantics should use `golang.org/x/sync/singleflight`, which shares the
winner's result with waiters; `TryLock` single-flight is the leaner shape for
"refresh opportunistically, serve stale meanwhile". A failure inside the load
does not poison anything: the last-good document stays published, the error is
recorded (wrapped with `%w`, so `errors.Is` still sees the cause) and returned,
and the next `Refresh` attempt starts clean.

The load counter is an `atomic.Int64` rather than a field under the `RWMutex` —
it is touched while `refreshMu` is held but read by tests at any time, and an
atomic keeps that observation lock-free.

Create `jwksrefresh.go`:

```go
package jwksrefresh

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrRefreshInFlight reports that another goroutine's refresh is already
// running. The caller should keep serving the current document; the sentinel
// exists so callers can distinguish "skipped" from "failed".
var ErrRefreshInFlight = errors.New("refresh already in flight")

// Document is an immutable point-in-time view of the remote key set. A
// published document is never mutated; refreshes replace the whole pointer.
type Document struct {
	Version int
	KeyIDs  []string
}

// Loader fetches the latest document from the remote source (IdP, service
// registry). It may be arbitrarily slow; the Refresher never holds the value
// lock while running it.
type Loader func() (*Document, error)

// Refresher serves a cached document with lock-free-feeling reads and
// opportunistic, single-flight refreshes.
type Refresher struct {
	load Loader

	// refreshMu guards the ACTIVITY of refreshing and is held across the whole
	// slow load. Get never touches it, so readers never queue behind a refresh.
	refreshMu sync.Mutex

	loads atomic.Int64

	// mu guards doc and lastErr and is only ever held for a brief read or swap.
	mu      sync.RWMutex
	doc     *Document
	lastErr error
}

// New returns a Refresher seeded with an initial document.
func New(initial *Document, load Loader) *Refresher {
	return &Refresher{load: load, doc: initial}
}

// Get returns the current document. It never blocks behind an in-flight
// refresh: the value lock is only ever held to read or swap a pointer.
func (r *Refresher) Get() *Document {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.doc
}

// LastError returns the recorded error of the most recent refresh attempt, or
// nil if it succeeded.
func (r *Refresher) LastError() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastErr
}

// Loads reports how many times the loader has been invoked.
func (r *Refresher) Loads() int64 {
	return r.loads.Load()
}

// Refresh runs the loader and publishes its result. If a refresh is already in
// flight it returns ErrRefreshInFlight immediately: the loser serves stale
// rather than queueing. A loader failure keeps the last-good document and is
// both recorded and returned, wrapped.
func (r *Refresher) Refresh() error {
	if !r.refreshMu.TryLock() {
		return ErrRefreshInFlight
	}
	defer r.refreshMu.Unlock()

	r.loads.Add(1)
	doc, err := r.load() // slow work: no value lock held here

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.lastErr = fmt.Errorf("jwks refresh: %w", err)
		return r.lastErr
	}
	r.doc = doc
	r.lastErr = nil
	return nil
}
```

### The runnable demo

The demo stalls the loader on a channel so the interleaving is deterministic: a
background `Refresh` is provably mid-flight when the main goroutine both reads
the stale document instantly and gets `ErrRefreshInFlight` from its own
`Refresh` attempt.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/jwksrefresh"
)

func main() {
	started := make(chan struct{})
	release := make(chan struct{})

	r := jwksrefresh.New(
		&jwksrefresh.Document{Version: 1, KeyIDs: []string{"kid-2025"}},
		func() (*jwksrefresh.Document, error) {
			close(started)
			<-release // simulate a slow IdP round-trip
			return &jwksrefresh.Document{Version: 2, KeyIDs: []string{"kid-2025", "kid-2026"}}, nil
		},
	)

	done := make(chan error)
	go func() { done <- r.Refresh() }()
	<-started // the winner now holds refreshMu inside the loader

	fmt.Printf("during refresh: Get serves version %d\n", r.Get().Version)
	if err := r.Refresh(); errors.Is(err, jwksrefresh.ErrRefreshInFlight) {
		fmt.Printf("concurrent Refresh: %v\n", err)
	}

	close(release)
	if err := <-done; err != nil {
		fmt.Println("refresh failed:", err)
		return
	}
	fmt.Printf("after refresh: Get serves version %d\n", r.Get().Version)
	fmt.Printf("loader invocations: %d\n", r.Loads())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
during refresh: Get serves version 1
concurrent Refresh: refresh already in flight
after refresh: Get serves version 2
loader invocations: 1
```

### Tests

`TestSingleFlightCollapses` is the contract test: it parks the winner inside a
channel-stalled loader, fires eight more `Refresh` calls, and requires that all
eight return `ErrRefreshInFlight` *while the winner is still stalled* — the
`wg.Wait()` before `close(release)` is what proves the losers did not block.
`Get` mid-stall must serve the old version. After release: exactly one loader
invocation, new version visible. `TestLoaderErrorKeepsLastGood` drives a
failing loader and asserts the document survives, the wrapped cause surfaces
through `errors.Is` on both the return value and `LastError`, and a subsequent
success clears the record. `TestConcurrentGetAndRefresh` races readers against
refreshers under `-race` and asserts versions never go backwards — refreshes
are serialized by `refreshMu`, so publication order must be monotonic.

Create `jwksrefresh_test.go`:

```go
package jwksrefresh

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

var errUpstream = errors.New("upstream 503")

func TestSingleFlightCollapses(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})

	r := New(&Document{Version: 1}, func() (*Document, error) {
		close(started)
		<-release
		return &Document{Version: 2, KeyIDs: []string{"kid-2"}}, nil
	})

	winner := make(chan error, 1)
	go func() { winner <- r.Refresh() }()
	<-started // winner holds refreshMu and is stalled inside the loader

	const losers = 8
	errs := make(chan error, losers)
	var wg sync.WaitGroup
	for range losers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- r.Refresh()
		}()
	}
	wg.Wait() // every loser returned while the winner is still stalled
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrRefreshInFlight) {
			t.Fatalf("loser Refresh() = %v, want ErrRefreshInFlight", err)
		}
	}

	if got := r.Get().Version; got != 1 {
		t.Fatalf("Get during stalled refresh = version %d, want stale version 1", got)
	}

	close(release)
	if err := <-winner; err != nil {
		t.Fatalf("winner Refresh() = %v, want nil", err)
	}
	if got := r.Loads(); got != 1 {
		t.Fatalf("loader invoked %d times, want exactly 1", got)
	}
	if got := r.Get().Version; got != 2 {
		t.Fatalf("Get after refresh = version %d, want 2", got)
	}
}

func TestLoaderErrorKeepsLastGood(t *testing.T) {
	t.Parallel()
	fail := true
	r := New(&Document{Version: 7, KeyIDs: []string{"kid-7"}}, func() (*Document, error) {
		if fail {
			return nil, errUpstream
		}
		return &Document{Version: 8}, nil
	})

	err := r.Refresh()
	if !errors.Is(err, errUpstream) {
		t.Fatalf("Refresh() = %v, want wrapped errUpstream", err)
	}
	if got := r.Get().Version; got != 7 {
		t.Fatalf("Get after failed refresh = version %d, want last-good 7", got)
	}
	if !errors.Is(r.LastError(), errUpstream) {
		t.Fatalf("LastError() = %v, want wrapped errUpstream", r.LastError())
	}

	fail = false
	if err := r.Refresh(); err != nil {
		t.Fatalf("recovery Refresh() = %v, want nil", err)
	}
	if got := r.Get().Version; got != 8 {
		t.Fatalf("Get after recovery = version %d, want 8", got)
	}
	if r.LastError() != nil {
		t.Fatalf("LastError() after recovery = %v, want nil", r.LastError())
	}
}

func TestConcurrentGetAndRefresh(t *testing.T) {
	t.Parallel()
	var version atomic.Int64
	r := New(&Document{Version: 0}, func() (*Document, error) {
		return &Document{Version: int(version.Add(1))}, nil
	})

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				r.Refresh() // winners and in-flight losers are both fine here
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			last := -1
			for range 1000 {
				v := r.Get().Version
				if v < last {
					t.Errorf("version went backwards: %d after %d", v, last)
					return
				}
				last = v
			}
		}()
	}
	wg.Wait()
}

func Example() {
	r := New(&Document{Version: 1}, func() (*Document, error) {
		return &Document{Version: 2}, nil
	})

	fmt.Println(r.Get().Version)
	if err := r.Refresh(); err != nil {
		fmt.Println(err)
	}
	fmt.Println(r.Get().Version)
	// Output:
	// 1
	// 2
}
```

## Review

Three properties make this refresher production-shaped, and each maps to a test.
Single-flight: concurrent refresh intent collapses to one loader call, and the
losers return *immediately* — if `TestSingleFlightCollapses` ever hangs at
`wg.Wait()`, someone replaced `TryLock` with `Lock` and the losers are queueing.
Stale-over-stall: `Get` never touches `refreshMu`, so a three-second IdP outage
costs zero request latency. Last-good on failure: an error keeps the published
document, wraps the cause with `%w`, and clears on the next success. The
mistakes to avoid: holding the `RWMutex` write lock across the loader (the
outage shape the concepts file warns about), spinning on `TryLock` (the loser's
job is to walk away, not to retry in a loop), and mutating a published
`*Document` in place — treat it as immutable and replace the pointer. If a
caller genuinely needs to wait for the fresh result instead of serving stale,
that is `x/sync/singleflight`'s job, not a `TryLock` loop. Verify with
`go test -count=1 -race ./...`.

## Resources

- [`sync.Mutex.TryLock`](https://pkg.go.dev/sync#Mutex.TryLock) — the non-blocking acquire, and the docs' warning that most uses of it are a design smell.
- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production primitive when callers must share the winner's result instead of serving stale.
- [JSON Web Key (JWK), RFC 7517](https://datatracker.ietf.org/doc/html/rfc7517) — the key-set document this refresher models, including why rotation tolerates staleness.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-rwmutex-vs-mutex-benchmark-harness.md](09-rwmutex-vs-mutex-benchmark-harness.md)
