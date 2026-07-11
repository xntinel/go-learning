# Exercise 11: Publish a Prefetched Signing Key to Many Awaiters via Close

**Level: Intermediate**

An auth middleware prefetches the JWKS signing key once at startup and every
incoming request must block until that key is ready, then read the exact same
fully-built key. The naive fix — a mutex plus a boolean and a condition variable,
or each request re-fetching — either reintroduces lock contention on the hot path
or fetches the key N times. This exercise builds a one-shot `Future`: the fetcher
writes the result once and closes a done channel, and because close is a broadcast
with a happens-before edge, every current and future awaiter observes the same
published value without a lock.

This module is self-contained: its own module, a `prefetch` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
prefetch/                    independent module: example.com/prefetch
  go.mod                     go 1.26
  prefetch.go                Key, Future; Start, Await, Ready
  cmd/demo/main.go           runnable demo: 50 awaiters all read one published key
  prefetch_test.go           fetched key, shared error, 50-awaiter identity, repeat Await, Ready gating, goleak
```

- Files: `prefetch.go`, `cmd/demo/main.go`, `prefetch_test.go`.
- Implement: `Start(fetch func() (Key, error)) *Future`, `(*Future).Await() (Key, error)`, `(*Future).Ready() bool`.
- Test: Await returns the fetched key; a fetch error reaches every awaiter; 50 concurrent awaiters read byte-identical Key under `-race`; Await after completion returns immediately; Ready is deterministically false-then-true; goleak confirms no leaked goroutines.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/prefetch/cmd/demo
cd ~/go-exercises/prefetch
go mod init example.com/prefetch
go get go.uber.org/goleak
go mod tidy
```

### Publish once, broadcast with close

The problem is a single-assignment result that many goroutines must read exactly
as it was written. A mutex-and-flag design works but forces every awaiter — and
there is one per request — to take a lock just to check "is it ready yet", and it
is easy to leak the memory-visibility guarantee if you read the fields outside the
lock. A channel gives you both the wakeup and the visibility for free:

1. `Start` makes an unbuffered signalling channel `done chan struct{}` and
   launches the fetcher goroutine immediately. The `Future` holds `key`, `err`,
   and `done`.
2. The fetcher runs `fetch`, stores the result into `f.key` and `f.err`, and then
   calls `close(f.done)`. Those field writes happen-before the close, by program
   order in that single goroutine.
3. `Await` does `<-f.done` and then reads `f.key`/`f.err`. The Go memory model
   says the close happens-before every receive that observes it, and the writes
   happened-before the close, so by transitivity each awaiter reads a
   fully-constructed value. No lock is needed on the read side because no field is
   ever written after the close.
4. Close is a *broadcast*: receiving from a closed channel never blocks and never
   drains anything, so all 50 awaiters — and any that arrive after completion —
   proceed past `<-f.done` and read the one published result. There is a single
   publish, not a copy per awaiter racing to fill itself in.
5. `Ready` is a non-blocking `select` on `done` with a `default`, so it reports
   the landed/not-landed state without ever blocking a request goroutine.

The invariant that makes this safe: the fetcher is the sole writer of `key`/`err`,
it writes them exactly once, and it writes them strictly before the close. Every
reader synchronizes through the close. Break either half — write a field after the
close, or read a field without going through `done` — and you have a data race.

Create `prefetch.go`:

