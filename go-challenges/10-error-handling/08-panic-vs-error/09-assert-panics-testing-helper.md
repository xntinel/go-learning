# Exercise 9: Reusable Test Helper That Asserts a Function Panics

Test infrastructure a backend team actually ships: `RequirePanics` and
`RequirePanicValue`, small helpers that assert a callback panics (optionally with a
matching value or sentinel error) and fail the test cleanly if it does not. They
are reused across the whole suite, and building them cements the recover semantics
the rest of this lesson relies on.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
paniccheck/                  independent module: example.com/paniccheck
  go.mod                     go 1.26
  paniccheck.go              TB interface; RequirePanics; RequirePanicValue; panicMatches
  cmd/
    demo/
      main.go                runnable demo using a stdout-printing TB
  paniccheck_test.go         meta-test with a fake TB recorder; also drives a real *testing.T
```

Files: `paniccheck.go`, `cmd/demo/main.go`, `paniccheck_test.go`.
Implement: a minimal `TB` interface (`Helper`, `Errorf`), `RequirePanics(t TB, fn func())`, and `RequirePanicValue(t TB, want any, fn func())` that uses `errors.Is` for error `want` and `reflect.DeepEqual` otherwise.
Test: a fake `TB` recorder proves `RequirePanics` passes when `fn` panics and records a failure when it does not; `RequirePanicValue` matches an expected value/sentinel and records a failure on a mismatch; `Helper` is invoked; a case drives a real `*testing.T` for the passing path.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/paniccheck/cmd/demo
cd ~/go-exercises/paniccheck
go mod init example.com/paniccheck
go mod edit -go=1.26
```

### Why the helper takes an interface, not *testing.T

The helper must call `recover()` inside a deferred function, so that a panic in the
callback is caught (that is the whole assertion) and a *missing* panic is reported
as a failure. The recover has to be in the deferred closure directly — the same
rule as every other recovery boundary in this lesson.

The design choice that makes the helper itself *testable* is the `TB` interface. If
`RequirePanics` took a concrete `*testing.T`, you could not meta-test it: a real
`*testing.T.Errorf` would fail the very test that is checking the helper's failure
path. By depending on a tiny interface — `Helper()` and `Errorf(format, args...)`,
both satisfied by `*testing.T` — you can pass a fake recorder that captures the
failure instead of raising it, and assert the helper reports correctly. This is a
general lesson about testable infrastructure: depend on the narrowest interface
that does the job.

`t.Helper()` is what makes a failure point at the *caller's* line rather than the
line inside `RequirePanics`. Calling it is not optional in real infrastructure — a
helper that reports failures against its own body is nearly useless for debugging.

`RequirePanicValue` compares the recovered value to `want`: if `want` is an
`error`, it uses `errors.Is` (so a wrapped sentinel still matches); otherwise it
uses `reflect.DeepEqual`. Using `errors.Is` for the error case is what lets a test
assert `RequirePanicValue(t, io.EOF, fn)` even when the code panicked with a
wrapped `io.EOF`.

Create `paniccheck.go`:

```go
package paniccheck

import (
	"errors"
	"reflect"
)

// TB is the minimal testing surface the helpers need. *testing.T and *testing.B
// both satisfy it; a fake implementation lets the helpers be meta-tested.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// RequirePanics asserts that fn panics. If fn returns normally, it reports a
// failure through t.
func RequirePanics(t TB, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected fn to panic, but it returned normally")
		}
	}()
	fn()
}

// RequirePanicValue asserts that fn panics with a value matching want. For an
// error want it matches with errors.Is (so a wrapped sentinel counts); otherwise
// it uses reflect.DeepEqual.
func RequirePanicValue(t TB, want any, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("expected fn to panic with %v, but it returned normally", want)
			return
		}
		if !panicMatches(r, want) {
			t.Errorf("panic value = %v, want %v", r, want)
		}
	}()
	fn()
}

func panicMatches(got, want any) bool {
	if wantErr, ok := want.(error); ok {
		gotErr, ok := got.(error)
		return ok && errors.Is(gotErr, wantErr)
	}
	return reflect.DeepEqual(got, want)
}
```

### The runnable demo

The demo supplies a tiny `TB` that prints to stdout, so you can watch the helper
pass on a real panic and report on a missing one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/paniccheck"
)

// printTB is a TB that prints failures instead of failing a test.
type printTB struct{}

func (printTB) Helper() {}
func (printTB) Errorf(format string, args ...any) {
	fmt.Printf("FAIL: "+format+"\n", args...)
}

