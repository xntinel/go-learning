# Exercise 16: Rate Limiter with a Transform Panic Boundary

**Nivel: Intermedio** — validacion rapida (un test corto).

A rate limiter that normalizes or validates a request key before checking
quota is running caller-supplied logic — a regex, a case-folding rule, a
tenant-ID parser — inside its own hot path. When that transform has a bug
and panics, the limiter cannot just crash the request goroutine: it needs to
atomically account for the failed attempt so a caller cannot retry a
panicking key in a loop and bypass rate limiting entirely, and it needs to
tell the caller whether the failure is worth retrying at all. This module
builds `Limiter.Check`, which reserves a slot, runs the transform under a
recover boundary, and classifies exactly what came back. It is fully
self-contained: its own module, demo, and tests.

## What you'll build

```text
ratelimiter/                independent module: example.com/ratelimiter
  go.mod                    go 1.24
  ratelimiter.go             Decision, Limiter, NewLimiter, Transform, Check
  cmd/
    demo/
      main.go               runnable demo: 4 keys through a quota of 6
  ratelimiter_test.go         clean allow, runtime bug, app panic, exhaustion
```

Files: `ratelimiter.go`, `cmd/demo/main.go`, `ratelimiter_test.go`.
Implement: `Limiter.Check(key string, transform Transform) (Decision, string, error)` that atomically reserves a quota slot, runs `transform` under `defer`/`recover`, penalizes a panicking attempt with a second atomic decrement, and classifies the recovered value into `Reject` (a `runtime.Error` bug) or `RetryLater` (an application `panic(error)`).
Test: a clean transform allows and consumes one slot; a transform that panics with a genuine `runtime.Error` (index out of range) is `Reject`ed and costs two slots; a transform that panics with an application error is `RetryLater` and also costs two slots, with `errors.Is` reaching the sentinel; an exhausted limiter rejects without ever calling the transform; an ordinary returned error is `RetryLater` and costs only its one reserved slot.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/rate-limiter-panic-containment/cmd/demo
cd ~/go-exercises/rate-limiter-panic-containment
go mod init example.com/ratelimiter
go mod edit -go=1.24
```

### Why reservation is a compare-and-swap loop, and the panic path costs double

`reserve` cannot just do `atomic.AddInt64(&l.quota, -1)` and check the
result, because two concurrent callers racing on the very last slot would
both decrement past zero — one of them consumed a slot that never existed.
The compare-and-swap loop reads the current quota, and only commits the
decrement if nothing else changed it in between; if the CAS fails, it
retries against the newer value. This is the standard "atomic reservation"
pattern for a counter multiple goroutines contend over, and it is the only
way `Check`'s "no slot, reject before ever calling transform" guarantee
holds under concurrency.

The double-decrement on panic is the part of this exercise that has nothing
to do with plain error handling. A transform that panics has already
consumed its reserved slot (the `reserve()` call succeeded before the
transform ever ran); the recover boundary then decrements the quota a
*second* time as a deliberate penalty. Without that penalty, a caller who
notices a key reliably panics could hammer it in a retry loop: each attempt
consumes one slot but the transform never produces a successful `Allow`, so
naively the caller could burn through the same key over and over at the cost
of one slot per try, which is indistinguishable from normal traffic to
anything watching only the allow/reject ratio. Charging the failed attempt
double makes a panic loop bleed quota twice as fast as it can now, and makes
a spike in panics visible as a spike in quota consumption, not just a
spike in a separate error metric nobody is watching.

Create `ratelimiter.go`:

```go
package ratelimiter

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
)

// Decision is what the caller should do with a checked request.
type Decision int

const (
	Allow Decision = iota
	Reject
	RetryLater
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Reject:
		return "reject"
	case RetryLater:
		return "retry-later"
	default:
		return "unknown"
	}
}

// ErrQuotaExhausted means the limiter had no slots left before the
// transform was ever invoked.
var ErrQuotaExhausted = errors.New("ratelimiter: quota exhausted")

// Transform normalizes or validates a request key before the quota check
// commits. It is caller-supplied domain logic and may panic on malformed
// input (a bad regex group, an out-of-range index) — that is exactly the
// failure mode this package contains.
type Transform func(key string) (string, error)

