# Exercise 3: Pinning the Panic Contract: Re-Panic Semantics of OnceValue/OnceValues

When a lazy initializer panics in production, what every later caller sees is
decided by which once primitive you chose — and the two contracts are
opposites; this exercise pins both with tests so the difference is proven, not
remembered.

## What you'll build

```text
oncepanic/                 independent module: example.com/oncepanic
  go.mod                   go mod init example.com/oncepanic
  oncepanic.go             Int, Strings wrappers; PanicValue recover harness
  oncepanic_test.go        re-panic identity tests, Once.Do swallow test, Example
  cmd/
    demo/
      main.go              runnable demo: OnceValue re-panics twice, Once.Do goes silent
```

- Files: `oncepanic.go`, `oncepanic_test.go`, `cmd/demo/main.go`.
- Implement: `Int` and `Strings` once-wrappers plus `PanicValue(fn func()) (any, bool)`, a deferred-recover harness that reports what a call panicked with.
- Test: both calls to a panicking `OnceValue` panic with the identical value and the init runs exactly once; same for `OnceValues`; a contrast test shows `sync.Once.Do` treating the panicking init as done and returning silently on the second call.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

### Two primitives, two contracts

Both `sync.Once.Do` and the Go 1.21 helpers agree on one thing: a panicking
init counts as done, and the function is never re-run. They disagree on what
later callers experience:

| Primitive | First call, f panics | Every later call | Init re-runs |
|---|---|---|---|
| `sync.Once.Do(f)` | panic propagates | returns silently, runs nothing | never |
| `OnceFunc` / `OnceValue` / `OnceValues` | panic propagates | panics with the **same value** | never |

The `Once.Do` behavior is the dangerous one in a service. Imagine `f` builds a
registry and panics halfway: the first request crashes its goroutine (or is
caught by a recovery middleware and turned into a 500), and every request
after that sails through `Do` untouched and reads a half-initialized registry
— nil maps, missing entries — producing failures far from the cause, possibly
much later, possibly only on some endpoints. The helpers keep the failure
loud and attributable instead: every caller that touches the broken singleton
panics with the original value, so the incident points at the init that broke.
The proposal (golang/go#56102) chose this deliberately; it is a contract, not
an accident, and pkg.go.dev states it: "If f panics, the returned function
will panic with the same value on every call."

"Same value" is strong: it is the identical panic value, not a copy or a
re-rendering. The tests below panic with a pointer (`*sentinelError`) and
assert identity with `==` across both calls, which also demonstrates the
practical consequence — a recovery middleware can match the panic value
against a known sentinel to classify the failure.

Neither primitive retries. If the correct behavior for your init is
"recover and try again later", you must `recover` inside `f` and convert the
panic into an error (with `OnceValues`), or use the retryable mutex-based
`Lazy[T]` from exercise 05.

### The recover harness

Asserting "this call panicked with value X" requires the deferred-recover
dance in every test; `PanicValue` packages it once. It runs `fn`, and if `fn`
panics it captures the value and reports `(value, true)`; a normal return
reports `(nil, false)`. The named return values are what make the deferred
closure able to overwrite the results after the panic unwinds — a plain
`return recover(), true` inside `fn`'s frame would never run.

Create `oncepanic.go`:

```go
// Package oncepanic pins the panic semantics of the sync once helpers
// against raw sync.Once.Do, and provides a small recover harness for
// asserting panic values in tests.
package oncepanic

import "sync"

// Int wraps fn in sync.OnceValue. If fn panics on first call, every later
// call panics with the same value and fn never re-runs.
func Int(fn func() int) func() int {
	return sync.OnceValue(fn)
}

// Strings wraps fn in sync.OnceValues with the same panic contract.
func Strings(fn func() (string, error)) func() (string, error) {
	return sync.OnceValues(fn)
}

// PanicValue runs fn and reports the value it panicked with. A normal
// return reports (nil, false). The named results let the deferred recover
// overwrite them while the panic unwinds.
func PanicValue(fn func()) (val any, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			val, panicked = r, true
		}
	}()
	fn()
	return nil, false
}
```

One honest caveat about `PanicValue`: it cannot distinguish "did not panic"
from "panicked with untyped nil", because `recover` reports nil for both.
Since Go 1.21 that gap barely exists — `panic(nil)` is converted to a
`*runtime.PanicNilError` precisely so recovered values are never nil — so the
harness is sound on any toolchain this lesson targets.

### The demo

The demo stages both contracts side by side: a panicking `OnceValue` init is
called twice and panics twice with the same value while running once; the
same init behind `sync.Once.Do` panics once and then goes silent.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/oncepanic"
)

