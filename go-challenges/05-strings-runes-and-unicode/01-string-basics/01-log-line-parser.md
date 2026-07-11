# Exercise 1: Parse and re-emit structured log lines with a round-trip contract

Every backend ingests its own logs at some point — a shipper tailing a file, a
sidecar re-emitting to a collector, a test harness asserting on output. This
module builds the parser and formatter for a structured log line and pins the
one property that matters most for a codec: `Parse` and `Format` are inverses.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
logparse/                   independent module: example.com/logparse
  go.mod                    go 1.26
  logparse.go               Entry, Level (+String), Parse, Format, sentinel errors
  cmd/
    demo/
      main.go               parse a line, print the fields, round-trip it back
  logparse_test.go          valid/reject table, round-trip property, Unicode safety
```

Files: `logparse.go`, `cmd/demo/main.go`, `logparse_test.go`.
Implement: `Parse(line string) (Entry, error)`, `Format(Entry) string`, a `Level`
enum with `String()`, and sentinel errors `ErrEmpty`, `ErrMalformed`, `ErrBadTime`.
Test: valid line, nanosecond timestamp, four rejection cases, a `Format`→`Parse`
round-trip property, and a Unicode-message pass-through.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logparse/cmd/demo
cd ~/go-exercises/logparse
go mod init example.com/logparse
```

## Why SplitN with n=3, and why time.Parse over hand-rolling

A structured log line here is three fields separated by spaces: an RFC 3339
timestamp, a level token, and a free-form message. The message itself contains
spaces, so a plain split would shred it. `strings.SplitN(line, " ", 3)` caps the
result at three pieces, which means the third piece keeps every internal space —
the whole reason `n` is capped rather than unbounded. This is the canonical use
of `SplitN`: split off a fixed number of leading fields and leave the remainder
intact.

The timestamp is parsed with `time.Parse(time.RFC3339Nano, ...)`. `RFC3339Nano`
accepts both a whole-second stamp (`2024-01-15T10:30:00Z`) and a fractional one
(`...00.123456789Z`), so one layout covers both shapes. Never hand-roll a
timestamp parser; the standard layout is correct about time zones, leap-second
text, and fractional digits in ways a split-on-colon never is.

Errors are package-level sentinels wrapped with `%w` so callers can branch with
`errors.Is`. `ErrEmpty`, `ErrMalformed`, and `ErrBadTime` distinguish the three
structural failures; an unknown level carries the offending token in its message
so an operator can see what was rejected. The message field is stored as the raw
string with no transformation — that is what lets non-ASCII content survive the
round trip byte-for-byte.

Create `logparse.go`:

```go
package logparse

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors let callers branch with errors.Is.
var (
	ErrEmpty     = errors.New("empty line")
	ErrMalformed = errors.New("malformed log line")
	ErrBadTime   = errors.New("bad timestamp")
	ErrBadLevel  = errors.New("unknown level")
)

// Level is a severity enum with a canonical string form.
type Level uint8

const (
	LevelUnknown Level = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
)

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
		return "UNKNOWN"
	}
}

// Entry is one parsed log record.
type Entry struct {
	Time    time.Time
	Level   Level
	Message string
}

// Parse decodes one structured log line: "<RFC3339 time> <LEVEL> <message>".
// The message may contain spaces; it is kept intact.
func Parse(line string) (Entry, error) {
	if line == "" {
		return Entry{}, ErrEmpty
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 3 {
		return Entry{}, ErrMalformed
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %q", ErrBadTime, parts[0])
	}
	level, err := parseLevel(parts[1])
	if err != nil {
		return Entry{}, err
	}
	return Entry{Time: t, Level: level, Message: parts[2]}, nil
}

func parseLevel(raw string) (Level, error) {
	switch raw {
	case "DEBUG":
		return LevelDebug, nil
	case "INFO":
		return LevelInfo, nil
	case "WARN":
		return LevelWarn, nil
	case "ERROR":
		return LevelError, nil
	default:
		return LevelUnknown, fmt.Errorf("%w: %q", ErrBadLevel, raw)
	}
}

// Format is the inverse of Parse: it renders an Entry back to a log line.
func Format(e Entry) string {
	return e.Time.Format(time.RFC3339Nano) + " " + e.Level.String() + " " + e.Message
}
```

## The runnable demo

The demo parses a line, prints the decoded fields, then formats the entry back to
prove the round trip visually.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logparse"
)

func main() {
	const line = "2024-01-15T10:30:00Z WARN disk space low on /var"

	e, err := logparse.Parse(line)
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}
	fmt.Printf("time=%s level=%s message=%q\n",
		e.Time.Format("15:04:05"), e.Level, e.Message)
	fmt.Println("reformatted:", logparse.Format(e))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
