# Exercise 9: TryLock refresh guard: serve stale config while one goroutine revalidates

The `sync.Mutex.TryLock` documentation warns that its correct uses "are rare" —
this module builds one of them. A config/feature-flag holder serves every
caller instantly from an in-memory snapshot; when the snapshot goes stale, one
caller wins a `TryLock` and revalidates against the slow origin while every
other caller keeps serving the stale value instead of piling up. This is
stale-while-revalidate, the pattern behind HTTP's `stale-while-revalidate`
cache directive, applied to in-process state.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
refreshguard/                independent module: example.com/refreshguard
  go.mod                     go 1.26
  holder.go                  type Holder[T any]; New, Get
  cmd/
    demo/
      main.go                runnable demo: fresh, stale-triggered refresh,
                             loader-run count
  holder_test.go             exactly-one-refresh storm, losers-serve-stale with
                             a blocking loader, failed-refresh retry, boundary
                             table, Example
```

- Files: `holder.go`, `cmd/demo/main.go`, `holder_test.go`.
- Implement: a generic `Holder[T]` with two mutexes in distinct roles — a data mutex guarding the snapshot swap and a refresh mutex whose `TryLock` admits exactly one revalidator — plus an injectable clock and loader.
- Test: 200 concurrent `Get` calls past the deadline run the loader exactly once (atomic counter) and every caller returns stale or fresh, never a zero value; a blocking loader proves losers return immediately; a failing loader keeps serving stale and releases admission for a retry.
- Verify: `go test -count=1 -race ./...`

```bash
mkdir -p ~/go-exercises/refreshguard/cmd/demo
cd ~/go-exercises/refreshguard
go mod init example.com/refreshguard
```

### Why this is the rare legitimate TryLock

Contrast with exercise 6, the idempotency store: there, duplicate callers
*need* the result of the in-flight call, so they block on a channel until it is
ready. Here the opposite is true: a caller asking for feature flags has a
perfectly good answer already — the stale snapshot — and blocking hundreds of
request handlers behind one slow origin fetch would turn a 30-second-old config
into a latency spike. The correct behavior for the losers is *not to wait*.
That is precisely what `TryLock` expresses: attempt admission, and if someone
else already holds it, take the fallback path immediately. The stdlib's warning
("use of TryLock is often a sign of a deeper problem") targets code that uses
it to dodge contention on ordinary shared state; here it is deliberate
admission control with a well-defined fallback, which is the sanctioned use.

The design uses two mutexes with strictly separated roles, and keeping those
roles separate is the whole trick:

- `mu` (the data mutex) guards the snapshot fields — value, version, expiry.
  Every critical section under it is a few word copies, nanoseconds long. It is
  never held during I/O.
- `refreshMu` (the admission mutex) is held for the entire revalidation,
  including the slow loader call. That is safe *because nobody ever blocks on
  it*: the only acquisition is `TryLock`, so a long hold cannot queue anyone.

`Get` reads the snapshot under `mu`, and if it is still fresh, returns — the
hot path takes one short lock and no `TryLock` at all. Past the deadline, the
caller tries `refreshMu.TryLock()`. Losers return the stale snapshot they
already read. The winner re-checks staleness under `mu` before loading —
between its first read and its `TryLock`, a previous winner may have already
refreshed, and skipping the re-check would double-fetch. Then it runs the
loader with *no lock but `refreshMu`* held, swaps the snapshot under `mu`, and
the deferred `TryLock` unlock re-opens admission. A successful `TryLock` owes
an `Unlock` exactly like `Lock` does; forgetting it would silently disable
refresh forever — the holder would serve the stale value until process
restart, a bug with no crash and no race report.

Failure policy is a real production decision: if the loader errors, the holder
keeps serving the stale snapshot and releases admission so a later `Get`
retries. Stale flags beat no flags; the alternative (propagate the error to
every caller) turns a transient origin blip into a request-path outage.

Create `holder.go`:

```go
package refreshguard

import (
	"fmt"
	"sync"
	"time"
)

// Holder serves snapshots of slowly-changing configuration with
// stale-while-revalidate semantics: Get never blocks on the loader except for
// the single caller that wins admission to refresh.
type Holder[T any] struct {
	load func() (T, error)
	ttl  time.Duration
	now  func() time.Time

	// refreshMu admits at most one revalidator. It is only ever acquired with
	// TryLock, so holding it across the slow loader call queues nobody.
	refreshMu sync.Mutex

	// mu guards the snapshot fields below. Critical sections under it are a
	// few word copies; it is never held during I/O.
	mu      sync.Mutex
	val     T
	version int64
	expires time.Time
}

