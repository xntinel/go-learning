# Exercise 25: Deferred Closure Resource Cleanup with Guard Clause and Named Return

**Nivel: Intermedio** — validacion rapida (un test corto).

A constructor that both acquires a resource and validates it has a leak trap built
in: if validation fails, the caller never receives a handle, so whoever acquired the
resource is the only one who can still close it. This module builds that guarded
`Open` — a deferred closure with a guard clause that reads the named return to
decide whether cleanup is its job or the caller's.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
resource/                     module example.com/resource
  go.mod
  resource.go                   Resource, Open (guard-clause defer over named return)
  resource_test.go               success stays open, failure is closed, mixed outcomes, idempotent Close
```

- Files: `resource.go`, `resource_test.go`.
- Implement: `Resource` tracking open/closed state against a shared counter; `Open(name, counter, validate)` acquiring the resource, then deferring a guard clause that closes it only if the named return `err` is non-nil.
- Test: a successful validation returns an open resource and leaves the counter incremented; a failing validation returns `nil, err` and the counter returns to zero; mixed successes and failures leave only the successes open; `Close` is idempotent.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/25-deferred-cleanup-guard
cd go-solutions/04-functions/05-anonymous-functions/25-deferred-cleanup-guard
go mod edit -go=1.24
```

### The guard clause reads the named return, not a local flag

`Open` acquires the resource unconditionally, then immediately defers a closure that
checks the named return `err`: `if err != nil { r.Close() }`. Nothing else in the
closure — no separate boolean, no explicit "did validation fail" flag — decides
whether to clean up; it is entirely driven by whatever `err` holds by the time the
deferred closure runs, which is *after* `validate` has already assigned it. On the
success path `err` is `nil`, the guard does nothing, and the caller receives the open
`*Resource` and owns closing it from then on. On the failure path, `Open` itself
returns `nil` for the resource — the caller has no handle at all — so if the guard
clause did not close it here, the resource would be acquired forever with nothing
able to release it. That asymmetry — the caller owns cleanup on success, `Open` owns
it on failure — is exactly what the guard clause over the named return encodes in
one line.

Create `resource.go`:

```go
package resource

import "sync/atomic"

// Resource is a handle to an acquired resource, such as a file descriptor
// or a lease. Open tracks how many are currently held so tests can detect
// leaks.
type Resource struct {
	name    string
	counter *atomic.Int64
	closed  bool
}

// Closed reports whether Close has already run.
func (r *Resource) Closed() bool { return r.closed }

// Close releases the resource. It is idempotent.
func (r *Resource) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.counter.Add(-1)
	return nil
}

func acquire(name string, counter *atomic.Int64) *Resource {
	counter.Add(1)
	return &Resource{name: name, counter: counter}
}

// Open acquires a resource and validates it before handing it to the
// caller. The deferred closure is a guard clause over the named return
// err: it reads err only after validate has run (or after Open itself set
// it) and closes the resource *only on the failure path*. On success err
// is nil, the guard does nothing, and the caller receives the open
// Resource and owns its cleanup from then on. On failure the caller never
// gets a usable handle at all, so if Open didn't close it here itself, the
// resource would leak — the guard clause is what prevents that.
func Open(name string, counter *atomic.Int64, validate func(*Resource) error) (res *Resource, err error) {
	r := acquire(name, counter)
	defer func() {
		if err != nil {
			r.Close()
		}
	}()

	if err = validate(r); err != nil {
		return nil, err
	}
	return r, nil
}
```

### Tests

`TestOpenReturnsOpenResourceOnSuccess` checks the happy path leaves the resource open
and the counter incremented. `TestOpenClosesResourceOnValidationFailure` checks a
failing `validate` returns `nil, err` and the counter is back to zero — the guard
clause did the cleanup `Open`'s caller never gets the chance to do.
`TestOpenDoesNotLeakAcrossMixedOutcomes` mixes two successes and one failure and
checks only the successes remain open. `TestResourceCloseIsIdempotent` checks a
double `Close` does not double-decrement the counter.

Create `resource_test.go`:

```go
package resource

import (
	"errors"
	"sync/atomic"
	"testing"
)

func TestOpenReturnsOpenResourceOnSuccess(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64

	r, err := Open("ok", &counter, func(*Resource) error { return nil })
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	if r == nil {
		t.Fatal("Open() returned nil Resource on success")
	}
	if r.Closed() {
		t.Fatal("Resource is Closed() on the success path, want open")
	}
	if got := counter.Load(); got != 1 {
		t.Fatalf("counter = %d, want 1 (resource left open for the caller)", got)
	}
}

func TestOpenClosesResourceOnValidationFailure(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	want := errors.New("bad checksum")

	r, err := Open("bad", &counter, func(*Resource) error { return want })
	if r != nil {
		t.Fatalf("Open() returned a non-nil Resource on failure: %v", r)
	}
	if !errors.Is(err, want) {
		t.Fatalf("Open() err = %v, want %v", err, want)
	}
	if got := counter.Load(); got != 0 {
		t.Fatalf("counter = %d after a failed Open, want 0 (guard clause must close it)", got)
	}
}

func TestOpenDoesNotLeakAcrossMixedOutcomes(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64

	r1, err1 := Open("ok-1", &counter, func(*Resource) error { return nil })
	_, err2 := Open("bad-1", &counter, func(*Resource) error { return errors.New("fail") })
	r3, err3 := Open("ok-2", &counter, func(*Resource) error { return nil })

	if err1 != nil || err3 != nil {
		t.Fatalf("unexpected errors: err1=%v err3=%v", err1, err3)
	}
	if err2 == nil {
		t.Fatal("expected an error from the failing validate, got nil")
	}
	if got := counter.Load(); got != 2 {
		t.Fatalf("counter = %d, want 2 (only the two successes remain open)", got)
	}

	r1.Close()
	r3.Close()
	if got := counter.Load(); got != 0 {
		t.Fatalf("counter = %d after closing both, want 0", got)
	}
}

func TestResourceCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	r, err := Open("ok", &counter, func(*Resource) error { return nil })
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}

	r.Close()
	r.Close()
	if got := counter.Load(); got != 0 {
		t.Fatalf("counter = %d after double Close, want 0 (Close must be idempotent)", got)
	}
}
```

## Review

`Open` is correct when the counter always reflects exactly the resources the caller
actually still holds: incremented once per successful `Open`, back to zero for every
one that failed validation, and never double-decremented by a repeated `Close`. The
guard clause is the whole mechanism — reading the named return `err` after
`validate` has run is what lets one deferred closure serve both outcomes correctly
without an explicit success/failure branch duplicated at every `return` site in
`Open`'s body. Forgetting the guard (deferring an unconditional `r.Close()`, or
omitting the defer and calling `r.Close()` only next to the one `return nil, err`
that exists today) is the two ways this exact pattern breaks: the first closes even
the success path's resource out from under its caller, and the second leaks the
moment a future validation failure is added with its own new early return.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-tenant-worker-partition.md](24-tenant-worker-partition.md) | Next: [26-metric-aggregator-callback.md](26-metric-aggregator-callback.md)
