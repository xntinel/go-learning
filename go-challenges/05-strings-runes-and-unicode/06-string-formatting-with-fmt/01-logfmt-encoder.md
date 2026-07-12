# Exercise 1: A logfmt Structured-Logging Encoder

The first artifact is the front of a log pipeline: an encoder that turns a level, a
message, and variadic key-value pairs into one parseable `level=... msg="..."
key=value` line. It is where `%q` quoting, domain-type rendering, and
boundary validation all meet.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
logfmt/                    independent module: example.com/logfmt
  go.mod                   go 1.24
  logfmt.go                type Level; Encode(level, msg, kv ...any) (string, error); formatValue
  cmd/
    demo/
      main.go              runnable demo: emit an INFO and an ERROR line
  logfmt_test.go           golden-line + substring + error-path + round-trip tests
```

- Files: `logfmt.go`, `cmd/demo/main.go`, `logfmt_test.go`.
- Implement: `Encode(level Level, msg string, kv ...any) (string, error)` that emits `level=<level> msg=<quoted>` followed by one ` key=value` per pair, `%q`-quoting strings and errors, rendering `time.Duration` as `750ms`, and rejecting an empty level, an empty message, an odd `kv` count, or a non-string key.
- Test: golden-line assertions for simple fields; substring checks for quoted names, `err="..."`, and `elapsed=750ms`; error-path tests for every boundary; a split-and-parse test asserting every emitted token is a well-formed `key=value` pair.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/06-string-formatting-with-fmt/01-logfmt-encoder/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/06-string-formatting-with-fmt/01-logfmt-encoder
go mod edit -go=1.24
```

### Why the value formatting is a separate function

The encoder has two distinct jobs: the *frame* (`level=... msg=...`, then the loop
over pairs) and the *per-value rendering*. Splitting `formatValue` out keeps the
frame readable and puts the type-dependent policy in one place. That policy is the
interesting part:

- A `string` value is rendered with `%q`, so `Alice Smith` becomes
  `"Alice Smith"` — one token to the parser, with quotes/backslashes/control
  characters escaped for free. Hand-concatenating quotes here would be the classic
  bug (it misses escaping).
- An `error` is rendered as `%q` of its `Error()` string, so a failure becomes
  `err="connection refused"` rather than leaking a multi-word phrase that a
  logfmt parser would split.
- Everything else falls through to `%v`, which is exactly what makes durations
  work: `time.Duration` has a `String()` method, so `750 * time.Millisecond`
  renders as `750ms`, not the raw `750000000`. This is the Stringer dispatch from
  the concepts file doing real work — you get correct rendering of any domain type
  that implements `String()` for free.

The validation is the boundary contract. An empty level or message, an odd number
of key-value arguments, or a key that is not a string are all *caller* bugs, and
the encoder returns an error rather than emitting a malformed line that would
corrupt the downstream parser. `%T` in the non-string-key error reports the
offending type, which is what a developer needs to fix the call site.

Create `logfmt.go`:

```go
package logfmt

import (
	"errors"
	"fmt"
	"strings"
)

// Level is a log severity that renders as its own text (INFO, ERROR, ...).
type Level string

const (
	LevelDebug Level = "DEBUG"
	LevelInfo  Level = "INFO"
	LevelWarn  Level = "WARN"
	LevelError Level = "ERROR"
)

// Encode renders a single logfmt line: level=<level> msg=<quoted> followed by
// one space-separated key=value per pair in kv. Strings and errors are %q-quoted;
// any other value uses its default %v rendering (so time.Duration prints as
// 750ms). It validates at the boundary and never emits a malformed line.
func Encode(level Level, msg string, kv ...any) (string, error) {
	if level == "" {
		return "", errors.New("empty level")
	}
	if msg == "" {
		return "", errors.New("empty message")
	}
	if len(kv)%2 != 0 {
		return "", fmt.Errorf("odd number of key-value pairs: %d", len(kv))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "level=%s msg=%q", level, msg)
	for i := 0; i < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			return "", fmt.Errorf("key at index %d is %T, want string", i, kv[i])
		}
		fmt.Fprintf(&b, " %s=%v", key, formatValue(kv[i+1]))
	}
	return b.String(), nil
}

// formatValue renders one value: strings and errors are safely quoted; anything
// else uses %v, which routes through the value's Stringer if it has one.
func formatValue(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("%q", x)
	case error:
		return fmt.Sprintf("%q", x.Error())
	default:
		return fmt.Sprintf("%v", x)
	}
}
```

### The runnable demo

The demo emits the two lines a real service produces most: a successful request
and an upstream failure. The duration renders through its Stringer and the error
through `%q`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/logfmt"
)

