# Exercise 1: Job Status Enum With A Pure Stringer

A worker's job status is the enum you print thousands of times a day — in logs,
error messages, and dashboards. This is the baseline artifact every later module
extends: a `Status` enum whose `String()` is a pure value-receiver switch, with a
`TypeName(N)` fallback for out-of-range values and an `IsTerminal()` helper.

This module is fully self-contained. It has its own `go mod init`, all its code
inline, its own demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
statusenum/                 independent module: example.com/statusenum
  go.mod
  status.go                 type Status uint8; String() switch; IsTerminal()
  cmd/
    demo/
      main.go               prints each status under %s and %v
  status_test.go            table test; unknown(99); %s==%v==String(); Stringer assertion
```

- Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
- Implement: a `Status uint8` enum (`StatusUnknown`, `StatusPending`, `StatusRunning`, `StatusSucceeded`, `StatusFailed`) with a value-receiver `String()` switching to lowercase names, an `unknown(N)` default, and `IsTerminal()`.
- Test: each status maps to its expected string; `Status(99)` yields `unknown(99)`; `%s` and `%v` both equal `String()`; a compile-time `var _ fmt.Stringer = Status(0)`; the `IsTerminal` truth table.
- Verify: `go test -count=1 -race ./...`

### Why a value receiver and a TypeName(N) default

Two decisions define this type. First, `String()` is declared on a *value*
receiver, `func (s Status) String() string`. That places the method in the method
set of both `Status` and `*Status`, so a `Status` value — and a `Status` sitting
inside a slice, a map, or a struct field — all satisfy `fmt.Stringer` and format
correctly. A pointer receiver here would be a latent bug: the value would silently
fall back to printing its integer. For a small immutable enum, value receivers are
the correct and idiomatic choice.

Second, the default case returns `unknown(N)` rather than `""`. An enum stored in
a database or received over the wire can arrive out of range — because a caller
sent garbage, or because an integer was persisted and the constant it named was
later removed. Returning the empty string would produce a blank, undiagnosable log
line; returning `unknown(99)` keeps the offending value visible so the corruption
is traceable. `strconv.FormatUint` builds that fallback without pulling `fmt` into
the type's core, though `fmt.Sprintf` would work equally.

`IsTerminal()` encodes a domain fact: `Succeeded` and `Failed` are absorbing
states a job cannot leave. A scheduler uses it to decide when to stop polling.

Create `status.go`:

```go
package statusenum

import "strconv"

// Status is the lifecycle state of a background job. Its underlying type is
// uint8, but that integer is an implementation detail: never persist or transmit
// it (see the later modules). Format it through String() for humans.
type Status uint8

const (
	StatusUnknown Status = iota
	StatusPending
	StatusRunning
	StatusSucceeded
	StatusFailed
)

// String returns the lowercase human name of the status. It is a pure function
// of the receiver: idempotent, side-effect-free, safe to call from many
// goroutines in a log hot path. Out-of-range values render as unknown(N) so a
// corrupted value stays diagnosable instead of printing blank.
func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusSucceeded:
		return "succeeded"
	case StatusFailed:
		return "failed"
	default:
		return "unknown(" + strconv.FormatUint(uint64(s), 10) + ")"
	}
}

// IsTerminal reports whether the job can no longer change state. Succeeded and
// Failed are absorbing; a scheduler stops polling once IsTerminal is true.
func (s Status) IsTerminal() bool {
	return s == StatusSucceeded || s == StatusFailed
}
```

### The runnable demo

The demo prints each status through `%s` and `%v` so you can see both verbs
dispatch to `String()`, and prints an out-of-range value to see the fallback.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/statusenum"
)

func main() {
	all := []statusenum.Status{
		statusenum.StatusUnknown,
		statusenum.StatusPending,
		statusenum.StatusRunning,
		statusenum.StatusSucceeded,
		statusenum.StatusFailed,
	}
	for _, s := range all {
		fmt.Printf("%%s=%s %%v=%v terminal=%t\n", s, s, s.IsTerminal())
	}
	fmt.Printf("out of range: %s\n", statusenum.Status(99))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
%s=unknown(0) %v=unknown(0) terminal=false
%s=pending %v=pending terminal=false
%s=running %v=running terminal=false
%s=succeeded %v=succeeded terminal=true
%s=failed %v=failed terminal=false
out of range: unknown(99)
```

