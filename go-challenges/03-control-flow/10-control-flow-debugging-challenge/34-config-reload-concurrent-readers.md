# Exercise 34: Graceful Config Reload Races With Active Request Readers

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A service that reloads its configuration on `SIGHUP` without a full
restart earns that ability by swapping an in-memory config pointer:
new requests immediately see the new generation, and — this is the
part that is easy to get right by accident and wrong on purpose — old
requests that already grabbed a reference to the *previous* generation
keep using it safely, because Go's garbage collector will not reclaim
memory still reachable from a live reference. The trap is any resource
attached to that config which is *not* just memory: a real file
descriptor, a database connection pool, a listening socket. Those need
an explicit `Close()` to avoid leaking, and the natural place to call
it is right after swapping in the new config — except a request that
acquired the old config a moment earlier is very possibly still using
that resource when the close happens, turning a routine reload into a
use-after-free against a request that had no idea a reload was even in
progress. This module is fully self-contained: its own `go mod init`,
all code inline, its own demo and tests.

## What you'll build

```text
configmgr/                    independent module: example.com/config-reload-concurrent-readers
  go.mod                       go 1.21
  configmgr.go                  Resource, Config, Manager, New, Acquire, Reload
  cmd/
    demo/
      main.go                    runnable demo: an in-flight request surviving a concurrent reload
  configmgr_test.go               20 in-flight readers racing a reload, plus an eventual-close case
```

- Files: `configmgr.go`, `cmd/demo/main.go`, `configmgr_test.go`.
- Implement: `Manager.Reload(next *Config) error` that swaps the active config and waits for every reader that had already `Acquire`d the previous generation to release it before closing that generation's `Resource`.
- Test: 20 goroutines holding an acquired reference to the old config while `Reload` runs concurrently, asserting the resource is never observed closed while a reader still holds it; a further case confirming the old resource is eventually closed once no readers remain.
- Verify: `go test -count=1 -race ./...`.

### Why swapping the pointer is not enough to make reload safe

The version that ships first gets the pointer swap right — new
requests immediately see the new config, which is the part everyone
remembers to test — but closes the old resource unconditionally right
after:

```go
// BUG: closes the previous generation's Resource immediately after the
// swap, with no regard for requests that acquired it moments earlier and
// have not finished using it yet.
func (m *Manager) Reload(next *Config) error {
	m.mu.Lock()
	old := m.current
	m.current = next
	m.mu.Unlock()

	return old.Resource.Close()
}
```

