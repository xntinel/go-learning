# Exercise 9: Parsing a Fixed-Format Legacy Line with fmt.Sscanf

Ingestion is the inverse of formatting, and far more fragile. This exercise builds
an adapter that parses a fixed-format legacy access line with `fmt.Sscanf` into
typed fields, checks the returned count and error honestly, and then shows exactly
where `Sscanf` mangles a message field that `strings.Cut` handles correctly.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
legacyparse/               independent module: example.com/legacyparse
  go.mod                   go 1.24
  legacyparse.go           type Access; ParseAccess (Sscanf); Event; ParseEventSscanf/Cut
  cmd/
    demo/
      main.go              runnable demo: parse good/bad lines and the mangle contrast
  legacyparse_test.go      well-formed/short-count/mangle-vs-cut/round-trip tests
```

- Files: `legacyparse.go`, `cmd/demo/main.go`, `legacyparse_test.go`.
- Implement: `Access{IP; Status; Ms}` with `ParseAccess(line)` using `fmt.Sscanf` and checking `(n, err)`; `Event{Ts; Level; Msg}` with `ParseEventSscanf` (fragile) and `ParseEventCut` (robust); a sentinel `ErrMalformed` wrapped with `%w`.
- Test: well-formed lines yield typed fields; truncated/non-numeric input yields `ErrMalformed` (matched by `errors.Is`); a message with spaces is mangled by `Sscanf` but preserved by `Cut`; a format-then-reparse round trip.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/06-string-formatting-with-fmt/09-sscanf-parse-legacy-line/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/06-string-formatting-with-fmt/09-sscanf-parse-legacy-line
go mod edit -go=1.24
```

### Where Sscanf is acceptable, and where it lies

`fmt.Sscanf(line, format, &a, &b, ...)` returns `(n int, err error)`: `n` is how
many items it successfully assigned, and `err` describes why it stopped. For a
*genuinely fixed* format — a fixed number of whitespace-separated tokens, the last
of which contains no spaces — it is concise and fine, provided you check both
return values. An access line `10.0.0.1 200 42` parses cleanly with `%s %d %d`,
and a space in the format matches a *run* of whitespace, so extra spaces between
fields are tolerated.

The failure modes are where juniors get burned:

- **Truncated input.** Parsing `10.0.0.1 200` (missing the third field) with
  `%s %d %d` returns `n=2` and `err == io.EOF`, with the first two fields already
  assigned. If you ignore `n`/`err`, you silently accept a half-parsed record
  whose `Ms` is a zero value. `ParseAccess` treats `n != 3` (or any error) as a
  malformed line and returns `ErrMalformed` wrapped with `%w`, so callers match it
  with `errors.Is`.
- **Wrong type.** A non-numeric token where `%d` expects an integer
  (`10.0.0.1 OK 42`) returns `n=1` and `err` "expected integer" — again a short
  count you must check.
- **The greedy `%s` lie.** `%s` reads up to the next whitespace, so a field that
  is *supposed* to contain spaces is truncated to its first word, and `Sscanf`
  reports `n=3` with **no error** — the worst kind of failure, silent and
  plausible. Parsing `2026-07-02T10:00:00Z WARN disk almost full` with `%s %s %s`
  yields `Msg == "disk"`, dropping "almost full", and nothing tells you.

That last case is why `strings.Cut` is the right tool for a trailing free-text
field: `Cut(line, " ")` splits on the *first* separator and keeps the remainder
intact. `ParseEventCut` cuts the timestamp, then the level, and keeps everything
after as the message — `disk almost full`, whole. The rule of thumb: `Sscanf` for
a rigid, fully-tokenized numeric format with the count checked; `strings.Cut` /
`strings.Fields` + `strconv` whenever a field can contain spaces or be optional.

Create `legacyparse.go`:

```go
package legacyparse

import (
	"errors"
	"fmt"
	"strings"
)

// ErrMalformed is the sentinel a caller matches with errors.Is to route a bad
// line to a dead-letter queue instead of crashing the ingester.
var ErrMalformed = errors.New("malformed line")

// Access is one parsed access-log record.
type Access struct {
	IP     string
	Status int
	Ms     int
}

// String renders the record back to the legacy line format, enabling round trips.
func (a Access) String() string {
	return fmt.Sprintf("%s %d %d", a.IP, a.Status, a.Ms)
}

// ParseAccess parses "<ip> <status> <ms>" with Sscanf, checking BOTH the count
// and the error so a short parse is rejected rather than silently accepted.
func ParseAccess(line string) (Access, error) {
	var a Access
	n, err := fmt.Sscanf(line, "%s %d %d", &a.IP, &a.Status, &a.Ms)
	if err != nil {
		return Access{}, fmt.Errorf("%w: parsed %d/3 fields: %v", ErrMalformed, n, err)
	}
	if n != 3 {
		return Access{}, fmt.Errorf("%w: parsed %d/3 fields", ErrMalformed, n)
	}
	return a, nil
}

// Event is a timestamped log line whose message may contain spaces.
type Event struct {
	Ts    string
	Level string
	Msg   string
}

// ParseEventSscanf is the FRAGILE version: %s is greedy to the next space, so a
// multi-word message is truncated to its first word with no error reported.
// Present to contrast with ParseEventCut.
func ParseEventSscanf(line string) Event {
	var e Event
	fmt.Sscanf(line, "%s %s %s", &e.Ts, &e.Level, &e.Msg)
	return e
}

// ParseEventCut is the ROBUST version: it cuts on the first space twice and keeps
// the remainder as the message, so spaces in the message are preserved.
func ParseEventCut(line string) (Event, error) {
	ts, rest, ok := strings.Cut(line, " ")
	if !ok {
		return Event{}, fmt.Errorf("%w: missing level", ErrMalformed)
	}
	level, msg, ok := strings.Cut(rest, " ")
	if !ok {
		return Event{}, fmt.Errorf("%w: missing message", ErrMalformed)
	}
	return Event{Ts: ts, Level: level, Msg: msg}, nil
}
```

