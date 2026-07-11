# Exercise 1: Validated Structured Attribute Emitter

`t.Attr` will happily emit metadata that corrupts the `go test -json` stream: a key
with a space, a value with a newline. This exercise builds the small, reusable gate
that validates every key/value pair against the documented `Attr` contract and emits
in a stable order, so untrusted metadata never reaches the test log unchecked.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
attremit/                   independent module: example.com/attremit
  go.mod                    go 1.25 (T.Attr needs it)
  emit.go                   AttrSink interface; Emit; sentinel errors; validation
  cmd/
    demo/
      main.go               emits a sample metadata map to a printing sink
  emit_test.go              table-driven validation tests; real *testing.T sink; Example
```

- Files: `emit.go`, `cmd/demo/main.go`, `emit_test.go`.
- Implement: `Emit(sink AttrSink, kv map[string]string) error` that validates each pair against the `T.Attr` contract, and only if all pass forwards them to `sink.Attr` in ascending key order; otherwise returns a wrapped sentinel error and emits nothing.
- Test: a recording fake `AttrSink` asserting sorted-order emission for valid maps and no emission plus the right sentinel for invalid keys/values; one subtest passing the real `*testing.T` as the sink.
- Verify: `go test -count=1 -race ./...`

Set up the module. `testing.T.Attr` requires Go 1.25+, so pin the language version:

```bash
mkdir -p ~/go-exercises/attremit/cmd/demo
cd ~/go-exercises/attremit
go mod init example.com/attremit
go mod edit -go=1.25
```

### Why a one-method interface

The producer of test metadata should be unit-testable without a running test, yet
usable against a real `*testing.T` in production. Both goals are met by defining the
smallest possible interface the emitter needs:

```
type AttrSink interface{ Attr(key, value string) }
```

`*testing.T` already has a method `Attr(key, value string)`, so it satisfies
`AttrSink` structurally with no adapter. In a unit test you pass a recording fake
that appends each call to a slice; in a real test you pass `t` directly. The emitter
never imports `testing`, so it stays a plain library that happens to be drivable by a
test. This is the same dependency-inversion move you would make for any subsystem you
want to test in isolation, applied to the testing API itself.

### Validate-all-then-emit

The contract from the concepts file is precise: the key must not be empty or contain
whitespace, and the value must not contain a newline or carriage return. The emitter
enforces all of it before touching the sink. It validates every pair first and only
then, in a second pass, calls `sink.Attr`. That two-pass shape is deliberate: emitting
attributes is a side effect on the shared test log, and a half-emitted batch (three
good attributes, then an error on the fourth) would leave the log in a state that
depends on map iteration order. By validating everything up front, `Emit` is
all-or-nothing — on any invalid pair it returns an error and emits nothing.

Ordering is the other design decision. Attributes are unordered on the wire, so a test
that asserts on emitted output would flake if the emitter walked the map in Go's
randomized iteration order. `Emit` imposes a stable order by sorting the keys with
`slices.Sorted(maps.Keys(kv))` before both passes. `maps.Keys` returns an iterator
over the map's keys and `slices.Sorted` collects it into a sorted slice, giving a
deterministic sequence for free. Validation walks the sorted keys too, so the error
you get for a map with several bad pairs is itself deterministic (the
lexicographically first offending key).

Validation uses stdlib predicates rather than hand-rolled loops: `strings.IndexFunc`
with `unicode.IsSpace` finds any whitespace rune in the key (covering spaces, tabs,
and less obvious runes like the vertical tab), and `strings.ContainsAny(value,
"\n\r")` catches either line terminator in the value. Each failure returns a distinct
sentinel wrapped with `%w`, so a caller can branch on the reason with `errors.Is`.

Create `emit.go`:

```go
package attremit

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
)

// AttrSink receives validated key/value attributes. *testing.T satisfies it via
// its Attr(key, value string) method, so Emit can target a recording fake in a
// unit test or a real test in production without any adapter.
type AttrSink interface {
	Attr(key, value string)
}