// Limiter reserves a fixed quota of request slots, atomically, across
// concurrent callers.
type Limiter struct {
	quota int64
}

// NewLimiter creates a Limiter with the given number of slots.
func NewLimiter(quota int64) *Limiter {
	return &Limiter{quota: quota}
}

// reserve atomically claims one quota slot via a compare-and-swap loop, so
// concurrent callers racing on the last slot never both succeed.
func (l *Limiter) reserve() bool {
	for {
		cur := atomic.LoadInt64(&l.quota)
		if cur <= 0 {
			return false
		}
		if atomic.CompareAndSwapInt64(&l.quota, cur, cur-1) {
			return true
		}
	}
}

// Check reserves one slot and runs transform against key. If the quota is
// already exhausted, transform is never called and nothing is consumed. If
// transform panics, the recover boundary decrements the quota a second
// time — on top of the slot already reserved — as a deliberate penalty for
// the failed attempt, closing off a loophole where a caller could retry a
// panicking key in a tight loop and never actually be rate-limited. The
// recovered value is then classified: a runtime.Error means the transform
// itself has a bug, so the request is rejected outright and not worth
// retrying; an application panic(error) is treated as transient and the
// caller is told to retry later; anything else (a bare string, an int) is
// treated conservatively as Reject.
func (l *Limiter) Check(key string, transform Transform) (decision Decision, normalized string, err error) {
	if !l.reserve() {
		return Reject, "", ErrQuotaExhausted
	}

	defer func() {
		if r := recover(); r != nil {
			atomic.AddInt64(&l.quota, -1) // extra penalty beyond the slot already reserved

			if e, ok := r.(error); ok {
				var rerr runtime.Error
				if errors.As(e, &rerr) {
					decision = Reject
					err = fmt.Errorf("ratelimiter: transform bug for key %q: %w", key, rerr)
					return
				}
				decision = RetryLater
				err = fmt.Errorf("ratelimiter: transform failed for key %q: %w", key, e)
				return
			}
			decision = Reject
			err = fmt.Errorf("ratelimiter: transform panicked for key %q: %v", key, r)
		}
	}()

	normalized, tErr := transform(key)
	if tErr != nil {
		return RetryLater, "", tErr // ordinary error still consumes its reserved slot
	}
	return Allow, normalized, nil
}

// Remaining reports the current quota, for tests and diagnostics.
func (l *Limiter) Remaining() int64 {
	return atomic.LoadInt64(&l.quota)
}
```

### The runnable demo

Four requests hit a quota of 6: a clean transform, one that panics with a
genuine index-out-of-range (`runtime.Error`), one that panics with an
application error, and a final clean one. The quota's final value shows
both panics charged double.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/ratelimiter"
)

func main() {
	limiter := ratelimiter.NewLimiter(6)

	clean := func(k string) (string, error) { return k, nil }
	runtimeBug := func(k string) (string, error) {
		parts := []string{"one"}
		return parts[5], nil // index out of range
	}
	appPanic := func(k string) (string, error) {
		panic(errors.New("normalization backend timed out"))
	}

	requests := []struct {
		key       string
		transform ratelimiter.Transform
	}{
		{"key-a", clean},
		{"key-b", runtimeBug},
		{"key-c", appPanic},
		{"key-d", clean},
	}

	for _, req := range requests {
		decision, _, err := limiter.Check(req.key, req.transform)
		fmt.Printf("%s: %s (err=%v)\n", req.key, decision, err != nil)
	}
	fmt.Printf("quota remaining: %d\n", limiter.Remaining())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
key-a: allow (err=false)
key-b: reject (err=true)
key-c: retry-later (err=true)
key-d: allow (err=false)
quota remaining: 0
```

### Tests

Five tests cover the full decision matrix: a clean allow, a `runtime.Error`
panic classified as `Reject`, an application `panic(error)` classified as
`RetryLater` with the sentinel still reachable via `errors.Is`, exhaustion
rejecting before the transform ever runs, and an ordinary returned error
costing only its one reserved slot.

Create `ratelimiter_test.go`:

```go
package ratelimiter

import (
	"errors"
	"testing"
)

func TestCheckAllowsCleanTransform(t *testing.T) {
	l := NewLimiter(3)
	decision, normalized, err := l.Check("user-1", func(k string) (string, error) {
		return "USER-1", nil
	})
	if decision != Allow || err != nil || normalized != "USER-1" {
		t.Fatalf("got (%v, %q, %v), want (Allow, USER-1, nil)", decision, normalized, err)
	}
	if got := l.Remaining(); got != 2 {
		t.Fatalf("Remaining() = %d, want 2", got)
	}
}

func TestCheckClassifiesRuntimeBug(t *testing.T) {
	l := NewLimiter(3)
	badTransform := func(k string) (string, error) {
		parts := []string{"only-one"}
		return parts[5], nil // index out of range: a genuine runtime.Error
	}

	decision, _, err := l.Check("user-2", badTransform)
	if decision != Reject {
		t.Fatalf("decision = %v, want Reject for a runtime bug", decision)
	}
	if err == nil {
		t.Fatal("err = nil, want a wrapped runtime.Error")
	}
	// The panic penalty means this single failed attempt costs 2 slots.
	if got := l.Remaining(); got != 1 {
		t.Fatalf("Remaining() = %d, want 1 (3 - 2 for the penalized panic)", got)
	}
}

func TestCheckClassifiesApplicationPanic(t *testing.T) {
	l := NewLimiter(3)
	sentinel := errors.New("upstream normalization service unavailable")
	flaky := func(k string) (string, error) {
		panic(sentinel)
	}

	decision, _, err := l.Check("user-3", flaky)
	if decision != RetryLater {
		t.Fatalf("decision = %v, want RetryLater for an application panic", decision)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err %v does not wrap the sentinel", err)
	}
	if got := l.Remaining(); got != 1 {
		t.Fatalf("Remaining() = %d, want 1 (3 - 2 for the penalized panic)", got)
	}
}

func TestCheckRejectsWhenQuotaExhausted(t *testing.T) {
	l := NewLimiter(1)
	clean := func(k string) (string, error) { return k, nil }

	if decision, _, err := l.Check("first", clean); decision != Allow || err != nil {
		t.Fatalf("first Check = (%v, %v), want Allow, nil", decision, err)
	}
	decision, _, err := l.Check("second", clean)
	if decision != Reject || !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("second Check = (%v, %v), want (Reject, ErrQuotaExhausted)", decision, err)
	}
}

func TestCheckOrdinaryErrorDoesNotPanic(t *testing.T) {
	l := NewLimiter(2)
	decision, _, err := l.Check("user-4", func(k string) (string, error) {
		return "", errors.New("invalid key format")
	})
	if decision != RetryLater || err == nil {
		t.Fatalf("got (%v, %v), want (RetryLater, non-nil)", decision, err)
	}
	if got := l.Remaining(); got != 1 {
		t.Fatalf("Remaining() = %d, want 1 (ordinary error still consumes its reserved slot)", got)
	}
}
```

## Review

`Check` is correct when the quota accounting stays atomic under concurrent
callers (the CAS loop in `reserve`) and when a panicking transform never
lets a caller retry for free — the extra decrement in the recover boundary
is what closes that loophole. The classification step is what separates
this from a generic recover-and-log middleware: `errors.As(e, &rerr)`
against the `runtime.Error` interface is the only reliable way to tell "the
transform has an actual bug" from "the transform hit a modeled, retryable
failure," and conflating the two either blinds on-call to real bugs (if
runtime errors are silently retried) or needlessly hard-fails transient
issues (if application errors are rejected outright). Notice the exhaustion
path never calls `transform` at all — `reserve` gates entry, so a caller
already over quota cannot even trigger the transform's side effects, panicking
or not.

## Resources

- [sync/atomic: CompareAndSwapInt64](https://pkg.go.dev/sync/atomic#CompareAndSwapInt64) — the reservation loop this limiter's quota accounting relies on.
- [runtime.Error](https://pkg.go.dev/runtime#Error) — the interface that distinguishes a genuine runtime bug from an application panic.
- [errors.As](https://pkg.go.dev/errors#As) — detecting runtime.Error without comparing concrete types.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-connection-pool-worker-isolation.md](15-connection-pool-worker-isolation.md) | Next: [17-config-loader-fallback-chain.md](17-config-loader-fallback-chain.md)
