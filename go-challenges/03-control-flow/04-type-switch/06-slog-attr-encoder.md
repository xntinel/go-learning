# Exercise 6: Render Log Attributes with a Type Switch in a slog Adapter

A custom `slog.Handler` must turn each attribute's value into a canonical textual
form: strings quoted, times in RFC3339, errors via `.Error()`, byte slices as
hex. The value arrives as `any` (through `slog.Value.Any()`), so the encoder is a
type switch — and this one mixes concrete cases with interface cases, where
ordering decides the outcome.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
logenc/                      independent module: example.com/logenc
  go.mod                     go 1.26
  encode.go                  EncodeValue(v any) string; EncodeAttr(a slog.Attr) string
  cmd/
    demo/
      main.go                encodes a set of attributes into a logfmt line
  encode_test.go             per-type canonical form; error-before-Stringer ordering; nil token
```

- Files: `encode.go`, `cmd/demo/main.go`, `encode_test.go`.
- Implement: `EncodeValue(v any) string` type-switching over the concrete value
  and interface cases (`error`, `fmt.Stringer`), and `EncodeAttr(a slog.Attr)
  string` using `a.Value.Any()`.
- Test: canonical encoding for `string` (quoted), `int64`, `bool`, `time.Time`
  (RFC3339), a value implementing `error`, a value implementing `fmt.Stringer`,
  and `[]byte` (hex); a type implementing BOTH `error` and `fmt.Stringer` hits the
  case listed first; `nil` renders a stable token, not a panic.
- Verify: `go test -count=1 -race ./...`

## Concrete cases first, interface cases last, and why order is the design

A logfmt encoder needs one canonical rendering per value shape so logs are
greppable and machine-parseable. `EncodeValue` type-switches:

- concrete scalar cases (`string`, `bool`, `int64`, `int`, `float64`) render with
  `strconv`, quoting strings so a value with spaces stays one token;
- `time.Time` renders as RFC3339;
- `[]byte` renders as hex (raw bytes are not printable);
- then the interface cases `error` (render `.Error()`) and `fmt.Stringer` (render
  `.String()`);
- `default` falls back to a quoted `fmt.Sprint`.

Two ordering facts make this correct, and both are silent bugs if you get them
wrong. First, `time.Time` implements `fmt.Stringer` (it has a `String` method), so
`case time.Time` must come *before* `case fmt.Stringer`; otherwise every time
value would render through `String()` in Go's default format instead of RFC3339.
Concrete-before-interface handles this automatically. Second, `case error` is
listed before `case fmt.Stringer` deliberately: a type that implements *both*
should log through its error form, because for a log an error's message is the
useful text. A type implementing both hits whichever case is written first, so
the order encodes a real policy decision — the test pins it.

`EncodeAttr` bridges to `slog`: it reads `a.Value.Any()` to get the underlying
value out of the `slog.Value` and feeds it to `EncodeValue`, producing `key=value`.

Create `encode.go`:

```go
package logenc

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// EncodeValue renders v into a canonical logfmt token. Concrete cases are listed
// before interface cases so, for example, time.Time uses RFC3339 rather than its
// Stringer form; error precedes fmt.Stringer so a type that is both logs as an error.
func EncodeValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(x)
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case time.Time:
		return x.Format(time.RFC3339)
	case []byte:
		return hex.EncodeToString(x)
	case error:
		return strconv.Quote(x.Error())
	case fmt.Stringer:
		return strconv.Quote(x.String())
	default:
		return strconv.Quote(fmt.Sprint(x))
	}
}

// EncodeAttr renders a slog.Attr as key=value, pulling the concrete value out of
// the slog.Value via Any.
func EncodeAttr(a slog.Attr) string {
	return a.Key + "=" + EncodeValue(a.Value.Any())
}
```

## The runnable demo

The demo builds a handful of `slog.Attr` values of different kinds and joins their
encodings into one logfmt line.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"example.com/logenc"
)

func main() {
	ts := time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC)
	attrs := []slog.Attr{
		slog.String("msg", "user login"),
		slog.Int64("uid", 42),
		slog.Bool("admin", false),
		slog.Time("at", ts),
		slog.Any("err", errors.New("token expired")),
		slog.Any("digest", []byte{0xde, 0xad, 0xbe, 0xef}),
	}

	parts := make([]string, len(attrs))
	for i, a := range attrs {
		parts[i] = logenc.EncodeAttr(a)
	}
	fmt.Println(strings.Join(parts, " "))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
msg="user login" uid=42 admin=false at=2026-07-02T15:04:05Z err="token expired" digest=deadbeef
```

## Tests

The table test asserts the canonical form of each kind. The ordering test hands
`EncodeValue` a type implementing both `error` and `fmt.Stringer` and asserts it
took the `error` branch (the one listed first). The nil test asserts a stable
`null` token with no panic.

Create `encode_test.go`:

```go
package logenc

import (
	"errors"
	"log/slog"
	"testing"
	"time"
)

type onlyStringer struct{}

func (onlyStringer) String() string { return "S" }

type both struct{}

func (both) Error() string  { return "as-error" }
func (both) String() string { return "as-stringer" }

func TestEncodeValue(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"string", "a b", `"a b"`},
		{"int64", int64(42), "42"},
		{"bool", true, "true"},
		{"float64", 3.5, "3.5"},
		{"time", ts, "2026-01-02T03:04:05Z"},
		{"bytes", []byte{0xde, 0xad}, "dead"},
		{"error", errors.New("boom"), `"boom"`},
		{"stringer", onlyStringer{}, `"S"`},
		{"nil", nil, "null"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := EncodeValue(tc.in); got != tc.want {
				t.Fatalf("EncodeValue(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestErrorBeatsStringer(t *testing.T) {
	t.Parallel()
	// both implements error and fmt.Stringer; case error is listed first.
	if got := EncodeValue(both{}); got != `"as-error"` {
		t.Fatalf("EncodeValue(both) = %q, want %q (error case must win)", got, `"as-error"`)
	}
}

func TestEncodeAttr(t *testing.T) {
	t.Parallel()
	got := EncodeAttr(slog.String("k", "v"))
	if got != `k="v"` {
		t.Fatalf("EncodeAttr = %q, want %q", got, `k="v"`)
	}
}
```

## Review

The encoder is correct when each kind has exactly one canonical form, when a
`time.Time` renders as RFC3339 rather than through its `String` method, and when a
type that is both an `error` and a `fmt.Stringer` logs through its error form. The
mistakes are ordering mistakes: placing `case fmt.Stringer` before `case
time.Time` silently changes every timestamp's format, and placing `case
fmt.Stringer` before `case error` silently changes how dual-interface values log.
Concrete cases first, then the most meaningful interface first — and a test that
pins the dual-interface case, because nothing in the compiler will.

## Resources

- [log/slog.Value and Value.Any](https://pkg.go.dev/log/slog#Value.Any)
- [log/slog.Attr](https://pkg.go.dev/log/slog#Attr)
- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer)
- [encoding/hex.EncodeToString](https://pkg.go.dev/encoding/hex#EncodeToString)

---

Prev: [05-config-value-coercion.md](05-config-value-coercion.md) | Up: [00-concepts.md](00-concepts.md) | Next: [07-command-router-worker.md](07-command-router-worker.md)