func main() {
	line, err := logfmt.Encode(logfmt.LevelInfo, "request handled",
		"method", "GET", "path", "/api/users", "status", 200,
		"elapsed", 750*time.Millisecond)
	if err != nil {
		panic(err)
	}
	fmt.Println(line)

	line2, err := logfmt.Encode(logfmt.LevelError, "upstream call failed",
		"service", "billing", "err", errors.New("connection refused"))
	if err != nil {
		panic(err)
	}
	fmt.Println(line2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=INFO msg="request handled" method="GET" path="/api/users" status=200 elapsed=750ms
level=ERROR msg="upstream call failed" service="billing" err="connection refused"
```

### Tests

The tests pin the contract three ways. Golden-line assertions fix the exact output
for simple fields, so a formatting regression is caught precisely. Substring checks
prove the quoting/rendering rules (`name="Alice Smith"`, `err="kaboom"`,
`elapsed=750ms`) without over-specifying the whole line. Error-path tests cover
every boundary. Finally `TestEncodeProducesLineFormat` splits the line and asserts
every token past the first two is a well-formed `key=value` pair — the parseability
contract the whole encoder exists to uphold.

Create `logfmt_test.go`:

```go
package logfmt

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEncodeFormatsSimpleFields(t *testing.T) {
	t.Parallel()

	got, err := Encode(LevelInfo, "user logged in", "user", "alice", "ip", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	want := `level=INFO msg="user logged in" user="alice" ip="10.0.0.1"`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEncodeQuotesStrings(t *testing.T) {
	t.Parallel()

	got, err := Encode(LevelInfo, "hello", "name", "Alice Smith")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `name="Alice Smith"`) {
		t.Fatalf("got %q", got)
	}
}

func TestEncodeFormatsErrors(t *testing.T) {
	t.Parallel()

	got, err := Encode(LevelError, "request failed", "err", errors.New("kaboom"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `err="kaboom"`) {
		t.Fatalf("got %q", got)
	}
}

func TestEncodeFormatsDurations(t *testing.T) {
	t.Parallel()

	got, err := Encode(LevelInfo, "timing", "elapsed", 750*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "elapsed=750ms") {
		t.Fatalf("got %q", got)
	}
}

func TestEncodeRejectsBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		level   Level
		msg     string
		kv      []any
		wantSub string
	}{
		{"empty level", "", "msg", nil, "empty level"},
		{"empty message", LevelInfo, "", nil, "empty message"},
		{"odd kv", LevelInfo, "msg", []any{"only-key"}, "odd number"},
		{"non-string key", LevelInfo, "msg", []any{42, "value"}, "want string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Encode(tt.level, tt.msg, tt.kv...)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantSub)
			}
		})
	}
}

// TestEncodeProducesLineFormat pins the parseability contract: every token past
// the leading level= and msg= is a well-formed key=value pair.
func TestEncodeProducesLineFormat(t *testing.T) {
	t.Parallel()

	line, err := Encode(LevelInfo, "ok", "user", "alice", "status", 200, "elapsed", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// msg is quoted so it never contains a bare space; splitting on spaces is
	// safe for this input.
	tokens := strings.Split(line, " ")
	if len(tokens) < 2 {
		t.Fatalf("too few tokens: %q", line)
	}
	for i, tok := range tokens {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			t.Fatalf("token %d %q is not key=value", i, tok)
		}
		if k == "" || v == "" {
			t.Fatalf("token %d %q has empty key or value", i, tok)
		}
	}
	if !strings.HasPrefix(line, "level=INFO ") {
		t.Fatalf("line does not start with level=INFO: %q", line)
	}
}

func Example() {
	line, _ := Encode(LevelWarn, "disk high", "usage", "92%")
	fmt.Println(line)
	// Output: level=WARN msg="disk high" usage="92%"
}
```

## Review

The encoder is correct when its output is always a parseable logfmt line or an
explicit error — never a malformed line. The golden test pins the exact frame; the
substring tests pin the per-type rendering rules that make the line safe to parse;
the boundary tests pin that a bad call is rejected rather than silently emitted.
The subtle correctness point is that durations and any other `Stringer` render
correctly *for free* through the `%v` fall-through in `formatValue` — you did not
special-case `time.Duration`, the type did it itself. Do not add manual quoting;
`%q` is doing the escaping. Run `go test -race` to confirm the encoder is safe to
call from concurrent handlers (it shares no state, so it is).

## Resources

- [`fmt` package](https://pkg.go.dev/fmt) — verbs `%v`, `%q`, `%s`, `%T` and the Stringer/error dispatch.
- [`strings.Builder`](https://pkg.go.dev/strings#Builder) — the allocation-light way to assemble a line.
- [logfmt](https://brandur.org/logfmt) — the key=value line format and why quoting matters.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-stringer-money-type.md](02-stringer-money-type.md)