// New runs the loader once synchronously and returns a holder whose snapshot
// is valid for ttl. A nil now defaults to time.Now; tests inject a fake clock.
func New[T any](load func() (T, error), ttl time.Duration, now func() time.Time) (*Holder[T], error) {
	if now == nil {
		now = time.Now
	}
	v, err := load()
	if err != nil {
		return nil, fmt.Errorf("initial load: %w", err)
	}
	return &Holder[T]{
		load:    load,
		ttl:     ttl,
		now:     now,
		val:     v,
		version: 1,
		expires: now().Add(ttl),
	}, nil
}

// Get returns the current snapshot and its version. While the snapshot is
// fresh, Get is one short lock. Past the deadline, exactly one caller wins
// refreshMu.TryLock and revalidates; every other caller returns the stale
// snapshot immediately instead of piling onto the origin.
func (h *Holder[T]) Get() (T, int64) {
	h.mu.Lock()
	val, version, expires := h.val, h.version, h.expires
	h.mu.Unlock()

	if h.now().Before(expires) {
		return val, version
	}
	if !h.refreshMu.TryLock() {
		// A refresh is already in flight: serve stale, do not wait.
		return val, version
	}
	defer h.refreshMu.Unlock()

	// Re-check under admission: a previous winner may have refreshed between
	// our snapshot read and our TryLock.
	h.mu.Lock()
	if h.now().Before(h.expires) {
		val, version = h.val, h.version
		h.mu.Unlock()
		return val, version
	}
	h.mu.Unlock()

	fresh, err := h.load()
	if err != nil {
		// Serve stale; the deferred Unlock re-opens admission so a later Get
		// retries the origin.
		return val, version
	}

	h.mu.Lock()
	h.val = fresh
	h.version++
	h.expires = h.now().Add(h.ttl)
	val, version = h.val, h.version
	h.mu.Unlock()
	return val, version
}
```

### The runnable demo

The demo drives the holder with a fake clock and a counting loader. The first
`Get` past the deadline is the winner (there is no contention, so its `TryLock`
succeeds) and returns the freshly loaded value inline; the loader-run count at
the end proves revalidation happened exactly once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"
	"time"

	"example.com/refreshguard"
)

func main() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var offset atomic.Int64 // nanoseconds past base
	now := func() time.Time { return base.Add(time.Duration(offset.Load())) }

	var loads atomic.Int64
	load := func() (string, error) {
		return fmt.Sprintf("flags-v%d", loads.Add(1)), nil
	}

	h, err := refreshguard.New(load, 30*time.Second, now)
	if err != nil {
		panic(err)
	}

	v, ver := h.Get()
	fmt.Printf("fresh:         %s version=%d\n", v, ver)

	offset.Store(int64(31 * time.Second)) // past the deadline
	v, ver = h.Get()                      // this caller wins TryLock, refreshes inline
	fmt.Printf("past deadline: %s version=%d\n", v, ver)

	v, ver = h.Get()
	fmt.Printf("after refresh: %s version=%d\n", v, ver)
	fmt.Printf("loader runs:   %d\n", loads.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fresh:         flags-v1 version=1
past deadline: flags-v2 version=2
after refresh: flags-v2 version=2
loader runs:   2
```

### Tests

`TestRefreshRunsExactlyOnceUnderStorm` is the herd test: 200 goroutines hit a
stale holder and the atomic counter must read exactly 2 (initial load plus one
refresh) — the re-check under admission is what makes that exact.
`TestLosersServeStaleWhileRefreshInFlight` is fully deterministic: the loader
blocks on a channel, so the test *knows* a refresh is in flight when it calls
`Get`, and asserts the call returns the stale snapshot instead of waiting.
`TestFailedRefreshServesStaleAndRetries` pins the failure policy and proves the
admission lock is released after an error. The boundary table pins the freshness
comparison: `now.Before(expires)`, so a snapshot is stale at exactly its expiry
instant.

Create `holder_test.go`:

