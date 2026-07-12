# Exercise 4: Classifying Recovered Values: runtime.Error vs App Panic vs PanicNilError

Recovering a panic is only half the job; the other half is deciding what you
caught. A nil-pointer dereference is a code bug that must be logged with a stack
and often re-raised. An application `panic(error)` can be unwrapped and mapped to
a domain response. A `panic(nil)` — which since Go 1.21 surfaces as a
`*runtime.PanicNilError` — is a bug too. This module builds `Classify`, the
function every recovery boundary should run on the value it caught, returning a
typed `Severity` and the underlying error.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
classify/                  independent module: example.com/classify
  go.mod                   go 1.26
  classify.go              Severity, Classify(rec any) (Severity, error)
  cmd/
    demo/
      main.go              runnable demo classifying four kinds of panic
  classify_test.go         real runtime panics + app panic + panic(nil) cases
```

Files: `classify.go`, `cmd/demo/main.go`, `classify_test.go`.
Implement: a `Severity` enum (`SeverityUnknown`, `SeverityBug`, `SeverityApplication`) and `Classify(rec any) (Severity, error)` distinguishing `*runtime.PanicNilError`, `runtime.Error`, an application error, and a non-error value.
Test: trigger each real runtime panic (nil deref, index out of range, nil map write, bad type assertion) in a recovered closure and assert `SeverityBug`; assert `panic(errors.New(...))` is `SeverityApplication` and unwraps; assert `panic(nil)` recovers as `*runtime.PanicNilError` and is `SeverityBug`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/04-classify-runtime-panics/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/04-classify-runtime-panics
```

### The three severities and why they differ

`runtime.Error` is an interface: it is `error` plus a marker method
`RuntimeError()`. The runtime attaches it to the panics *it* generates — nil
pointer dereference, index or slice out of range, nil map write, integer divide by
zero, a failed type assertion. Recovering one almost always means you caught a
bug, not a handled condition: your program did something impossible. So `Classify`
tags it `SeverityBug`, and the caller's boundary should log it with a full stack,
bump a bug metric, and frequently re-panic so the crash-reporter fires. You detect
it with `errors.As(err, &runtimeErr)` where `runtimeErr` is a `runtime.Error`
interface variable — never by listing concrete types, because there are several
(`*runtime.TypeAssertionError`, the internal `boundsError`, `plainError`, and
more) and new ones can appear.

An application `panic(error)` is a value your own code chose to panic with. It is
not a `runtime.Error`, so it falls through to `SeverityApplication`, and
`Classify` returns it as the underlying error for the boundary to `Unwrap` and map
to a domain response. This is the only severity where converting the panic into a
normal returned error is legitimate.

`*runtime.PanicNilError` is what `recover` returns for `panic(nil)` since Go 1.21.
Before 1.21, `panic(nil)` made `recover` return `nil`, so `if r := recover(); r != nil` silently skipped it — a genuine footgun. Now `panic(nil)` produces a
`*runtime.PanicNilError` (which also satisfies `runtime.Error`), so the guard
finally behaves and the value is caught. `Classify` checks for it explicitly and
tags it `SeverityBug` — someone panicked with no value, which is a bug. The old
behavior is available only via `GODEBUG=panicnil=1`, and only for migrating legacy
code; new code should expect and classify the `*runtime.PanicNilError`.

Because `*runtime.PanicNilError` satisfies `runtime.Error`, the generic
`runtime.Error` check would already tag it `SeverityBug`. `Classify` still checks
`*runtime.PanicNilError` first so the boundary can report the panic-nil case
specifically (it is worth its own log message and metric). A non-error value —
`panic("string")`, `panic(42)` — cannot be unwrapped or matched, so it is
`SeverityUnknown` with a formatted error; the lesson-wide rule is to panic with an
error precisely so callers never land here.

Create `classify.go`:

```go
package classify

import (
	"errors"
	"fmt"
	"runtime"
)

// Severity ranks a recovered panic by what the boundary must do about it.
type Severity int

const (
	// SeverityUnknown is a non-error panic value (e.g. panic("string")).
	SeverityUnknown Severity = iota
	// SeverityBug is a runtime.Error or a panic(nil): a code defect.
	SeverityBug
	// SeverityApplication is a panic(error) the program chose: unwrap and map it.
	SeverityApplication
)

func (s Severity) String() string {
	switch s {
	case SeverityBug:
		return "bug"
	case SeverityApplication:
		return "application"
	default:
		return "unknown"
	}
}

// Classify inspects a recovered value and returns its severity plus the
// underlying error (nil for a nil recovery). Use it at a recovery boundary to
// decide whether to re-raise (bug) or map to a domain response (application).
func Classify(rec any) (Severity, error) {
	if rec == nil {
		return SeverityUnknown, nil
	}

	err, ok := rec.(error)
	if !ok {
		return SeverityUnknown, fmt.Errorf("non-error panic: %v", rec)
	}

	// panic(nil) since Go 1.21 surfaces as *runtime.PanicNilError; report it
	// specifically even though it also satisfies runtime.Error.
	var pne *runtime.PanicNilError
	if errors.As(err, &pne) {
		return SeverityBug, err
	}

	// Any runtime-generated panic (nil deref, bounds, nil map write, bad
	// assertion, divide by zero) is a code bug.
	var re runtime.Error
	if errors.As(err, &re) {
		return SeverityBug, err
	}

	// An application error the program panicked with: unwrap and handle it.
	return SeverityApplication, err
}
```