This passes any test that reloads between requests, sequentially,
because there is never an in-flight reader to race against. Under real
traffic, `SIGHUP` does not politely wait for a quiet moment: a request
handler can call `Acquire`, get a reference to generation 1, and be
partway through using its `Resource` — the exact millisecond an
operator's reload script fires. `Reload` swaps `current` to generation
2 (correct — new requests are now isolated from the in-flight one) and
immediately closes generation 1's `Resource` (incorrect — that
handler's reference is still live and still using it). The handler's
next call into the resource fails, not because anything about the
request itself was wrong, but because the ground it was standing on
was pulled out from underneath it by an operation it has no visibility
into and no way to defend against. This is a genuine use-after-free at
the application level: the *memory* backing `old` survives (Go's GC
sees the handler's reference), but the *resource* it owns has already
been explicitly torn down.

The fix tracks in-flight readers per generation with a `sync.WaitGroup`
and makes `Reload` wait for that count to drain before closing
anything:

```go
func (m *Manager) Reload(next *Config) error {
	m.mu.Lock()
	old := m.current
	oldWG := m.wg
	m.current = next
	m.wg = &sync.WaitGroup{}
	m.mu.Unlock()

	oldWG.Wait() // block until every in-flight reader of `old` has released it
	return old.Resource.Close()
}
```

`Acquire` registers itself (`wg.Add(1)`) on the *current* generation's
`WaitGroup` before returning it, all under the same mutex that
`Reload` uses to read `current` and `wg` together — so a reader that
acquires generation 1 either does so entirely before `Reload` swaps to
generation 2 (and is counted in `oldWG`), or entirely after (and is
counted in the *new* generation's fresh `WaitGroup` instead). There is
no window where a reader could acquire generation 1 without being
tracked by whichever `WaitGroup` corresponds to it at that instant.

Create `configmgr.go`:

```go
package configmgr

import (
	"errors"
	"sync"
)

// errClosed is returned by Resource.Use once Close has been called.
var errClosed = errors.New("configmgr: resource used after reload closed it")

// Resource models something a Config owns that must be explicitly closed
// on reload -- a real file descriptor, a DB connection pool, a listening
// socket -- unlike the Config struct itself, whose memory the garbage
// collector reclaims safely whenever the last reference disappears.
type Resource struct {
	mu     sync.Mutex
	closed bool
}

// Close marks the resource closed. Idempotent.
func (r *Resource) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

// Use reports an error if the resource has already been closed.
func (r *Resource) Use() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errClosed
	}
	return nil
}

// Config is one generation of configuration, holding a Resource that must
// be closed once no request is still using this generation.
type Config struct {
	Version  int
	Resource *Resource
}

// Manager holds the active Config and lets requests borrow a reference to
// it that survives a concurrent reload. A reload must not close the old
// Config's Resource until every request that already Acquired that
// generation has Released it.
type Manager struct {
	mu      sync.Mutex
	current *Config
	wg      *sync.WaitGroup // tracks in-flight readers of `current`
}

// New creates a Manager with initial as the active Config.
func New(initial *Config) *Manager {
	return &Manager{current: initial, wg: &sync.WaitGroup{}}
}

// Acquire returns the active Config and registers the caller as an
// in-flight reader of it. The caller must call the returned release func
// when done with it.
func (m *Manager) Acquire() (*Config, func()) {
	m.mu.Lock()
	cfg := m.current
	wg := m.wg
	wg.Add(1)
	m.mu.Unlock()

	return cfg, wg.Done
}

// Reload swaps in next as the active Config, then waits for every reader
// that had already Acquired the previous Config to release it before
// closing the previous Config's Resource -- so a request that grabbed the
// old Config right before reload can still safely finish using it.
func (m *Manager) Reload(next *Config) error {
	m.mu.Lock()
	old := m.current
	oldWG := m.wg
	m.current = next
	m.wg = &sync.WaitGroup{}
	m.mu.Unlock()

	oldWG.Wait() // block until every in-flight reader of `old` has released it
	return old.Resource.Close()
}
```

### The runnable demo

A request goroutine acquires the current config, then does 20ms of
simulated work; the main goroutine waits for the acquire to happen and
then triggers a reload concurrently with that work. Because `Reload`
blocks on the request's in-flight `WaitGroup`, the request always
finishes using its resource before the old one is closed — the print
order below is fully deterministic, not a lucky race.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/config-reload-concurrent-readers"
)

func main() {
	initial := &configmgr.Config{Version: 1, Resource: &configmgr.Resource{}}
	mgr := configmgr.New(initial)

	// A request acquires the current (v1) config and holds it while doing
	// some work, simulating a handler that is mid-flight when SIGHUP
	// arrives.
	var wg sync.WaitGroup
	wg.Add(1)
	requestStarted := make(chan struct{})
	requestDone := make(chan struct{})
	go func() {
		defer wg.Done()
		cfg, release := mgr.Acquire()
		defer release()
		close(requestStarted)
		fmt.Println("request: acquired config version", cfg.Version)

		time.Sleep(20 * time.Millisecond) // simulated request work

		if err := cfg.Resource.Use(); err != nil {
			fmt.Println("request: resource use FAILED:", err)
		} else {
			fmt.Println("request: resource still usable after reload")
		}
		close(requestDone)
	}()

	<-requestStarted // ensure the request has acquired v1 before we reload

	next := &configmgr.Config{Version: 2, Resource: &configmgr.Resource{}}
	fmt.Println("reload: swapping to config version", next.Version)
	if err := mgr.Reload(next); err != nil {
		fmt.Println("reload: error closing old resource:", err)
	} else {
		fmt.Println("reload: old resource closed cleanly, after the request finished")
	}

	<-requestDone
	wg.Wait()
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
request: acquired config version 1
reload: swapping to config version 2
request: resource still usable after reload
reload: old resource closed cleanly, after the request finished
```

### Tests

`TestReloadWaitsForInFlightReadersBeforeClosing` is the concurrency
case: 20 goroutines acquire the config and hold their reference on a
channel-controlled barrier while `Reload` runs concurrently in its own
goroutine; only after every reader has used its resource and released
does the test let `Reload` proceed to completion, asserting zero
resource-use failures throughout. `TestReloadClosesOldResourceOnceReadersRelease`
is the other half of the contract: once no readers remain, the old
resource genuinely does get closed, so reload never leaks it.

Create `configmgr_test.go`:

```go
package configmgr

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestReloadWaitsForInFlightReadersBeforeClosing is the concurrency case:
// many readers acquire the current config and hold their reference while
// Reload runs concurrently. Reload must not close the old Resource until
// every one of those readers has released it -- a reload that closes
// immediately after swapping would let some readers observe an already
// closed resource while they still believed they held a valid reference.
func TestReloadWaitsForInFlightReadersBeforeClosing(t *testing.T) {
	mgr := New(&Config{Version: 1, Resource: &Resource{}})

	var useErrs int64
	var wg sync.WaitGroup

	const readers = 20
	acquired := make(chan struct{}, readers)
	release := make(chan struct{})
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			cfg, done := mgr.Acquire()
			acquired <- struct{}{}
			<-release // hold the reference until told to proceed
			if err := cfg.Resource.Use(); err != nil {
				atomic.AddInt64(&useErrs, 1)
			}
			done()
		}()
	}

	for i := 0; i < readers; i++ {
		<-acquired
	}

	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- mgr.Reload(&Config{Version: 2, Resource: &Resource{}})
	}()

	// Give Reload a moment to reach its wait point before letting readers
	// proceed, so this exercises the actual "reload is blocked on
	// in-flight readers" window rather than racing past it.
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()

	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatalf("Reload() error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reload did not return after every reader released its reference")
	}

	if got := atomic.LoadInt64(&useErrs); got != 0 {
		t.Fatalf("resource use failed %d/%d times while a reader still held the old config, want 0 (reload closed it too early)", got, readers)
	}
}

