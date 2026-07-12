# Exercise 2: Debugging Stringer — Infinite Recursion And Pointer-Receiver Non-Satisfaction

Two of the most common `Stringer` bugs in production code are invisible until they
bite: a `String()` that recurses into itself and overflows the stack, and a
`String()` on a pointer receiver that makes every value — and every value inside a
slice or struct — silently print its integer instead of its name. This module
reproduces both on a log-level type, then builds the correct version and proves it
with tests.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
loglevel/                   independent module: example.com/loglevel
  go.mod
  level.go                  type Level int8; correct value-receiver String() with the raw trick
  cmd/
    demo/
      main.go               prints a Level, a []Level, and a struct field
  level_test.go             composite dispatch tests; the raw-conversion fallback; no-recursion guard
```

- Files: `level.go`, `cmd/demo/main.go`, `level_test.go`.
- Implement: a `Level int8` (`Debug`/`Info`/`Warn`/`Error`) with a *value*-receiver `String()` whose out-of-range fallback uses a locally defined `type raw Level` to format the underlying value without re-entering `String()`.
- Test: a `[]Level` and a struct field of type `Level` both render via `String()`; the `raw` fallback yields `Level(N)`; a guard call that returns normally (no recursion).
- Verify: `go test -count=1 -race ./...` and `go vet ./...`

### Bug 1: recursing through the receiver

The tempting one-liner is to build the string by formatting the receiver:

```go
// BROKEN: do not do this.
func (l Level) String() string {
	return fmt.Sprintf("level %v", l) // %v dispatches to String() -> forever
}
```

`%v` on a `Stringer` calls `String()`, so this method calls itself, and the call
chain grows until the goroutine's stack overflows. It is not a `panic` you can
`recover`; the runtime aborts the process with `runtime: goroutine stack exceeds
...`. `fmt.Sprint(l)` inside `String()` has the identical defect. The rule is: a
`String()` must never hand its own named receiver to a default-dispatching verb.

### Bug 2: a pointer receiver on a value type

The second bug is quieter. Declare `String()` on `*Level`:

```go
// BROKEN for value use: the method is only in *Level's method set.
func (l *Level) String() string { ... }
```

Now `*Level` satisfies `fmt.Stringer`, but `Level` does not. A `Level` value, and
every `Level` inside a slice, array, map, or struct field, is addressed as a value
during formatting, finds no `String()` on the value method set, and falls back to
Go's default:

```text
levels := []Level{LevelInfo, LevelError}
fmt.Sprint(levels)   // "[1 3]" — the integers, not "[INFO ERROR]"
```

The fix is a value receiver. A value method `func (l Level) String()` is in the
method set of both `Level` and `*Level`, so the value, the pointer, and every
composite that contains a `Level` all dispatch correctly.

### The fix, and the `type raw` trick

The correct `String()` switches on the known levels and, for an out-of-range
value, formats the *underlying* number. To keep the `Level(N)` fallback readable
we still want `%v`-style formatting, but calling `%v` on `l` would recurse. The
idiom is a locally defined type that shares `Level`'s underlying `int8` but not its
methods: `type raw Level`. Because methods are not inherited by a defined type,
`raw` has no `String()`, so `fmt.Sprintf("Level(%v)", raw(l))` formats the integer
and never re-enters `String()`.

Create `level.go`:

```go
package loglevel

import "fmt"

// Level is a logging severity. String() is on a value receiver so that a Level
// value, a []Level, and a Level struct field all format by name.
type Level int8

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String renders the level name. For an unknown value it prints Level(N) using
// a locally defined type without the String method, so the fallback does not
// recurse back into String().
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		type raw Level // no String method, so %v prints the underlying int8
		return fmt.Sprintf("Level(%v)", raw(l))
	}
}

// Event is a log record. Its Level field must format by name, which only works
// because String() is a value method.
type Event struct {
	Msg   string
	Level Level
}
```

### The runnable demo

The demo shows all three dispatch sites: a bare `Level`, a `[]Level`, and a
`Level` nested in a struct. It also prints the dynamic type via `reflect.TypeOf`
to make the "the value itself is a Stringer" point concrete.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"reflect"

	"example.com/loglevel"
)

func main() {
	l := loglevel.LevelWarn
	fmt.Printf("value: %s (type %s)\n", l, reflect.TypeOf(l))

	levels := []loglevel.Level{loglevel.LevelDebug, loglevel.LevelError}
	fmt.Printf("slice: %v\n", levels)

	ev := loglevel.Event{Msg: "disk full", Level: loglevel.LevelError}
	fmt.Printf("struct: %v\n", ev)

	fmt.Printf("unknown: %s\n", loglevel.Level(42))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value: WARN (type loglevel.Level)
slice: [DEBUG ERROR]
struct: {disk full ERROR}
unknown: Level(42)
```

