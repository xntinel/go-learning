# Exercise 2: Structured Log Line Parser with Named Groups

Application logs are semi-structured: a timestamp, a level, an optional trace id,
and a free-text message, in a house format that is not quite JSON and not quite
anything a stdlib parser handles. This module turns such a line into a typed
struct with a single package-level named-capture regex, resolving every field by
name via `SubexpIndex` instead of fragile numeric indices, and returning a
sentinel error rather than panicking on a malformed line.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
logparse/                   independent module: example.com/logparse
  go.mod                    go 1.26
  logparse.go               type Entry; lineRe with (?P<...>); Parse via SubexpIndex; ErrMalformed
  cmd/
    demo/
      main.go               runnable demo: parse three log lines
  logparse_test.go          table-driven: well-formed, optional trace, malformed sentinel, index guards
```

- Files: `logparse.go`, `cmd/demo/main.go`, `logparse_test.go`.
- Implement: `Parse(line string) (Entry, error)` with a package-level `(?P<ts>...)(?P<level>...)(?P<trace>...)?(?P<msg>...)` regex, resolving fields with `SubexpIndex`, parsing the timestamp with `time.Parse`.
- Test: a well-formed line maps every field; a line with no trace id yields an empty `TraceID`; a malformed line returns `ErrMalformed` (no panic); `SubexpIndex` guards against a renamed group; a non-matching line never indexes past `len(match)`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/07-regular-expressions/02-structured-log-line-parser/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/07-regular-expressions/02-structured-log-line-parser
```

### Why named groups and SubexpIndex, not m[1]

The pattern has four fields and one of them is optional, so positional indexing
is a maintenance hazard: the day someone adds a `host=` field in the middle, every
`m[3]` downstream reads the wrong column and the bug is silent — the code still
compiles and still returns a struct, just a wrong one. Naming each group with
`(?P<name>...)` and resolving it with `re.SubexpIndex("name")` makes the mapping
explicit and position-independent. It also lets you *guard*: `SubexpIndex` returns
-1 for a name that does not exist, so a test asserting `SubexpIndex("level") >= 0`
fails loudly if someone renames the group, instead of the parser quietly reading
garbage.

The regex is:

```
^(?P<ts>\S+)\s+(?P<level>[A-Z]+)\s+(?:trace_id=(?P<trace>\S+)\s+)?(?P<msg>.*)$
```

`ts` is a non-space run (an RFC 3339 timestamp), `level` an uppercase word, the
trace id is an *optional* non-capturing group `(?:...)?` wrapping a named `trace`,
and `msg` is the rest of the line. Because the trace group sits inside an optional
group, a line without it still matches and `trace` captures the empty string —
which is exactly the "field missing an optional group yields an empty field"
behavior the tests pin.

The critical safety property: `FindStringSubmatch` returns nil on no match. `Parse`
checks `if m == nil` and returns `ErrMalformed` *before* indexing, so a garbage
line is a handled error, never an index-out-of-range panic. This is the
number-one regex panic in production Go, and the guard is one line.

Create `logparse.go`:

```go
package logparse

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrMalformed is returned for a line that does not match the log grammar or
// carries an unparseable timestamp.
var ErrMalformed = errors.New("malformed log line")

// lineRe is compiled once at package init and shared across goroutines.
var lineRe = regexp.MustCompile(
	`^(?P<ts>\S+)\s+(?P<level>[A-Z]+)\s+(?:trace_id=(?P<trace>\S+)\s+)?(?P<msg>.*)$`,
)

// Field indices resolved once by name, so inserting a group later does not shift
// them and a renamed group is caught at init.
var (
	idxTS    = lineRe.SubexpIndex("ts")
	idxLevel = lineRe.SubexpIndex("level")
	idxTrace = lineRe.SubexpIndex("trace")
	idxMsg   = lineRe.SubexpIndex("msg")
)

// Entry is a parsed log line.
type Entry struct {
	Time    time.Time
	Level   string
	TraceID string
	Message string
}

// Parse turns one semi-structured log line into an Entry. A line that does not
// match, or whose timestamp is unparseable, returns ErrMalformed wrapped with
// context.
func Parse(line string) (Entry, error) {
	m := lineRe.FindStringSubmatch(line)
	if m == nil {
		return Entry{}, fmt.Errorf("%w: %q", ErrMalformed, line)
	}
	ts, err := time.Parse(time.RFC3339, m[idxTS])
	if err != nil {
		return Entry{}, fmt.Errorf("%w: bad timestamp %q", ErrMalformed, m[idxTS])
	}
	return Entry{
		Time:    ts,
		Level:   m[idxLevel],
		TraceID: m[idxTrace],
		Message: m[idxMsg],
	}, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/logparse"
)

func main() {
	lines := []string{
		"2026-07-02T10:15:30Z INFO trace_id=abc123 user logged in",
		"2026-07-02T10:15:31Z WARN disk almost full",
	}
	for _, line := range lines {
		e, err := logparse.Parse(line)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("level=%s trace=%q msg=%q\n", e.Level, e.TraceID, e.Message)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=INFO trace="abc123" msg="user logged in"
level=WARN trace="" msg="disk almost full"
```