```go
// Package prefetch publishes a one-shot background result to many awaiters using
// close-as-broadcast: the fetcher writes the value once, then closes a done
// channel, and every current and future awaiter receives the same published
// result with a memory-model happens-before guarantee.
package prefetch

// Key is a fully-built signing key. Await hands every caller the same Key value,
// so all readers see byte-identical PEM without copying per awaiter.
type Key struct {
	ID  string
	PEM []byte
}

// Future is a single-assignment result cell. The fetcher goroutine stores key
// and err exactly once and then closes done; no field is ever written after the
// close, so no lock is needed on the read side.
type Future struct {
	key  Key
	err  error
	done chan struct{}
}

// Start launches fetch immediately in the background and returns a Future that
// resolves when fetch returns. The result is published by writing key/err and
// then closing done: close is a broadcast, so every awaiter observes it.
func Start(fetch func() (Key, error)) *Future {
	f := &Future{done: make(chan struct{})}
	go func() {
		key, err := fetch()
		// These writes happen-before the close below, and the close
		// happens-before every receive on done, so each awaiter reads a
		// fully-constructed result without additional synchronization.
		f.key = key
		f.err = err
		close(f.done)
	}()
	return f
}

// Await blocks until the result has landed and returns it. It is safe for many
// concurrent callers and returns the same (Key, error) every time: receiving on
// a closed channel never blocks, so all awaiters proceed past the close and read
// the single published value.
func (f *Future) Await() (Key, error) {
	<-f.done
	return f.key, f.err
}

// Ready reports whether the result has landed, without blocking. It is a
// non-blocking select on done: the closed case fires only after the fetcher has
// published and closed.
func (f *Future) Ready() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}
```

### The runnable demo

The demo gates the fetcher on a `release` channel the demo controls, so `Ready`
is deterministically false before release and true after. It launches 50 awaiters
that all block on the same `Future`, then closes `release` to publish once and
checks every awaiter saw the same key ID. Output is order-independent (no
per-goroutine printing, no map iteration), so it is byte-for-byte reproducible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/prefetch"
)

