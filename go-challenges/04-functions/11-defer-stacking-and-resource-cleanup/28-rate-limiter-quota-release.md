# Exercise 28: Rate Limiter Quota — Acquire and Idempotent Release

**Nivel: Intermedio** — validacion rapida (un test corto).

Deducting quota from a rate limiter is only half the contract — the other
half is giving it back exactly once, even if the caller releases it early
on a fast path *and* a deferred safety net also tries to release it on the
way out. This module builds that guarantee directly into the release
closure itself, using an atomic flag so a second (or third, or
thirty-second concurrent) call to release is a guaranteed no-op.

## What you'll build

```text
ratelimit/                   independent module: example.com/ratelimit
  go.mod
  ratelimit/ratelimit.go       Limiter; Acquire (idempotent release); Do
  cmd/demo/main.go              acquire, release twice, over-budget request
  ratelimit/ratelimit_test.go   idempotent release; acquire failure; error/panic release; concurrent releases
```

- Files: `ratelimit/ratelimit.go`, `cmd/demo/main.go`, `ratelimit/ratelimit_test.go`.
- Implement: a `Limiter` guarding an `int64` quota with `atomic.CompareAndSwapInt64`; `Acquire(cost int64) (release func(), ok bool)`, whose returned closure is itself guarded by an `atomic.CompareAndSwapInt32` flag so only the first call refunds; and `Do(l *Limiter, cost int64, work func() error) error`, which acquires, defers `release`, and runs `work`.
- Test: calling the release closure three times refunds the quota exactly once; `Acquire` fails (and leaves quota untouched) when the requested cost exceeds what remains; `Do` releases on both an error return and a panic; 32 goroutines calling the same release closure concurrently still refund exactly once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the release itself needs a guard

`defer release()` is the ordinary way to make sure quota comes back no
matter how a function returns. But real call sites are messier than a
single `defer`: a fast-path success branch might want to release the quota
immediately rather than wait for the function to fully unwind, while a
`defer` at the top of the same function stays in place as a safety net for
every other path, including a panic. If `release` itself just did
`atomic.AddInt64(&l.quota, cost)` unconditionally, both calls would run and
the quota would be refunded twice — silently handing out more capacity than
the limiter was ever configured with.

`Acquire` closes over a private `released int32` for exactly this reason.
The closure it returns first tries `atomic.CompareAndSwapInt32(&released,
0, 1)`; only the call that wins that swap — necessarily the first one,
whether it came from an explicit call or a deferred one, and whether it
came from this goroutine or a concurrent one — actually adds the cost back
to the quota. Every other call, no matter how many there are or where they
come from, sees `released` already `1` and does nothing.

Create `ratelimit/ratelimit.go`:

```go
package ratelimit

import (
	"errors"
	"sync/atomic"
)

// ErrQuotaExceeded is returned by Do when the limiter has too little quota
// remaining to cover the requested cost.
var ErrQuotaExceeded = errors.New("ratelimit: quota exceeded")

// Limiter tracks a quota of available units, safe for concurrent use.
type Limiter struct {
	quota int64
}

// NewLimiter returns a Limiter starting with quota available units.
func NewLimiter(quota int64) *Limiter {
	return &Limiter{quota: quota}
}

// Available reports how many units currently remain.
func (l *Limiter) Available() int64 {
	return atomic.LoadInt64(&l.quota)
}

// Acquire deducts cost units from the quota if enough are available. On
// success it returns a release closure that refunds cost -- and that
// closure is idempotent: it is guarded by its own atomic flag, so calling
// it more than once (for example once explicitly on a fast success path and
// once more from a caller's defer as a safety net) refunds the quota
// exactly once, never twice.
func (l *Limiter) Acquire(cost int64) (release func(), ok bool) {
	for {
		cur := atomic.LoadInt64(&l.quota)
		if cur < cost {
			return func() {}, false
		}
		if atomic.CompareAndSwapInt64(&l.quota, cur, cur-cost) {
			break
		}
	}

	var released int32
	release = func() {
		if atomic.CompareAndSwapInt32(&released, 0, 1) {
			atomic.AddInt64(&l.quota, cost)
		}
	}
	return release, true
}

// Do acquires cost units, runs work, and defers the release of those units
// so they are refunded on every exit path -- including a panic inside work.
func Do(l *Limiter, cost int64, work func() error) error {
	release, ok := l.Acquire(cost)
	if !ok {
		return ErrQuotaExceeded
	}
	defer release()

	return work()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ratelimit/ratelimit"
)

func main() {
	limiter := ratelimit.NewLimiter(10)

	release, ok := limiter.Acquire(4)
	fmt.Println("acquired:", ok, "available:", limiter.Available())

	// An explicit, early release (perhaps a fast-path short-circuit)...
	release()
	fmt.Println("after first release, available:", limiter.Available())

	// ...and a second, redundant call (perhaps from a caller's own defer)
	// must not double-refund the quota.
	release()
	fmt.Println("after second release, available:", limiter.Available())

	err := ratelimit.Do(limiter, 20, func() error { return nil })
	fmt.Println("over-budget request err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquired: true available: 6
after first release, available: 10
after second release, available: 10
over-budget request err: ratelimit: quota exceeded
```

