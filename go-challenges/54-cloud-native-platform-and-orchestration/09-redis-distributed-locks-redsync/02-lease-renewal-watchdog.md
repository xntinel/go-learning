# Exercise 2: Keep a Lock Alive Across Long Work with a Renewal Watchdog

Some protected work outlives any TTL you would want to set: a cache rebuild that
runs for minutes, a migration, a large reconciliation. The answer is not a huge
TTL (a crashed holder would then block everyone for minutes) but a short TTL plus
a watchdog that renews the lease from a background goroutine — and, decisively,
aborts the work the instant renewal fails.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
watchdog/                  independent module: example.com/watchdog
  go.mod                   go 1.26; requires redsync, go-redis, miniredis
  watchdog.go              Watchdog, New, Guarded (returns ctx+stop), renew; ErrLeaseLost; WithReclaim
  cmd/
    demo/
      main.go              embedded miniredis; long work renews, then loses the lease and aborts
  watchdog_test.go         miniredis tests: renewal keeps ctx alive, lease loss cancels ctx, stop releases, reclaim
```

Files: `watchdog.go`, `cmd/demo/main.go`, `watchdog_test.go`.
Implement: a `Watchdog` that acquires a short-expiry lock and returns a derived context valid only while the lease is provably held; a background `renew` goroutine that calls `ExtendContext` on a ticker at expiry/2 and cancels the context (cause `ErrLeaseLost`) when renewal fails; a `stop` closure that releases the lock and shuts the goroutine down.
Test: with `miniredis`, assert the context stays alive across several intervals while renewal succeeds; that deleting the key cancels the context with cause `ErrLeaseLost`; that `stop` releases the key and the goroutine exits; and that `WithReclaim` (SetNX-on-extend) survives a flush.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/watchdog/cmd/demo
cd ~/go-exercises/watchdog
go mod init example.com/watchdog
go mod edit -go=1.26
go get github.com/go-redsync/redsync/v4@latest
go get github.com/redis/go-redis/v9@latest
go get github.com/alicebob/miniredis/v2@latest
```

### The shape: a context that means "the lease is held"

The design turns "do I still hold the lease?" into "is this context still alive?"
— which is the idiom the rest of your code already understands. `Guarded`
acquires the lock, derives a cancellable context from the parent with
`context.WithCancelCause`, starts a `renew` goroutine, and hands back that context
plus a `stop` closure. The work loop selects on `ctx.Done()`; if renewal ever
fails, the watchdog cancels the context with cause `ErrLeaseLost`, and the work
loop drops out of its `select` and aborts. That is the whole point: the work runs
only while the lease is provably held, and it stops at the moment the lease is
lost rather than pressing on unprotected.

Two subtleties matter. First, renewal runs on its *own* goroutine, never inline
with the work — a long synchronous step must not be able to starve the renewal
ticker. Second, the failure test is the boolean, not a specific error. On a single
node, `ExtendContext` on a lost key returns `(false, err)` where the error is a
`*redsync.ErrTaken` (the touch script saw the key gone); it does not return a
tidy `(false, nil)`. So the watchdog keys off `ok == false` and attaches whatever
error it got as context to the cause. The ticker period is expiry/2 so that a
single missed or slow renewal still leaves half the lease window as slack.

### Cancellation and cleanup ordering

`stop` must both release the lock and guarantee the renewal goroutine has exited
(no leak). It cancels the context with `context.Canceled` first, then blocks on a
`done` channel the goroutine closes on exit, then releases the lock. Because
`context.WithCancelCause` records the *first* cause, a `stop`-initiated
`context.Canceled` wins even if the renew goroutine races to cancel with
`ErrLeaseLost` — so a clean shutdown reports `Canceled`, not a false lease-loss.
The release uses `context.WithoutCancel(parent)` so that unlocking still works
after the derived context is already cancelled. `WithReclaim` threads
`redsync.WithSetNXOnExtend` into the mutex, letting renewal re-create a key that
vanished (a Redis restart) — an availability win that weakens the
continuously-held guarantee, so it is opt-in.

Create `watchdog.go`:

```go
package watchdog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-redsync/redsync/v4"
	goredis "github.com/go-redsync/redsync/v4/redis/goredis/v9"
	goredislib "github.com/redis/go-redis/v9"
)

// ErrLeaseLost is the cause attached to the derived context when the watchdog can
// no longer prove the lease is held.
var ErrLeaseLost = errors.New("watchdog: lease lost")

// Watchdog acquires a short-expiry lock and renews it from a background
// goroutine so work can outlive a single TTL.
type Watchdog struct {
	rs      *redsync.Redsync
	expiry  time.Duration
	reclaim bool
}

// Option configures a Watchdog.
type Option func(*Watchdog)

// WithReclaim enables SetNX-on-extend: renewal re-creates a key that vanished.
// It improves availability but weakens the continuously-held guarantee, so it is
// appropriate for efficiency locks, not correctness locks.
func WithReclaim() Option { return func(w *Watchdog) { w.reclaim = true } }

// New builds a Watchdog over a go-redis client with the given lease TTL.
func New(client goredislib.UniversalClient, expiry time.Duration, opts ...Option) *Watchdog {
	w := &Watchdog{rs: redsync.New(goredis.NewPool(client)), expiry: expiry}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Guarded acquires name and returns a context that stays valid only while the
// lease is provably held, plus a stop closure. A background goroutine renews the
// lease at expiry/2; if a renewal fails (lease lost to a pause, partition, or
// expiry) the context is cancelled with cause ErrLeaseLost. Call stop when the
// work is done to release the lock and shut the renewal goroutine down.
func (w *Watchdog) Guarded(parent context.Context, name string) (context.Context, func(), error) {
	opts := []redsync.Option{redsync.WithExpiry(w.expiry), redsync.WithTries(1)}
	if w.reclaim {
		opts = append(opts, redsync.WithSetNXOnExtend())
	}
	m := w.rs.NewMutex(name, opts...)
	if err := m.LockContext(parent); err != nil {
		return nil, nil, fmt.Errorf("watchdog: acquire %q: %w", name, err)
	}

	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go w.renew(ctx, m, cancel, done)

	stop := func() {
		cancel(context.Canceled)
		<-done
		// Best-effort release after the goroutine has exited; a false result is
		// legitimate if the lease was already lost.
		_, _ = m.UnlockContext(context.WithoutCancel(parent))
	}
	return ctx, stop, nil
}

func (w *Watchdog) renew(ctx context.Context, m *redsync.Mutex, cancel context.CancelCauseFunc, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(w.expiry / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := m.ExtendContext(ctx)
			if !ok {
				cancel(fmt.Errorf("%w: extend failed: %v", ErrLeaseLost, err))
				return
			}
		}
	}
}
```

### The runnable demo

The demo runs an embedded `miniredis` so it needs no external Redis. It holds a
400 ms lease while running three work steps of 300 ms each (well past one TTL, so
the watchdog renews underneath the work), then simulates a partition by deleting
the lock key and shows the work aborting when the lease is lost.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/watchdog"
	"github.com/alicebob/miniredis/v2"
	goredislib "github.com/redis/go-redis/v9"
)