time=10:30:00 level=WARN message="disk space low on /var"
reformatted: 2024-01-15T10:30:00Z WARN disk space low on /var
```

## Tests

The rejection table asserts each sentinel with `errors.Is`. The round-trip test
is the core property: formatting an `Entry` and parsing the result must reproduce
the original fields. `TestParseUnicodeMessage` proves the message is treated as
opaque bytes — a `café résumé` message survives parse and format unchanged, which
is the guarantee that stops the parser from corrupting non-ASCII input.

Create `logparse_test.go`:

```go
package logparse

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseAcceptsValidLine(t *testing.T) {
	t.Parallel()

	got, err := Parse("2024-01-15T10:30:00Z INFO hello world")
	if err != nil {
		t.Fatal(err)
	}
	want := Entry{
		Time:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Level:   LevelInfo,
		Message: "hello world",
	}
	if !got.Time.Equal(want.Time) {
		t.Fatalf("Time = %v, want %v", got.Time, want.Time)
	}
	if got.Level != want.Level {
		t.Fatalf("Level = %v, want %v", got.Level, want.Level)
	}
	if got.Message != want.Message {
		t.Fatalf("Message = %q, want %q", got.Message, want.Message)
	}
}

func TestParseAcceptsNanosecondTimestamp(t *testing.T) {
	t.Parallel()

	got, err := Parse("2024-01-15T10:30:00.123456789Z DEBUG nanosecond test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Level != LevelDebug {
		t.Fatalf("Level = %v, want LevelDebug", got.Level)
	}
	if got.Time.Nanosecond() != 123456789 {
		t.Fatalf("Nanosecond = %d, want 123456789", got.Time.Nanosecond())
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		wantErr error
	}{
		{"empty", "", ErrEmpty},
		{"no message", "2024-01-15T10:30:00Z INFO", ErrMalformed},
		{"bad time", "not-a-time INFO hello", ErrBadTime},
		{"unknown level", "2024-01-15T10:30:00Z FOO hello", ErrBadLevel},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tc.line)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Parse(%q) err = %v, want errors.Is %v", tc.line, err, tc.wantErr)
			}
		})
	}
}

func TestFormatRoundTrips(t *testing.T) {
	t.Parallel()

	cases := []Entry{
		{time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC), LevelWarn, "disk space low"},
		{time.Date(2024, 1, 15, 10, 30, 0, 123456789, time.UTC), LevelError, "trailing  double  spaces"},
		{time.Date(2000, 12, 31, 23, 59, 59, 0, time.UTC), LevelDebug, "edge of year"},
	}
	for _, original := range cases {
		line := Format(original)
		got, err := Parse(line)
		if err != nil {
			t.Fatalf("Parse(%q): %v", line, err)
		}
		if !got.Time.Equal(original.Time) || got.Level != original.Level || got.Message != original.Message {
			t.Fatalf("round trip of %q gave %+v", line, got)
		}
	}
}

func TestParseUnicodeMessage(t *testing.T) {
	t.Parallel()

	const msg = "café résumé 日本語 done"
	got, err := Parse("2024-01-15T10:30:00Z INFO " + msg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Message != msg {
		t.Fatalf("Message = %q, want %q (non-ASCII bytes corrupted)", got.Message, msg)
	}
	// Round-tripping must not touch the bytes either.
	if !strings.Contains(Format(got), msg) {
		t.Fatalf("Format dropped the Unicode message")
	}
}

func ExampleLevel_String() {
	e, _ := Parse("2024-01-15T10:30:00Z ERROR boom")
	fmt.Println(e.Level)
	// Output: ERROR
}
```

## Review

The parser is correct when `Format` and `Parse` are exact inverses on every field
and the message is never rewritten. The round-trip test is the strongest single
guarantee: if it holds for whole-second and nanosecond timestamps, for messages
with internal double spaces, and for non-ASCII content, the codec is sound. The
`n=3` cap on `SplitN` is load-bearing — drop it and any message with a space
becomes `ErrMalformed`. Assert errors with `errors.Is` against the sentinels
rather than string-matching, and keep the message field an untouched raw string
so Unicode survives byte-for-byte. Run `go test -race` to confirm the value-type
`Entry` is safe under the parallel subtests.

## Resources

- [Go Specification: String types](https://go.dev/ref/spec#String_types)
- [strings.SplitN (pkg.go.dev)](https://pkg.go.dev/strings#SplitN)
- [time.Parse and RFC3339Nano (pkg.go.dev)](https://pkg.go.dev/time#Parse)
- [The Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-header-and-cut.md](02-header-and-cut.md)