### Tests

Create `ratelimit/ratelimit_test.go`:

```go
package ratelimit

import (
	"errors"
	"sync"
	"testing"
)

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	l := NewLimiter(10)
	release, ok := l.Acquire(4)
	if !ok {
		t.Fatal("Acquire(4) = false, want true")
	}
	if got := l.Available(); got != 6 {
		t.Fatalf("Available() = %d, want 6", got)
	}

	release()
	release()
	release()

	if got := l.Available(); got != 10 {
		t.Fatalf("Available() = %d after triple release, want 10 (refunded once)", got)
	}
}

func TestAcquireFailsWhenQuotaInsufficient(t *testing.T) {
	t.Parallel()

	l := NewLimiter(3)
	_, ok := l.Acquire(4)
	if ok {
		t.Fatal("Acquire(4) = true, want false: only 3 units available")
	}
	if got := l.Available(); got != 3 {
		t.Fatalf("Available() = %d, want 3 (untouched on failed acquire)", got)
	}
}

func TestDoReleasesOnErrorAndOnPanic(t *testing.T) {
	t.Parallel()

	l := NewLimiter(10)
	boom := errors.New("boom")

	err := Do(l, 5, func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want errors.Is %v", err, boom)
	}
	if got := l.Available(); got != 10 {
		t.Fatalf("Available() = %d, want 10 after error", got)
	}

	func() {
		defer func() { _ = recover() }()
		_ = Do(l, 5, func() error { panic("kaboom") })
	}()
	if got := l.Available(); got != 10 {
		t.Fatalf("Available() = %d, want 10 after panic", got)
	}
}

func TestReleaseIsIdempotentUnderConcurrency(t *testing.T) {
	t.Parallel()

	l := NewLimiter(100)
	release, ok := l.Acquire(10)
	if !ok {
		t.Fatal("Acquire(10) = false, want true")
	}

	const workers = 32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release()
		}()
	}
	wg.Wait()

	if got := l.Available(); got != 100 {
		t.Fatalf("Available() = %d, want 100 (refunded exactly once across %d concurrent releases)", got, workers)
	}
}
```

## Review

The limiter is correct when its `Available` count is exact after any
sequence of acquires and releases — including redundant, concurrent, or
panic-triggered releases — never drifting above the configured quota or
staying deducted forever. The mistake this pattern exists to prevent is
writing a release closure that unconditionally credits the quota back,
which looks correct in the single-caller, single-return-path case this
chapter usually tests but silently over-refunds the moment a caller (or a
future maintainer adding a fast path) calls it more than once. Guarding the
refund with its own atomic flag, scoped to that one closure instance,
makes the idempotence a property of the release itself rather than a
discipline every call site has to remember to uphold. Run with `-race`.

## Resources

- [sync/atomic package](https://pkg.go.dev/sync/atomic)
- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Stripe API rate limits](https://stripe.com/docs/rate-limits) — a real-world token-bucket-style quota the shape here is modeled after.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-schema-migration-down-stack.md](27-schema-migration-down-stack.md) | Next: [29-dns-cache-entry-ttl-cleanup.md](29-dns-cache-entry-ttl-cleanup.md)