// Sentinel errors describe why a pair was rejected; callers branch with errors.Is.
var (
	ErrEmptyKey      = errors.New("attr: key must not be empty")
	ErrKeyWhitespace = errors.New("attr: key must not contain whitespace")
	ErrValueNewline  = errors.New("attr: value must not contain newline or carriage return")
)

// Emit validates every pair in kv against the testing.T.Attr contract (key:
// non-empty and whitespace-free; value: no newline or carriage return) and, only
// if all pass, forwards them to sink in ascending key order. Attributes are
// unordered on the wire, so Emit imposes a stable order for reproducible output.
// On the first invalid pair it returns a wrapped sentinel error and emits nothing.
func Emit(sink AttrSink, kv map[string]string) error {
	keys := slices.Sorted(maps.Keys(kv))
	for _, k := range keys {
		switch {
		case k == "":
			return fmt.Errorf("attr: %w", ErrEmptyKey)
		case strings.IndexFunc(k, unicode.IsSpace) >= 0:
			return fmt.Errorf("attr: key %q: %w", k, ErrKeyWhitespace)
		case strings.ContainsAny(kv[k], "\n\r"):
			return fmt.Errorf("attr: value for key %q: %w", k, ErrValueNewline)
		}
	}
	for _, k := range keys {
		sink.Attr(k, kv[k])
	}
	return nil
}
```

### The runnable demo

The demo builds a realistic per-test metadata map — a case id, an owner, a commit,
and a numeric SLO budget serialized with `strconv.Itoa` because `Attr` takes only
strings — and emits it to a sink that prints each pair in the `=== ATTR` shape the
real test log would use. It shows the sorted ordering the emitter imposes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"strconv"

	"example.com/attremit"
)

// stdoutSink mimics the verbose test log's attribute line so you can see what the
// emitter would produce inside a real test.
type stdoutSink struct{}

func (stdoutSink) Attr(key, value string) {
	fmt.Printf("=== ATTR  demo %s %s\n", key, value)
}

func main() {
	meta := map[string]string{
		"case_id": "TC-1042",
		"owner":   "payments",
		"commit":  "9f3a1c2",
		"slo_ms":  strconv.Itoa(250), // numeric metric serialized before emission
	}
	if err := attremit.Emit(stdoutSink{}, meta); err != nil {
		fmt.Fprintln(os.Stderr, "emit failed:", err)
		os.Exit(1)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== ATTR  demo case_id TC-1042
=== ATTR  demo commit 9f3a1c2
=== ATTR  demo owner payments
=== ATTR  demo slo_ms 250
```

### Tests

The unit tests drive `Emit` with a recording fake that captures every `Attr` call.
A valid map must produce calls in sorted-key order; an invalid key or value must
produce zero calls and the matching sentinel, asserted with `errors.Is`. A separate
subtest passes the real `*testing.T` as the sink to prove the interface compatibility
is not theoretical — run it under `go test -v` and you will see the `=== ATTR` lines.
The `Example` uses a printing sink to demonstrate the deterministic ordering with an
`// Output:` block the test runner verifies.

Create `emit_test.go`:

