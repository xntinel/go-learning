# Exercise 8: Detect Formatting Capabilities For Structured Logging

A logging helper is handed an arbitrary `any` and must render it to a string. The
right representation depends on which optional interfaces the value implements —
`error`, `fmt.Stringer`, `encoding.TextMarshaler` — probed in a deliberate priority
order, with a `%v` fallback. This mirrors how `slog` and `fmt` pick a
representation, and the whole thing is interface-satisfaction assertions.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
logfmt/                     independent module: example.com/logfmt
  go.mod                    module path
  logfmt.go                 Render(v any) string with error > Stringer > TextMarshaler > %v
  cmd/
    demo/
      main.go               runnable demo rendering values of each kind
  logfmt_test.go            precedence, multi-interface value, MarshalText error, %v fallback
```

Files: `logfmt.go`, `cmd/demo/main.go`, `logfmt_test.go`.
Implement: `Render(v any) string` type-switching over `error`, `fmt.Stringer`, `encoding.TextMarshaler` in that order, then `%v`.
Test: a value implementing several interfaces resolves to the highest-priority one; a plain struct falls back to `%v`; a `MarshalText` that errors is handled without panic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logfmt/cmd/demo
cd ~/go-exercises/logfmt
go mod init example.com/logfmt
```

### Precedence is the whole design

`Render` asserts the value against three interface types, in order, and the first
match wins because a type switch evaluates its cases top to bottom. The order
encodes a policy:

`error` first, because if a value is an error the most useful log representation is
its `Error()` message — even if it also happens to be a `Stringer`. `fmt.Stringer`
next, because `String()` is the type's own chosen human representation. Then
`encoding.TextMarshaler`, whose `MarshalText` is meant for serialization but is a
reasonable textual form when nothing better exists. Finally `%v`, the reflection
based default that always works. Reordering these changes behavior: put `Stringer`
before `error` and an error value that is also a `Stringer` would log its `String()`
form instead of its message.

Two failure modes to handle. `MarshalText` returns `([]byte, error)`; when it errors
you must not propagate a broken serialization into the log line — fall back to `%v`
rather than panicking or logging an empty string. And a value implementing none of
the three must still render, which is why `%v` is the `default` and not an error.
Note that all four arms are interface assertions or a catch-all; none uses the panic
form, because the input is `any` and therefore a boundary.

Create `logfmt.go`:

```go
package logfmt

import (
	"encoding"
	"fmt"
)

// Render picks the best textual representation of v by probing optional interfaces
// in priority order: error, then fmt.Stringer, then encoding.TextMarshaler, then a
// reflection-based %v fallback. A MarshalText error falls back to %v, never panics.
func Render(v any) string {
	switch t := v.(type) {
	case error:
		return t.Error()
	case fmt.Stringer:
		return t.String()
	case encoding.TextMarshaler:
		b, err := t.MarshalText()
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/logfmt"
)

type user struct {
	ID   int
	Name string
}

func (u user) String() string { return "user#" + fmt.Sprint(u.ID) + "(" + u.Name + ")" }

// version implements only encoding.TextMarshaler (not Stringer), so Render reaches
// the TextMarshaler arm. A type like time.Time would hit the Stringer arm first.
type version struct {
	major, minor int
}

func (v version) MarshalText() ([]byte, error) {
	return []byte(fmt.Sprintf("v%d.%d", v.major, v.minor)), nil
}

func main() {
	values := []any{
		errors.New("connection refused"),
		user{ID: 7, Name: "alice"},
		version{major: 1, minor: 4},
		struct{ A, B int }{1, 2}, // falls back to %v
	}
	for _, v := range values {
		fmt.Println(logfmt.Render(v))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
connection refused
user#7(alice)
v1.4
{1 2}
```

### Tests

`bothErrorAndStringer` implements both interfaces and must resolve to its `Error()`
message, proving precedence. `badText` returns an error from `MarshalText` and must
fall back to `%v` without panicking. A plain struct exercises the `%v` default.

Create `logfmt_test.go`:

```go
package logfmt

import (
	"errors"
	"fmt"
	"testing"
)

type stringerOnly struct{}

func (stringerOnly) String() string { return "STRINGER" }

type textOnly struct{}

func (textOnly) MarshalText() ([]byte, error) { return []byte("TEXT"), nil }

type bothErrorAndStringer struct{}

func (bothErrorAndStringer) Error() string  { return "ERROR" }
func (bothErrorAndStringer) String() string { return "STRINGER" }

type badText struct{}

func (badText) MarshalText() ([]byte, error) { return nil, errors.New("marshal failed") }

func TestRenderPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   any
		want string
	}{
		{"error", errors.New("boom"), "boom"},
		{"stringer", stringerOnly{}, "STRINGER"},
		{"text marshaler", textOnly{}, "TEXT"},
		{"error beats stringer", bothErrorAndStringer{}, "ERROR"},
		{"bad marshaltext falls back", badText{}, "{}"},
		{"plain struct fallback", struct{ A int }{5}, "{5}"},
		{"nil", nil, "<nil>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Render(tc.in); got != tc.want {
				t.Fatalf("Render(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func ExampleRender() {
	fmt.Println(Render(errors.New("disk full")))
	fmt.Println(Render(stringerOnly{}))
	// Output:
	// disk full
	// STRINGER
}
```

## Review

`Render` is correct when the highest-priority satisfied interface determines the
output: an error's message wins over its `String()`, a `Stringer` wins over a
`TextMarshaler`, and a value implementing none still renders via `%v`. The
`error beats stringer` case is the precedence proof, and `bad marshaltext falls back`
proves the `MarshalText` error path degrades to `%v` instead of panicking or logging
garbage. The subtle point is that all four arms are interface assertions handled by
the ordered type switch — reorder the cases and you change the policy. Run
`go test -race` to confirm every precedence rule.

## Resources

- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer)
- [encoding.TextMarshaler](https://pkg.go.dev/encoding#TextMarshaler)
- [log/slog: LogValuer](https://pkg.go.dev/log/slog#LogValuer) — how slog itself resolves a value's representation.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-event-dispatcher-type-switch.md](07-event-dispatcher-type-switch.md) | Next: [09-driver-value-normalizer.md](09-driver-value-normalizer.md)