// TestReloadClosesOldResourceOnceReadersRelease confirms the other half of
// the contract: the old resource IS eventually closed once no readers
// remain, so reload does not leak it forever.
func TestReloadClosesOldResourceOnceReadersRelease(t *testing.T) {
	initial := &Config{Version: 1, Resource: &Resource{}}
	mgr := New(initial)

	if err := mgr.Reload(&Config{Version: 2, Resource: &Resource{}}); err != nil {
		t.Fatalf("Reload() error = %v, want nil", err)
	}
	if err := initial.Resource.Use(); err == nil {
		t.Fatal("initial.Resource.Use() = nil error after Reload, want the resource to be closed once no readers remain")
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Reload` is correct when it never closes a generation's `Resource`
while any reader that acquired that exact generation is still using
it — proven with a test that deliberately holds 20 readers open on a
barrier so `Reload` is forced to genuinely wait, not just usually get
away with racing ahead. The mistake this design avoids is trusting the
garbage collector to make a config swap fully safe: the GC keeps the
`Config` struct itself alive for as long as any reader references it,
but that says nothing about resources the struct merely *points to*
that need an explicit, one-time `Close()` — those need their own
lifetime tracked independently of Go's memory lifetime. Pairing every
generation with its own `sync.WaitGroup`, swapped atomically alongside
the config pointer under one mutex, turns "wait for readers to finish"
from a hopeful assumption into a mechanism: `Acquire` and `Reload`
agree, by construction, on exactly which generation's counter every
in-flight reader belongs to.

## Resources

- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — tracking in-flight readers of a specific generation so a close can wait for them to finish.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — reading `current` and `wg` together atomically, so a reader is never left uncounted by any generation's WaitGroup.
- [nginx: Controlling nginx / reloading configuration](https://nginx.org/en/docs/control.html) — the production pattern of workers finishing in-flight requests against their old configuration before exiting on reload.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-cache-double-check-locking-race.md](33-cache-double-check-locking-race.md) | Next: [../../04-functions/01-function-declaration-and-multiple-return-values/00-concepts.md](../../04-functions/01-function-declaration-and-multiple-return-values/00-concepts.md)