```go
package refreshguard

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errOrigin = errors.New("config origin unavailable")

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestRefreshRunsExactlyOnceUnderStorm(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	var loads atomic.Int64
	load := func() (string, error) {
		if loads.Add(1) == 1 {
			return "v1", nil
		}
		return "v2", nil
	}
	h, err := New(load, time.Minute, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)

	const callers = 200
	results := make([]string, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			v, _ := h.Get()
			results[i] = v
		}()
	}
	wg.Wait()

	if got := loads.Load(); got != 2 {
		t.Fatalf("loader ran %d times, want 2 (initial + exactly one refresh)", got)
	}
	for i, v := range results {
		if v != "v1" && v != "v2" {
			t.Fatalf("caller %d got %q, want v1 (stale) or v2 (fresh), never zero", i, v)
		}
	}
	if v, ver := h.Get(); v != "v2" || ver != 2 {
		t.Fatalf("after storm Get() = %q version %d, want v2 version 2", v, ver)
	}
}

func TestLosersServeStaleWhileRefreshInFlight(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	load := func() (string, error) {
		if calls.Add(1) == 1 {
			return "v1", nil // synchronous initial load in New
		}
		close(entered) // signal: the winner is inside the loader
		<-release
		return "v2", nil
	}
	h, err := New(load, time.Minute, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)

	winnerDone := make(chan struct{})
	go func() {
		defer close(winnerDone)
		if v, ver := h.Get(); v != "v2" || ver != 2 {
			t.Errorf("winner Get() = %q version %d, want v2 version 2", v, ver)
		}
	}()

	<-entered // refresh is now provably in flight, refreshMu held

	if v, ver := h.Get(); v != "v1" || ver != 1 {
		t.Fatalf("in-flight Get() = %q version %d, want stale v1 version 1", v, ver)
	}

	close(release)
	<-winnerDone

	if v, ver := h.Get(); v != "v2" || ver != 2 {
		t.Fatalf("post-refresh Get() = %q version %d, want v2 version 2", v, ver)
	}
}

func TestFailedRefreshServesStaleAndRetries(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	var calls atomic.Int64
	failing := atomic.Bool{}
	failing.Store(true)
	load := func() (string, error) {
		if calls.Add(1) == 1 {
			return "v1", nil
		}
		if failing.Load() {
			return "", errOrigin
		}
		return "v2", nil
	}
	h, err := New(load, time.Minute, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)

	if v, ver := h.Get(); v != "v1" || ver != 1 {
		t.Fatalf("Get() after failed refresh = %q version %d, want stale v1 version 1", v, ver)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("loader calls = %d, want 2 (the failed attempt ran)", got)
	}

	// Admission was released by the failed attempt: the next Get retries.
	if _, _ = h.Get(); calls.Load() != 3 {
		t.Fatalf("loader calls = %d, want 3 (retry after failure)", calls.Load())
	}

	failing.Store(false)
	if v, ver := h.Get(); v != "v2" || ver != 2 {
		t.Fatalf("Get() after origin recovery = %q version %d, want v2 version 2", v, ver)
	}
}

func TestInitialLoadError(t *testing.T) {
	t.Parallel()

	_, err := New(func() (string, error) { return "", errOrigin }, time.Minute, nil)
	if !errors.Is(err, errOrigin) {
		t.Fatalf("New() error = %v, want errors.Is errOrigin", err)
	}
}

func TestFreshnessBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		advance   time.Duration
		wantLoads int64
	}{
		{"well before deadline", 30 * time.Second, 1},
		{"one nanosecond before", time.Minute - time.Nanosecond, 1},
		{"exactly at deadline", time.Minute, 2},
		{"past deadline", 2 * time.Minute, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := newFakeClock()
			var loads atomic.Int64
			load := func() (string, error) {
				return fmt.Sprintf("v%d", loads.Add(1)), nil
			}
			h, err := New(load, time.Minute, clk.Now)
			if err != nil {
				t.Fatal(err)
			}
			clk.Advance(tt.advance)
			h.Get()
			if got := loads.Load(); got != tt.wantLoads {
				t.Fatalf("loader ran %d times, want %d", got, tt.wantLoads)
			}
		})
	}
}

func ExampleHolder_Get() {
	h, _ := New(func() (string, error) { return "flags-v1", nil }, time.Minute, nil)
	v, version := h.Get()
	fmt.Println(v, version)
	// Output: flags-v1 1
}
```

## Review

The guard is correct when the two mutexes never swap roles: the data mutex is
held only for word-copy critical sections, the admission mutex is acquired only
via `TryLock`, and the loader runs under the admission mutex alone. The
blocking-loader test is the strongest evidence, because it removes timing luck
entirely — the test holds the refresh open and observes a `Get` complete
against it. The exactly-once storm depends on the re-check under admission;
delete that re-check and `loads` drifts above 2 under contention.

The traps: forgetting `Unlock` after a successful `TryLock` (refresh silently
disabled forever — no panic, no race report, just eternally stale config);
running the loader under the *data* mutex, which reintroduces the pile-up this
design exists to prevent; and reaching for this pattern when callers actually
need the fresh result — then exercise 6's blocking singleflight shape is
correct and TryLock is the wrong tool. That judgment call is the module's real
lesson. Run `go test -count=1 -race ./...`.

## Resources

- [`sync.Mutex.TryLock`](https://pkg.go.dev/sync#Mutex.TryLock) — signature and the "correct uses are rare" warning.
- [Go 1.18 release notes](https://go.dev/doc/go1.18) — where `TryLock` landed, with the same caveat.
- [RFC 5861: HTTP stale-while-revalidate](https://www.rfc-editor.org/rfc/rfc5861) — the same pattern at the HTTP caching layer.
- [The Go Memory Model](https://go.dev/ref/mem) — why the snapshot swap under one mutex publishes to all readers.

---

Back to [08-ordered-lock-transfer.md](08-ordered-lock-transfer.md) | Next: [10-sharded-counter-store.md](10-sharded-counter-store.md)