func main() {
	inits := 0
	get := oncepanic.Int(func() int {
		inits++
		panic("schema registry corrupt")
	})

	for i := 1; i <= 2; i++ {
		v, panicked := oncepanic.PanicValue(func() { _ = get() })
		fmt.Printf("OnceValue call %d: panicked=%v value=%v\n", i, panicked, v)
	}
	fmt.Println("OnceValue init runs:", inits)

	inits = 0
	var once sync.Once
	f := func() {
		inits++
		panic("schema registry corrupt")
	}
	for i := 1; i <= 2; i++ {
		v, panicked := oncepanic.PanicValue(func() { once.Do(f) })
		fmt.Printf("Once.Do call %d: panicked=%v value=%v\n", i, panicked, v)
	}
	fmt.Println("Once.Do init runs:", inits)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
OnceValue call 1: panicked=true value=schema registry corrupt
OnceValue call 2: panicked=true value=schema registry corrupt
OnceValue init runs: 1
Once.Do call 1: panicked=true value=schema registry corrupt
Once.Do call 2: panicked=false value=<nil>
Once.Do init runs: 1
```

The second `Once.Do` line is the whole lesson: `panicked=false`. The caller
proceeds as if initialization succeeded.

### Tests

`TestIntPanicCountsAsDone` pins the `OnceValue` side; `TestStringsPanicOnce`
does the same for `OnceValues`, panicking with a pointer and asserting
identity across calls with `==`; `TestOnceDoSwallowsSecondCall` proves the
contrasting `Once.Do` contract. All three also assert the init ran exactly
once — the property the contracts share.

Create `oncepanic_test.go`:

```go
package oncepanic

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

type sentinelError struct{ msg string }

func (e *sentinelError) Error() string { return e.msg }

func TestIntPanicCountsAsDone(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	const panicVal = "boom"
	get := Int(func() int {
		calls.Add(1)
		panic(panicVal)
	})

	for i := range 2 {
		v, panicked := PanicValue(func() { _ = get() })
		if !panicked {
			t.Fatalf("call %d did not panic", i)
		}
		if v != panicVal {
			t.Fatalf("call %d panicked with %v, want %q", i, v, panicVal)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times, want 1", got)
	}
}

func TestStringsPanicOnce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	sentinel := &sentinelError{msg: "credentials file corrupt"}
	get := Strings(func() (string, error) {
		calls.Add(1)
		panic(sentinel)
	})

	var recovered [2]any
	for i := range 2 {
		v, panicked := PanicValue(func() { _, _ = get() })
		if !panicked {
			t.Fatalf("call %d did not panic", i)
		}
		recovered[i] = v
	}
	if recovered[0] != any(sentinel) || recovered[1] != any(sentinel) {
		t.Fatalf("recovered %v then %v, want the sentinel both times", recovered[0], recovered[1])
	}
	if recovered[0] != recovered[1] {
		t.Fatal("the two panics carried different values; contract says identical")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times, want 1", got)
	}
}

func TestOnceDoSwallowsSecondCall(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	var once sync.Once
	f := func() {
		calls.Add(1)
		panic("boom")
	}

	if _, panicked := PanicValue(func() { once.Do(f) }); !panicked {
		t.Fatal("first Do did not propagate the panic")
	}
	v, panicked := PanicValue(func() { once.Do(f) })
	if panicked {
		t.Fatalf("second Do panicked with %v; Once.Do treats a panicking f as done", v)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("f called %d times, want 1", got)
	}
}

func ExamplePanicValue() {
	v, panicked := PanicValue(func() { panic("boom") })
	fmt.Println(panicked, v)
	v, panicked = PanicValue(func() {})
	fmt.Println(panicked, v)
	// Output:
	// true boom
	// false <nil>
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

## Review

The module has done its job when you can state both contracts without looking
them up, because your tests will fail if either changes: the helpers re-panic
with the identical value forever and never re-run the init; `Once.Do` panics
once and then pretends everything is fine. The mistakes this protects against
are porting bugs — code moved from a `var once sync.Once` guard to
`sync.OnceFunc` (or back) with recovery middleware written for the other
contract — and false hope: neither primitive retries after a panic, so a
recover-and-retry design must catch the panic *inside* the init function and
return an error instead. When reviewing initialization code, ask one question:
if this init panics on the first request of the day, what does the second
request see? The answer should be written in a test, exactly like the three
here.

## Resources

- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue) — the documented "panics with the same value on every call" contract.
- [sync.Once](https://pkg.go.dev/sync#Once) — Do's semantics, including panic-counts-as-returned.
- [Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — why the harness needs named results and a deferred recover.
- [Proposal: sync: add OnceFunc, OnceValue, OnceValues](https://github.com/golang/go/issues/56102) — the discussion that fixed the re-panic behavior.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-lazy-counter-oncefunc.md](02-lazy-counter-oncefunc.md) | Next: [04-db-handle-oncevalues.md](04-db-handle-oncevalues.md)