### Tests

`TestSliceDispatch` and `TestStructFieldDispatch` are the ones that would have
caught the pointer-receiver bug: they assert a `[]Level` renders `[DEBUG ERROR]`
and a struct field renders `ERROR`, both of which only work when the value
satisfies `fmt.Stringer`. `TestUnknownUsesRawFallback` proves the recursion-safe
`Level(N)` path. `TestNoRecursion` calls the fixed `String()` directly and checks
it returns a value — the buggy recursive version is never executed because a stack
overflow is unrecoverable, so it is shown only illustratively above.

Create `level_test.go`:

```go
package loglevel

import (
	"fmt"
	"testing"
)

var _ fmt.Stringer = Level(0) // value satisfies Stringer; pointer receiver would break this

func TestKnownNames(t *testing.T) {
	t.Parallel()
	tests := map[Level]string{
		LevelDebug: "DEBUG",
		LevelInfo:  "INFO",
		LevelWarn:  "WARN",
		LevelError: "ERROR",
	}
	for l, want := range tests {
		if got := l.String(); got != want {
			t.Errorf("Level(%d).String() = %q, want %q", int8(l), got, want)
		}
	}
}

func TestSliceDispatch(t *testing.T) {
	t.Parallel()
	got := fmt.Sprint([]Level{LevelDebug, LevelError})
	if got != "[DEBUG ERROR]" {
		t.Fatalf("[]Level rendered %q, want [DEBUG ERROR]; is String() on a pointer receiver?", got)
	}
}

func TestStructFieldDispatch(t *testing.T) {
	t.Parallel()
	ev := Event{Msg: "boom", Level: LevelWarn}
	got := fmt.Sprintf("%v", ev)
	if got != "{boom WARN}" {
		t.Fatalf("struct rendered %q, want {boom WARN}", got)
	}
}

func TestUnknownUsesRawFallback(t *testing.T) {
	t.Parallel()
	for _, l := range []Level{42, -1, 100} {
		want := fmt.Sprintf("Level(%d)", int8(l))
		if got := l.String(); got != want {
			t.Errorf("Level(%d).String() = %q, want %q", int8(l), got, want)
		}
	}
}

func TestNoRecursion(t *testing.T) {
	t.Parallel()
	// The fixed String() returns immediately. A recursive version would never
	// reach this assertion (it would overflow the stack), so simply arriving
	// here with a correct value is the guard.
	if got := LevelInfo.String(); got != "INFO" {
		t.Fatalf("String() = %q, want INFO", got)
	}
}

func ExampleLevel_String() {
	fmt.Println([]Level{LevelInfo, LevelError, Level(9)})
	// Output: [INFO ERROR Level(9)]
}
```

## Review

The two failure modes here are both about *where the method lives* and *what it
formats*. A pointer receiver silently strips Stringer satisfaction from values and
from every composite that holds one, so the `var _ fmt.Stringer = Level(0)`
assertion plus `TestSliceDispatch` and `TestStructFieldDispatch` are the guards
that keep the method on a value receiver. Recursion comes from formatting the
named receiver with a dispatching verb; the `type raw Level` conversion is the
clean break, because a defined type does not carry the original's methods. If you
ever need the underlying integer inside `String()`, convert first — to `int8(l)`
for `%d`, or to `raw(l)` when you still want `%v`-style formatting — and never
pass `l` itself. Run `go vet` to confirm nothing else in the type reaches for a
recursive format.

## Resources

- [Go spec: Method sets](https://go.dev/ref/spec#Method_sets) — why a value method is in both method sets and a pointer method is not.
- [fmt package: Stringer](https://pkg.go.dev/fmt#Stringer) — the verbs that dispatch and the recursion caveat.
- [Go Code Review Comments: receiver type](https://go.dev/wiki/CodeReviewComments#receiver-type) — when to prefer value receivers.

---

Back to [01-status-enum-stringer.md](01-status-enum-stringer.md) | Next: [03-enum-text-round-trip.md](03-enum-text-round-trip.md)