### The runnable demo

The demo recovers four kinds of panic and prints the severity `Classify` assigns
to each, so you can see the boundary's decision at a glance.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/classify"
)

func capture(f func()) (rec any) {
	defer func() { rec = recover() }()
	f()
	return nil
}

func main() {
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil deref", func() { var p *int; _ = *p }},
		{"app error", func() { panic(errors.New("order not found")) }},
		{"panic(nil)", func() { panic(nil) }},
		{"string panic", func() { panic("legacy string") }},
	}

	for _, c := range cases {
		sev, err := classify.Classify(capture(c.fn))
		fmt.Printf("%-13s -> %s (%v)\n", c.name, sev, err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
nil deref     -> bug (runtime error: invalid memory address or nil pointer dereference)
app error     -> application (order not found)
panic(nil)    -> bug (panic called with nil argument)
string panic  -> unknown (non-error panic: legacy string)
```

### Tests

The tests trigger *real* runtime panics — not fabricated errors — inside a
recovering `capture` helper, then assert `Classify` tags each `SeverityBug`.
`TestApplicationPanicUnwraps` proves an app error is `SeverityApplication` and the
returned error `errors.Is` the original. `TestPanicNilIsBug` proves `panic(nil)`
recovers as a non-nil `*runtime.PanicNilError` (documenting the 1.21 change) and
is tagged a bug. `TestNonErrorIsUnknown` covers a string panic.

Create `classify_test.go`:

```go
package classify

import (
	"errors"
	"runtime"
	"testing"
)

// capture runs f and returns whatever it panicked with (nil if it did not).
func capture(f func()) (rec any) {
	defer func() { rec = recover() }()
	f()
	return nil
}

func TestRuntimePanicsAreBugs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   func()
	}{
		{"nil pointer deref", func() { var p *int; _ = *p }},
		{"index out of range", func() { s := make([]int, 0); i := 3; _ = s[i] }},
		{"nil map write", func() { var m map[string]int; m["x"] = 1 }},
		{"failed type assertion", func() { var v any = "s"; _ = v.(int) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sev, err := Classify(capture(tc.fn))
			if sev != SeverityBug {
				t.Fatalf("severity = %s, want bug", sev)
			}
			var re runtime.Error
			if !errors.As(err, &re) {
				t.Fatalf("err = %v, want a runtime.Error", err)
			}
		})
	}
}

func TestApplicationPanicUnwraps(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("order not found")
	sev, err := Classify(capture(func() { panic(sentinel) }))
	if sev != SeverityApplication {
		t.Fatalf("severity = %s, want application", sev)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(%v, sentinel) = false, want true", err)
	}
}

func TestPanicNilIsBug(t *testing.T) {
	t.Parallel()

	rec := capture(func() { panic(nil) })
	// Since Go 1.21, panic(nil) recovers as a non-nil *runtime.PanicNilError.
	if rec == nil {
		t.Fatal("recover returned nil for panic(nil); expected *runtime.PanicNilError (Go 1.21+)")
	}
	var pne *runtime.PanicNilError
	if !errors.As(rec.(error), &pne) {
		t.Fatalf("recovered value %T is not a *runtime.PanicNilError", rec)
	}
	sev, _ := Classify(rec)
	if sev != SeverityBug {
		t.Fatalf("severity = %s, want bug", sev)
	}
}

func TestNonErrorIsUnknown(t *testing.T) {
	t.Parallel()

	sev, err := Classify(capture(func() { panic("legacy string") }))
	if sev != SeverityUnknown {
		t.Fatalf("severity = %s, want unknown", sev)
	}
	if err == nil {
		t.Fatal("want a formatted error for a non-error panic")
	}
}
```

## Review

`Classify` is correct when each real runtime panic — nil deref, out-of-range
index, nil map write, failed assertion — is tagged `SeverityBug` and detected via
`errors.As(err, &runtimeErr)` rather than a concrete-type switch, and when an
application `panic(error)` is `SeverityApplication` with the original reachable via
`errors.Is`. The two facts worth burning in: `runtime.Error` is an interface you
match structurally, and `panic(nil)` no longer disappears — since Go 1.21 it is a
`*runtime.PanicNilError` your boundary must expect (only `GODEBUG=panicnil=1`
brings back the old silent-`nil` behavior, for migration). The trap this closes is
recovering a runtime bug and returning a clean response, which hides the defect
from your on-call; classify first, then decide.

## Resources

- [runtime.Error](https://pkg.go.dev/runtime#Error) — the interface behind runtime-generated panics.
- [runtime.PanicNilError](https://pkg.go.dev/runtime#PanicNilError) — the Go 1.21 result of panic(nil).
- [Go 1.21 release notes](https://go.dev/doc/go1.21#language) — the panic(nil) behavior change and GODEBUG=panicnil.
- [errors.As](https://pkg.go.dev/errors#As) — matching an interface or concrete error through a chain.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-batch-worker-isolation.md](05-batch-worker-isolation.md)