```go
package attremit

import (
	"errors"
	"fmt"
	"testing"
)

type attrCall struct{ Key, Value string }

// recordingSink is a fake AttrSink that records every emitted pair in order.
type recordingSink struct {
	calls []attrCall
}

func (r *recordingSink) Attr(key, value string) {
	r.calls = append(r.calls, attrCall{key, value})
}

func TestEmitValidSortedOrder(t *testing.T) {
	t.Parallel()
	var sink recordingSink
	err := Emit(&sink, map[string]string{
		"owner":   "payments",
		"case_id": "TC-1",
		"commit":  "abc",
	})
	if err != nil {
		t.Fatalf("Emit returned error on valid input: %v", err)
	}
	want := []attrCall{
		{"case_id", "TC-1"},
		{"commit", "abc"},
		{"owner", "payments"},
	}
	if len(sink.calls) != len(want) {
		t.Fatalf("got %d calls, want %d: %v", len(sink.calls), len(want), sink.calls)
	}
	for i, c := range sink.calls {
		if c != want[i] {
			t.Errorf("call %d = %v, want %v", i, c, want[i])
		}
	}
}

func TestEmitRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		kv   map[string]string
		want error
	}{
		{"empty key", map[string]string{"": "x"}, ErrEmptyKey},
		{"space in key", map[string]string{"case id": "x"}, ErrKeyWhitespace},
		{"tab in key", map[string]string{"case\tid": "x"}, ErrKeyWhitespace},
		{"newline in value", map[string]string{"k": "a\nb"}, ErrValueNewline},
		{"carriage return in value", map[string]string{"k": "a\rb"}, ErrValueNewline},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sink recordingSink
			err := Emit(&sink, tc.kv)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Emit error = %v, want errors.Is(_, %v)", err, tc.want)
			}
			if len(sink.calls) != 0 {
				t.Fatalf("invalid input still emitted %d attrs: %v", len(sink.calls), sink.calls)
			}
		})
	}
}

// TestEmitToRealT proves *testing.T satisfies AttrSink: this compiles and runs
// only because T has an Attr(key, value string) method. Run `go test -v` to see
// the resulting `=== ATTR` lines.
func TestEmitToRealT(t *testing.T) {
	t.Parallel()
	var sink AttrSink = t
	if err := Emit(sink, map[string]string{"owner": "payments", "case_id": "TC-1"}); err != nil {
		t.Fatalf("Emit to *testing.T: %v", err)
	}
}

// printSink writes each pair as key=value so the Example can assert exact output.
type printSink struct{}

func (printSink) Attr(key, value string) { fmt.Printf("%s=%s\n", key, value) }

func ExampleEmit() {
	_ = Emit(printSink{}, map[string]string{
		"owner":   "payments",
		"case_id": "TC-42",
		"commit":  "abc123",
	})
	// Output:
	// case_id=TC-42
	// commit=abc123
	// owner=payments
}
```

## Review

The emitter is correct when it is all-or-nothing and order-stable. All-or-nothing
means a map with one bad pair emits zero attributes: `TestEmitRejects` asserts that
`len(sink.calls) == 0` after every rejection, which would fail if you emitted as you
validated instead of validating first. Order-stable means the calls come out sorted
by key regardless of map iteration order; `TestEmitValidSortedOrder` pins that, and
it would flake if you ranged over the map directly instead of over
`slices.Sorted(maps.Keys(kv))`.

The mistakes to avoid are the contract's own traps. Do not accept a numeric value
without serializing it first — `Attr` takes a `string`, and the demo shows
`strconv.Itoa` doing that job. Do not let a whitespace key or a newline value through;
the two-pass validation exists precisely so those never reach the sink, and each is
tied to a distinct sentinel you assert with `errors.Is`. Confirm interface
compatibility is real, not assumed: `TestEmitToRealT` assigns `t` to an `AttrSink`
variable, which only compiles because `*testing.T` carries the Go 1.25 `Attr` method.
Run `go test -race -v` to watch the real attribute lines appear.

## Resources

- [`testing.T.Attr`](https://pkg.go.dev/testing#T.Attr) — the method, its string arguments, and the whitespace/newline constraints.
- [Go 1.25 release notes — testing](https://go.dev/doc/go1.25#testing) — `T.Attr`/`B.Attr`/`F.Attr`, the `=== ATTR` text format, and the `-json` attr action.
- [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) and [`maps.Keys`](https://pkg.go.dev/maps#Keys) — collecting a map's keys into a deterministic sorted slice.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-test-output-slog-handler.md](02-test-output-slog-handler.md)