### Tests

Create `logparse_test.go`:

```go
package logparse

import (
	"errors"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		line      string
		wantLevel string
		wantTrace string
		wantMsg   string
	}{
		{
			name:      "with trace id",
			line:      "2026-07-02T10:15:30Z INFO trace_id=abc123 user logged in",
			wantLevel: "INFO",
			wantTrace: "abc123",
			wantMsg:   "user logged in",
		},
		{
			name:      "optional trace missing",
			line:      "2026-07-02T10:15:31Z WARN disk almost full",
			wantLevel: "WARN",
			wantTrace: "",
			wantMsg:   "disk almost full",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, err := Parse(tc.line)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.line, err)
			}
			if e.Level != tc.wantLevel {
				t.Errorf("Level = %q, want %q", e.Level, tc.wantLevel)
			}
			if e.TraceID != tc.wantTrace {
				t.Errorf("TraceID = %q, want %q", e.TraceID, tc.wantTrace)
			}
			if e.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", e.Message, tc.wantMsg)
			}
		})
	}
}

func TestParseTimestamp(t *testing.T) {
	t.Parallel()
	e, err := Parse("2026-07-02T10:15:30Z INFO ok")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 2, 10, 15, 30, 0, time.UTC)
	if !e.Time.Equal(want) {
		t.Fatalf("Time = %v, want %v", e.Time, want)
	}
}

func TestParseMalformed(t *testing.T) {
	t.Parallel()
	// No level, no message structure at all: must be a handled error, not a panic.
	for _, line := range []string{"garbage", "", "2026-07-02T10:15:30Z lowercase msg"} {
		if _, err := Parse(line); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Parse(%q) err = %v, want ErrMalformed", line, err)
		}
	}
}

func TestParseBadTimestamp(t *testing.T) {
	t.Parallel()
	if _, err := Parse("not-a-time INFO hello"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestNamedGroupsResolve(t *testing.T) {
	t.Parallel()
	// If any group is renamed, SubexpIndex returns -1 and this guard fails loudly.
	for _, name := range []string{"ts", "level", "trace", "msg"} {
		if lineRe.SubexpIndex(name) < 0 {
			t.Fatalf("SubexpIndex(%q) < 0: group renamed or removed", name)
		}
	}
}
```

## Review

The parser is correct when field resolution is by name, not position: each field
index comes from `SubexpIndex` at init, so inserting a group later shifts nothing,
and `TestNamedGroupsResolve` turns a renamed group into a failing test instead of
a silent misread. The optional trace group demonstrates the intended empty-field
behavior — a line without `trace_id=` still matches and `TraceID` is `""`, not an
error. The load-bearing safety line is `if m == nil`: it converts a non-matching
line into `ErrMalformed` before any `m[i]`, which is the whole difference between
a logged parse failure and a panic that takes down the ingestion goroutine. Run
`go test -race` to confirm the shared package-level regex is concurrency-safe.

## Resources

- [`regexp` package](https://pkg.go.dev/regexp) — `FindStringSubmatch`, `SubexpIndex`, `SubexpNames`.
- [RE2 syntax: named captures](https://github.com/google/re2/wiki/Syntax) — the `(?P<name>...)` form and non-capturing groups.
- [`time.Parse`](https://pkg.go.dev/time#Parse) — parsing the RFC 3339 timestamp instead of a regex for the date.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-url-parts-extractor.md](01-url-parts-extractor.md) | Next: [03-dynamic-pattern-rule-engine.md](03-dynamic-pattern-rule-engine.md)