### Tests

The tests pin the four properties that make this a real Stringer: every known
value maps to its name, an out-of-range value yields `unknown(N)`, `%s` and `%v`
both route through `String()`, and the type satisfies `fmt.Stringer` at compile
time. The `var _ fmt.Stringer = Status(0)` line is the compile-time assertion:
if a later edit moved `String()` to a pointer receiver, this file would stop
compiling — the drift is caught by the build, not by a runtime surprise.

Create `status_test.go`:

```go
package statusenum

import (
	"fmt"
	"testing"
)

// Compile-time proof that a Status value satisfies fmt.Stringer. If String()
// were ever moved to a pointer receiver, this line would fail to compile.
var _ fmt.Stringer = Status(0)

func TestStringKnownValues(t *testing.T) {
	t.Parallel()
	tests := map[Status]string{
		StatusUnknown:   "unknown(0)",
		StatusPending:   "pending",
		StatusRunning:   "running",
		StatusSucceeded: "succeeded",
		StatusFailed:    "failed",
	}
	for s, want := range tests {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", uint8(s), got, want)
		}
	}
}

func TestStringOutOfRange(t *testing.T) {
	t.Parallel()
	if got := Status(99).String(); got != "unknown(99)" {
		t.Fatalf("Status(99).String() = %q, want unknown(99)", got)
	}
}

func TestVerbsDispatchToString(t *testing.T) {
	t.Parallel()
	for _, s := range []Status{StatusUnknown, StatusRunning, Status(200)} {
		want := s.String()
		if got := fmt.Sprintf("%s", s); got != want {
			t.Errorf("%%s = %q, want %q", got, want)
		}
		if got := fmt.Sprintf("%v", s); got != want {
			t.Errorf("%%v = %q, want %q", got, want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	tests := map[Status]bool{
		StatusUnknown:   false,
		StatusPending:   false,
		StatusRunning:   false,
		StatusSucceeded: true,
		StatusFailed:    true,
	}
	for s, want := range tests {
		if got := s.IsTerminal(); got != want {
			t.Errorf("%s.IsTerminal() = %t, want %t", s, got, want)
		}
	}
}

func ExampleStatus_String() {
	fmt.Println(StatusRunning)
	fmt.Printf("%s\n", StatusSucceeded)
	// Output:
	// running
	// succeeded
}
```

## Review

The type is correct when `String()` is a total function: every input, in range or
not, produces a stable non-empty string, and `%s`/`%v`/`Sprint` all agree with a
direct `String()` call. `TestVerbsDispatchToString` deliberately includes an
out-of-range value so the fallback path is exercised through the verbs too. The
compile-time `var _ fmt.Stringer = Status(0)` is not decoration: it is the
cheapest possible guard against the number-one Stringer bug, a method quietly
moved to a pointer receiver. Keep `String()` pure — no logging, no counters, no
locks — because `fmt` will call it from hot paths and many goroutines. And resist
storing `uint8(s)` anywhere durable; the later modules build the text, JSON, and
SQL representations that make this enum safe to persist and transmit.

## Resources

- [fmt package: Stringer](https://pkg.go.dev/fmt#Stringer) — the interface and the verbs that dispatch to it.
- [Effective Go: printing](https://go.dev/doc/effective_go#printing) — the Stringer convention and the `TypeName(N)` fallback style.
- [strconv.FormatUint](https://pkg.go.dev/strconv#FormatUint) — building the numeric fallback without `fmt`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-stringer-value-vs-pointer-and-recursion.md](02-stringer-value-vs-pointer-and-recursion.md)