### The runnable demo

The demo parses a good access line, rejects a truncated one, and shows the mangle
contrast on an event line with a multi-word message.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/legacyparse"
)

func main() {
	a, err := legacyparse.ParseAccess("10.0.0.1 200 42")
	fmt.Printf("access: %+v err=%v\n", a, err)

	_, err = legacyparse.ParseAccess("10.0.0.1 200")
	fmt.Println("truncated is ErrMalformed:", errors.Is(err, legacyparse.ErrMalformed))

	line := "2026-07-02T10:00:00Z WARN disk almost full"
	bad := legacyparse.ParseEventSscanf(line)
	good, _ := legacyparse.ParseEventCut(line)
	fmt.Printf("sscanf msg=%q\n", bad.Msg)
	fmt.Printf("cut    msg=%q\n", good.Msg)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
access: 10.0.0.1 200 42 err=<nil>
truncated is ErrMalformed: true
sscanf msg="disk"
cut    msg="disk almost full"
```

### Tests

`TestParseAccessValid` pins the typed fields for a good line. `TestParseAccessRejects`
covers the short-count and wrong-type failure modes and asserts `errors.Is(err,
ErrMalformed)`. `TestSscanfMangleVsCut` is the heart of the lesson: it asserts
`Sscanf` truncates the message while `Cut` preserves it. `TestRoundTrip` formats a
record and re-parses it back to an equal value.

Create `legacyparse_test.go`:

```go
package legacyparse

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseAccessValid(t *testing.T) {
	t.Parallel()

	got, err := ParseAccess("10.0.0.1 200 42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Access{IP: "10.0.0.1", Status: 200, Ms: 42}
	if got != want {
		t.Fatalf("ParseAccess = %+v, want %+v", got, want)
	}
	// A run of whitespace between fields is tolerated by Sscanf.
	if got2, err := ParseAccess("10.0.0.1   200   42"); err != nil || got2 != want {
		t.Fatalf("extra-space line: got %+v err %v", got2, err)
	}
}

func TestParseAccessRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
	}{
		{"truncated", "10.0.0.1 200"},
		{"non-numeric status", "10.0.0.1 OK 42"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseAccess(tt.line)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("ParseAccess(%q) err = %v, want ErrMalformed", tt.line, err)
			}
		})
	}
}

func TestSscanfMangleVsCut(t *testing.T) {
	t.Parallel()

	line := "2026-07-02T10:00:00Z WARN disk almost full"

	fragile := ParseEventSscanf(line)
	if fragile.Msg != "disk" {
		t.Fatalf("Sscanf msg = %q, want the truncated \"disk\"", fragile.Msg)
	}

	robust, err := ParseEventCut(line)
	if err != nil {
		t.Fatalf("ParseEventCut error: %v", err)
	}
	if robust.Msg != "disk almost full" {
		t.Fatalf("Cut msg = %q, want the full \"disk almost full\"", robust.Msg)
	}
	if robust.Ts != "2026-07-02T10:00:00Z" || robust.Level != "WARN" {
		t.Fatalf("Cut fields wrong: %+v", robust)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	orig := Access{IP: "192.168.1.1", Status: 503, Ms: 1200}
	reparsed, err := ParseAccess(orig.String())
	if err != nil {
		t.Fatalf("round trip parse: %v", err)
	}
	if reparsed != orig {
		t.Fatalf("round trip = %+v, want %+v", reparsed, orig)
	}
}

func Example() {
	a, _ := ParseAccess("10.0.0.1 200 42")
	fmt.Printf("%s -> status %d\n", a.IP, a.Status)
	// Output: 10.0.0.1 -> status 200
}
```

## Review

The parser is correct when it never accepts a half-parsed record: `ParseAccess`
checks both `n` and `err` from `Sscanf` and returns `ErrMalformed` (wrapped with
`%w`, matchable by `errors.Is`) for truncated or wrong-type input, proven by
`TestParseAccessRejects`. The lesson's real payload is `TestSscanfMangleVsCut`: a
greedy `%s` truncates a multi-word message and reports success, so `Sscanf` is the
wrong tool for any field that can contain spaces — `strings.Cut` keeps the
remainder whole. Reach for `Sscanf` only on a rigid numeric format with the count
checked; reach for `Cut`/`Fields` + `strconv` the moment a field is free text or
optional. Run `go test -race`; the parser holds no shared state.

## Resources

- [`fmt.Sscanf`](https://pkg.go.dev/fmt#Sscanf) — the `(n, err)` contract and scanning verbs.
- [`strings.Cut`](https://pkg.go.dev/strings#Cut) — split on the first separator, keep the remainder.
- [`strconv.Atoi`](https://pkg.go.dev/strconv#Atoi) — the explicit integer parse for a `Fields`-based path.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-gostringer-debug-dump.md](10-gostringer-debug-dump.md)