func main() {
	t := printTB{}

	fmt.Println("case 1: fn panics as required")
	paniccheck.RequirePanics(t, func() { panic("boom") })

	fmt.Println("case 2: fn does not panic")
	paniccheck.RequirePanics(t, func() {})

	fmt.Println("case 3: matching sentinel error")
	paniccheck.RequirePanicValue(t, errors.ErrUnsupported, func() {
		panic(fmt.Errorf("wrapped: %w", errors.ErrUnsupported))
	})

	fmt.Println("case 4: mismatched value")
	paniccheck.RequirePanicValue(t, "want-this", func() { panic("got-that") })
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
case 1: fn panics as required
case 2: fn does not panic
FAIL: expected fn to panic, but it returned normally
case 3: matching sentinel error
case 4: mismatched value
FAIL: panic value = got-that, want want-this
```

### Tests

The meta-test injects a `recorder` implementing `TB` so it can assert *how* the
helper reported. `TestRequirePanics` checks both the pass path (no recorded errors)
and the fail path (one recorded error), and that `Helper` was called.
`TestRequirePanicValue` checks a matching value, a matching wrapped sentinel, and a
mismatch. `TestWithRealT` drives an actual `*testing.T` to prove the interface is
satisfied by the real type on the passing path.

Create `paniccheck_test.go`:

```go
package paniccheck

import (
	"errors"
	"fmt"
	"testing"
)

// recorder is a fake TB that captures failures instead of raising them.
type recorder struct {
	helperCalls int
	failures    []string
}

func (r *recorder) Helper() { r.helperCalls++ }
func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestRequirePanics(t *testing.T) {
	t.Parallel()

	// Pass path: fn panics, so no failure is recorded.
	pass := &recorder{}
	RequirePanics(pass, func() { panic("boom") })
	if len(pass.failures) != 0 {
		t.Fatalf("RequirePanics recorded %v on a panicking fn; want none", pass.failures)
	}
	if pass.helperCalls == 0 {
		t.Fatal("RequirePanics did not call t.Helper()")
	}

	// Fail path: fn does not panic, so exactly one failure is recorded.
	fail := &recorder{}
	RequirePanics(fail, func() {})
	if len(fail.failures) != 1 {
		t.Fatalf("RequirePanics recorded %d failures on a non-panicking fn; want 1", len(fail.failures))
	}
}

func TestRequirePanicValue(t *testing.T) {
	t.Parallel()

	// Matching plain value.
	m := &recorder{}
	RequirePanicValue(m, "boom", func() { panic("boom") })
	if len(m.failures) != 0 {
		t.Fatalf("recorded %v on a matching value; want none", m.failures)
	}

	// Matching wrapped sentinel error via errors.Is.
	sentinel := errors.New("sentinel")
	s := &recorder{}
	RequirePanicValue(s, sentinel, func() { panic(fmt.Errorf("wrapped: %w", sentinel)) })
	if len(s.failures) != 0 {
		t.Fatalf("recorded %v on a wrapped sentinel; want none", s.failures)
	}

	// Mismatch records a failure.
	mm := &recorder{}
	RequirePanicValue(mm, "want-this", func() { panic("got-that") })
	if len(mm.failures) != 1 {
		t.Fatalf("recorded %d failures on a mismatch; want 1", len(mm.failures))
	}

	// No panic at all records a failure.
	np := &recorder{}
	RequirePanicValue(np, "anything", func() {})
	if len(np.failures) != 1 {
		t.Fatalf("recorded %d failures on a non-panicking fn; want 1", len(np.failures))
	}
}

func TestWithRealT(t *testing.T) {
	t.Parallel()
	// The real *testing.T satisfies TB; on the passing path nothing is reported.
	RequirePanics(t, func() { panic("boom") })
	RequirePanicValue(t, "exact", func() { panic("exact") })
}

func ExampleRequirePanics() {
	RequirePanics(printTB{}, func() { panic("boom") })
	fmt.Println("no failure printed above means it panicked as required")
	// Output: no failure printed above means it panicked as required
}

// printTB is a TB used by the example; it prints failures.
type printTB struct{}

func (printTB) Helper() {}
func (printTB) Errorf(format string, args ...any) {
	fmt.Printf("FAIL: "+format+"\n", args...)
}
```

## Review

The helpers are correct when they pass exactly on a matching panic and record a
single failure otherwise — for both the missing-panic and mismatched-value cases.
The design payoff is the `TB` interface: it is what lets the meta-test inject a
`recorder` and assert the helper's *reporting* behavior, which a concrete
`*testing.T` would make impossible (its `Errorf` would fail the meta-test). The
`errors.Is` branch in `panicMatches` is why a wrapped sentinel still matches — a
plain `==` or `DeepEqual` on errors would miss it. And `t.Helper()` is the small
detail that makes real failures point at the caller. Run `go test -race` and
`go vet ./...`; vet's printf check validates the `Errorf` format strings.

## Resources

- [`testing.T.Helper`](https://pkg.go.dev/testing#T.Helper) — attributing failures to the caller's line.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel in `panicMatches`.
- [`reflect.DeepEqual`](https://pkg.go.dev/reflect#DeepEqual) — comparing non-error panic values.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../09-error-handling-in-goroutines/00-concepts.md](../09-error-handling-in-goroutines/00-concepts.md)