func main() {
	mr, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer mr.Close()

	client := goredislib.NewClient(&goredislib.Options{Addr: mr.Addr()})
	defer client.Close()

	w := watchdog.New(client, 400*time.Millisecond)
	ctx, stop, err := w.Guarded(context.Background(), "cache:rebuild")
	if err != nil {
		panic(err)
	}
	defer stop()
	fmt.Println("lease acquired; starting long work")

	for i := range 3 {
		select {
		case <-ctx.Done():
			fmt.Println("aborting early: lease lost")
			return
		case <-time.After(300 * time.Millisecond):
			fmt.Printf("work step %d done (lease still held)\n", i+1)
		}
	}

	fmt.Println("simulating lease loss")
	mr.Del("cache:rebuild")

	<-ctx.Done()
	if errors.Is(context.Cause(ctx), watchdog.ErrLeaseLost) {
		fmt.Println("work aborted: lease lost")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
lease acquired; starting long work
work step 1 done (lease still held)
work step 2 done (lease still held)
work step 3 done (lease still held)
simulating lease loss
work aborted: lease lost
```

### Tests

The tests use `miniredis`. Its key expiry is driven by `FastForward`/`Del`
rather than the wall clock, so a renewal that resets the TTL keeps the key present
regardless of real time — perfect for asserting renewal without flaky sleeps.
`TestRenewalKeepsLeaseAlive` runs across several renewal intervals and checks the
context stays alive and the key keeps a positive TTL. `TestLeaseLossCancelsContext`
deletes the key out from under the watchdog and asserts the context is cancelled
with cause `ErrLeaseLost`. `TestStopReleasesAndExits` confirms `stop` returns only
after the goroutine has exited (no leak) and that it deletes the lock key.
`TestReclaimAfterFlush` enables `WithReclaim`, flushes the whole store, and shows
the lease survives because renewal re-creates the key with SetNX.

Create `watchdog_test.go`:

```go
package watchdog

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredislib "github.com/redis/go-redis/v9"
)

func newTestClient(t *testing.T) (*miniredis.Miniredis, *goredislib.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := goredislib.NewClient(&goredislib.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestRenewalKeepsLeaseAlive(t *testing.T) {
	t.Parallel()
	mr, client := newTestClient(t)
	w := New(client, 200*time.Millisecond)

	ctx, stop, err := w.Guarded(context.Background(), "job")
	if err != nil {
		t.Fatalf("Guarded: %v", err)
	}
	defer stop()

	for i := range 3 {
		time.Sleep(120 * time.Millisecond)
		if ctx.Err() != nil {
			t.Fatalf("context cancelled during interval %d: %v", i, context.Cause(ctx))
		}
		if ttl := mr.TTL("job"); ttl <= 0 {
			t.Fatalf("lease TTL not renewed at interval %d: %v", i, ttl)
		}
	}
}

func TestLeaseLossCancelsContext(t *testing.T) {
	t.Parallel()
	mr, client := newTestClient(t)
	w := New(client, 200*time.Millisecond)

	ctx, stop, err := w.Guarded(context.Background(), "job")
	if err != nil {
		t.Fatalf("Guarded: %v", err)
	}
	defer stop()

	mr.Del("job") // simulate a partition / takeover: the key vanishes

	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrLeaseLost) {
			t.Fatalf("cause = %v; want ErrLeaseLost", context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not cancel context after lease loss")
	}
}

func TestStopReleasesAndExits(t *testing.T) {
	t.Parallel()
	mr, client := newTestClient(t)
	w := New(client, 500*time.Millisecond)

	ctx, stop, err := w.Guarded(context.Background(), "job")
	if err != nil {
		t.Fatalf("Guarded: %v", err)
	}

	stop() // returns only once the renewal goroutine has exited

	if ctx.Err() == nil {
		t.Fatal("context still active after stop")
	}
	if mr.Exists("job") {
		t.Fatal("lock key not released after stop")
	}
}

func TestReclaimAfterFlush(t *testing.T) {
	t.Parallel()
	mr, client := newTestClient(t)
	w := New(client, 200*time.Millisecond, WithReclaim())

	ctx, stop, err := w.Guarded(context.Background(), "job")
	if err != nil {
		t.Fatalf("Guarded: %v", err)
	}
	defer stop()

	mr.FlushAll() // the key vanishes entirely

	// With SetNX-on-extend the next renewal re-creates the key and the lease
	// survives across the next few intervals.
	time.Sleep(350 * time.Millisecond)

	if ctx.Err() != nil {
		t.Fatalf("reclaim watchdog lost lease: %v", context.Cause(ctx))
	}
	if !mr.Exists("job") {
		t.Fatal("reclaim did not re-create the lock key")
	}
}

func ExampleWatchdog_Guarded() {
	mr, _ := miniredis.Run()
	defer mr.Close()
	client := goredislib.NewClient(&goredislib.Options{Addr: mr.Addr()})
	defer client.Close()

	w := New(client, time.Second)
	ctx, stop, err := w.Guarded(context.Background(), "rebuild")
	if err != nil {
		panic(err)
	}
	defer stop()

	// Right after acquisition the lease is held, so the context is alive.
	fmt.Printf("lease held: %v\n", ctx.Err() == nil)
	// Output:
	// lease held: true
}
```

## Review

The watchdog is correct when the derived context's liveness tracks the lease
exactly: alive while renewal succeeds, cancelled with cause `ErrLeaseLost` the
moment it does not. The renewal-keeps-alive test proves the first half by watching
the TTL get bumped across intervals; the lease-loss test proves the second by
deleting the key and asserting the cause. The mistakes to avoid are the ones the
concepts warned about: never renew on the work goroutine (a long step would starve
the ticker), never continue after a failed extend (the context cancel is what
stops the work), and never leak the goroutine — `stop` blocks on the `done`
channel so it returns only after `renew` exits, and `defer ticker.Stop()` releases
the ticker. Keying the failure decision off the `ok` boolean rather than a
specific error is deliberate: a lost single-node lease returns `(false, err)`, not
`(false, nil)`. Run `go test -race` to confirm the goroutine and the work loop do
not race on the context.

## Resources

- [redsync v4 package reference](https://pkg.go.dev/github.com/go-redsync/redsync/v4)
- [How to do distributed locking — Martin Kleppmann](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html)
- [context package — WithCancelCause / Cause](https://pkg.go.dev/context#WithCancelCause)
- [miniredis v2 — FastForward and TTL](https://pkg.go.dev/github.com/alicebob/miniredis/v2)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-redlock-mutex-lifecycle.md](01-redlock-mutex-lifecycle.md) | Next: [03-fencing-tokens.md](03-fencing-tokens.md)