func main() {
	// The fetcher does not complete until the test/demo releases it, so Ready
	// is deterministically false before the release and true after.
	release := make(chan struct{})
	f := prefetch.Start(func() (prefetch.Key, error) {
		<-release
		return prefetch.Key{ID: "kid-2026", PEM: []byte("SIGNING-KEY-PEM")}, nil
	})

	fmt.Println("ready before release:", f.Ready())

	// Fan out many awaiters that all block on the same Future.
	const awaiters = 50
	var wg sync.WaitGroup
	ids := make([]string, awaiters)
	for i := range awaiters {
		wg.Go(func() {
			<-release // ensure every awaiter is parked before the key lands
			key, err := f.Await()
			if err == nil {
				ids[i] = key.ID
			}
		})
	}

	close(release) // publish: unblock the fetcher and every awaiter
	wg.Wait()

	// Every awaiter observed the same published ID.
	allSame := true
	for _, id := range ids {
		if id != "kid-2026" {
			allSame = false
		}
	}
	fmt.Println("ready after await:", f.Ready())
	fmt.Println("all 50 awaiters saw kid-2026:", allSame)

	key, err := f.Await() // Await after completion returns immediately.
	fmt.Printf("await again: id=%s pem=%s err=%v\n", key.ID, key.PEM, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ready before release: false
ready after await: true
all 50 awaiters saw kid-2026: true
await again: id=kid-2026 pem=SIGNING-KEY-PEM err=<nil>
```

### Tests

`TestAwaitReturnsFetchedKey` checks the happy path: Await hands back the fetched
key and a nil error. `TestFetchErrorDeliveredToEveryAwaiter` returns a sentinel
error from `fetch` and asserts all 20 awaiters receive that same error — the error
is published exactly like the key. `TestConcurrentAwaitersShareOnePublish` gates
the fetch on a release channel, launches 50 awaiters, and asserts every result is
byte-identical under `-race`; a single publish observed through the close is what
makes the race detector stay quiet. `TestAwaitAfterCompletionReturnsImmediately`
drives the Future to completion, then calls Await five more times and asserts it
returns the same value each time without blocking. `TestReadyIsDeterministic`
proves Ready is false before the gated fetch lands and true after `Await`
synchronizes — no sleep. `TestMain` wraps the suite in `goleak.VerifyTestMain` so
a leaked fetcher or parked awaiter fails the run.

Create `prefetch_test.go`:

```go
package prefetch

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestAwaitReturnsFetchedKey(t *testing.T) {
	t.Parallel()

	want := Key{ID: "kid-1", PEM: []byte("PEM-BYTES")}
	f := Start(func() (Key, error) { return want, nil })

	got, err := f.Await()
	if err != nil {
		t.Fatalf("Await err = %v, want nil", err)
	}
	if got.ID != want.ID || !bytes.Equal(got.PEM, want.PEM) {
		t.Fatalf("Await = %+v, want %+v", got, want)
	}
}

func TestFetchErrorDeliveredToEveryAwaiter(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("jwks unreachable")
	f := Start(func() (Key, error) { return Key{}, sentinel })

	const awaiters = 20
	var wg sync.WaitGroup
	for range awaiters {
		wg.Go(func() {
			_, err := f.Await()
			if !errors.Is(err, sentinel) {
				t.Errorf("Await err = %v, want %v", err, sentinel)
			}
		})
	}
	wg.Wait()
}

// TestConcurrentAwaitersShareOnePublish gates the fetch on a release channel the
// test controls, then launches 50 awaiters. Under -race, a byte-identical result
// across all of them proves a single publish observed through the close, not
// per-awaiter copies racing.
func TestConcurrentAwaitersShareOnePublish(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	want := Key{ID: "kid-shared", PEM: []byte("ONE-TRUE-KEY")}
	f := Start(func() (Key, error) {
		<-release
		return want, nil
	})

	const awaiters = 50
	var wg sync.WaitGroup
	results := make([]Key, awaiters)
	for i := range awaiters {
		wg.Go(func() {
			key, err := f.Await()
			if err != nil {
				t.Errorf("awaiter %d err = %v", i, err)
			}
			results[i] = key
		})
	}

	close(release) // publish once
	wg.Wait()

	for i, got := range results {
		if got.ID != want.ID || !bytes.Equal(got.PEM, want.PEM) {
			t.Fatalf("awaiter %d saw %+v, want %+v", i, got, want)
		}
	}
}

func TestAwaitAfterCompletionReturnsImmediately(t *testing.T) {
	t.Parallel()

	want := Key{ID: "kid-repeat", PEM: []byte("REPEAT")}
	f := Start(func() (Key, error) { return want, nil })

	first, _ := f.Await() // drive it to completion
	for range 5 {
		got, err := f.Await()
		if err != nil {
			t.Fatalf("repeat Await err = %v, want nil", err)
		}
		if got.ID != first.ID || !bytes.Equal(got.PEM, first.PEM) {
			t.Fatalf("repeat Await = %+v, want %+v", got, first)
		}
	}
}

// TestReadyIsDeterministic gates the fetch on a release channel so Ready is
// false before the result lands and true after, with no sleep-based timing.
func TestReadyIsDeterministic(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	f := Start(func() (Key, error) {
		<-release
		return Key{ID: "kid-ready", PEM: []byte("R")}, nil
	})

	if f.Ready() {
		t.Fatal("Ready = true before release, want false")
	}

	close(release)
	f.Await() // synchronizes: the result has now landed and done is closed

	if !f.Ready() {
		t.Fatal("Ready = false after completion, want true")
	}
}
```

## Review

Correct here means every awaiter — the fifty that are already blocked and any that
arrive later — reads the identical `(Key, error)` the fetcher built, with no data
race and no lock on the request path. The guarantee comes from close-as-broadcast
plus the memory model: the fetcher writes `key`/`err` before `close(done)`, the
close happens-before every receive that observes it, so each `Await` reads a
fully-constructed value by transitivity, and receiving on a closed channel never
blocks so all awaiters proceed. `TestConcurrentAwaitersShareOnePublish` running
under `-race` is the proof — fifty goroutines reading fields published by a
different goroutine would trip the detector if the synchronization were missing,
and byte-identical results prove a single publish rather than per-awaiter copies.
The production bug this prevents is the auth middleware that guards a lazily-loaded
signing key with an unsynchronized boolean: it either races on the key bytes under
load or serializes every request behind a lock; the Future gives lock-free reads
after one publish, which is exactly what a hot request path needs.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before rules for channel close and receive that make the lock-free read sound.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — close-as-broadcast used as a signalling mechanism across many goroutines.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — the leak detector that verifies the fetcher and all awaiters exit.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the send/receive and close semantics this Future is built on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-connection-registry-command-channel.md](10-connection-registry-command-channel.md) | Next: [12-hedged-replica-read-leak-free.md](12-hedged-replica-read-leak-free.md)
