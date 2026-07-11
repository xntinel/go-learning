# Exercise 25: Structured Logging Format Strings and Log Levels Validated and Precompiled at init

**Nivel: Intermedio** — validacion rapida (un test corto).

A logging package that re-parses its line format on every call, or that
checks a level name against a slice with a loop every time, pays a cost on
every single log line a program ever emits. This exercise validates the set
of allowed log levels and precompiles a log line template into literal and
field segments once, at package initialization, so that formatting a line at
runtime is a straight walk over a precompiled slice — no parsing, no level
lookup loop, no per-call allocation beyond the output string itself.

## What you'll build

```text
logformat/                 independent module: example.com/logformat
  go.mod                    module example.com/logformat
  logformat.go               level validation + template parser + Render
  cmd/
    demo/
      main.go                 renders a line, then shows both error paths
  logformat_test.go            level validation table + template parser table + Render tests
```

Files: `logformat.go`, `cmd/demo/main.go`, `logformat_test.go`.
Implement: `buildLevelSeverity([]string) (map[string]int, error)` validating level names are non-empty and unique; `parseTemplate(string) ([]segment, error)` splitting a template into literal/`{field}` segments and rejecting unbalanced braces, empty names, and duplicate fields; `Render(level, msg string, fields map[string]string) (string, error)` walking the precompiled segments.
Test: level validation rejects an empty list, a blank name, and a duplicate name; template parsing rejects unbalanced braces, an empty field name, and a duplicate field, and accepts a normal template; `Render` succeeds with all fields present, and fails for an unknown level and for a missing field.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logformat/cmd/demo
cd ~/go-exercises/logformat
go mod init example.com/logformat
go mod edit -go=1.24
```

### Why validate and precompile once, at init

A structured logger typically has two small, static pieces of configuration:
the set of valid level names (`DEBUG`, `INFO`, `WARN`, `ERROR`), and a line
format template describing where the level, the message, and a handful of
named fields go in the final string. Both are known at compile time and
never change while the program runs. Re-validating the level list on every
log call, or re-scanning the template string for `{`/`}` pairs every time a
line is rendered, is wasted work repeated on the hottest path a logging
package has — one call per log line, potentially thousands per second.

The fix is the same shape used elsewhere in this chapter for regexps and
lookup tables: do the parsing and validation exactly once, at package
initialization, and panic immediately if the static configuration itself is
broken. A malformed template or a duplicate level name is a programming
mistake baked into the binary, not a runtime condition to recover from — it
should fail the instant the process starts, not intermittently under load.
The parsing and validation logic is deliberately extracted into
`buildLevelSeverity` and `parseTemplate`, two plain functions that return an
error instead of panicking directly; `init()` calls them and panics only at
the top, which is what lets tests exercise every malformed-input path
directly without needing to fork a process to observe an init-time panic.

`Render` itself does no parsing at all: it walks the already-computed
`compiled` slice of segments, writing each literal run verbatim and looking
up each field reference — `level`, `msg`, or an arbitrary caller-supplied
field — in the precompiled order. A field referenced by the template but
missing from the caller's map is a genuine runtime error worth reporting,
not a silently blank substitution that would produce a malformed log line
downstream.

Create `logformat.go`:

```go
// logformat.go
// Package logformat validates a set of structured logging levels and
// precompiles a log line format string into literal/field segments at
// package initialization, so that formatting a line at runtime never
// re-parses the template or re-validates level names.
package logformat

import (
	"fmt"
	"strings"
)

// levelOrder lists valid level names from least to most severe. Declared
// here as the single source of truth; levelSeverity is derived from it.
var levelOrder = []string{"DEBUG", "INFO", "WARN", "ERROR"}

// levelSeverity maps a level name to its severity rank (0 = least severe).
// Computed once at init from levelOrder; see buildLevelSeverity.
var levelSeverity map[string]int

// segment is one piece of a precompiled log line template: either a literal
// run of text or a reference to a named field to substitute at format time.
type segment struct {
	literal string
	field   string
	isField bool
}

// template is the log line format. "{level}" and "{msg}" are always
// available; any other {name} must be supplied via the fields map passed to
// Render.
const template = "[{level}] {msg} request_id={request_id} tenant={tenant}"

// compiled holds the precompiled segments for template, built once at init.
var compiled []segment

func init() {
	sev, err := buildLevelSeverity(levelOrder)
	if err != nil {
		panic("logformat: " + err.Error())
	}
	levelSeverity = sev

	segs, err := parseTemplate(template)
	if err != nil {
		panic("logformat: " + err.Error())
	}
	compiled = segs
}

// buildLevelSeverity validates levelOrder (non-empty, no blank or duplicate
// names) and assigns each name its position as severity rank. It is
// extracted from init so tests can exercise the validation directly.
func buildLevelSeverity(levels []string) (map[string]int, error) {
	if len(levels) == 0 {
		return nil, fmt.Errorf("level list is empty")
	}
	sev := make(map[string]int, len(levels))
	for i, name := range levels {
		if name == "" {
			return nil, fmt.Errorf("level at position %d is empty", i)
		}
		if _, dup := sev[name]; dup {
			return nil, fmt.Errorf("duplicate level name %q", name)
		}
		sev[name] = i
	}
	return sev, nil
}

// parseTemplate splits s into literal and {field} segments. It is extracted
// from init so tests can exercise malformed templates directly, without
// needing to fork a process to observe an init panic.
func parseTemplate(s string) ([]segment, error) {
	var segs []segment
	seenFields := make(map[string]struct{})

	var literal strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '}' {
			return nil, fmt.Errorf("unbalanced %q at byte %d", "}", i)
		}
		if c != '{' {
			literal.WriteByte(c)
			i++
			continue
		}
		// c == '{': flush any pending literal, then read the field name.
		if literal.Len() > 0 {
			segs = append(segs, segment{literal: literal.String()})
			literal.Reset()
		}
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return nil, fmt.Errorf("unbalanced %q starting at byte %d", "{", i)
		}
		name := s[i+1 : i+end]
		if name == "" {
			return nil, fmt.Errorf("empty field name at byte %d", i)
		}
		if _, dup := seenFields[name]; dup {
			return nil, fmt.Errorf("duplicate field %q in template", name)
		}
		seenFields[name] = struct{}{}
		segs = append(segs, segment{field: name, isField: true})
		i += end + 1
	}
	if literal.Len() > 0 {
		segs = append(segs, segment{literal: literal.String()})
	}
	return segs, nil
}

// Render formats one log line using the precompiled segments. level must be
// one of levelOrder; every non-level, non-msg field referenced by the
// template must be present in fields, or Render returns an error rather than
// silently dropping data.
func Render(level, msg string, fields map[string]string) (string, error) {
	if _, ok := levelSeverity[level]; !ok {
		return "", fmt.Errorf("logformat: unknown level %q", level)
	}
	var b strings.Builder
	for _, seg := range compiled {
		if !seg.isField {
			b.WriteString(seg.literal)
			continue
		}
		switch seg.field {
		case "level":
			b.WriteString(level)
		case "msg":
			b.WriteString(msg)
		default:
			v, ok := fields[seg.field]
			if !ok {
				return "", fmt.Errorf("logformat: missing required field %q", seg.field)
			}
			b.WriteString(v)
		}
	}
	return b.String(), nil
}

// Severity returns level's numeric severity rank and whether level is known.
func Severity(level string) (int, bool) {
	s, ok := levelSeverity[level]
	return s, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/logformat"
)

func main() {
	line, err := logformat.Render("INFO", "user signed in", map[string]string{
		"request_id": "req-42",
		"tenant":     "acme",
	})
	if err != nil {
		fmt.Println("error:", err)
	} else {
		fmt.Println(line)
	}

	sev, ok := logformat.Severity("WARN")
	fmt.Println("WARN severity:", sev, ok)

	_, err = logformat.Render("TRACE", "unreachable", nil)
	fmt.Println("unknown level error:", err)

	_, err = logformat.Render("INFO", "missing tenant", map[string]string{
		"request_id": "req-43",
	})
	fmt.Println("missing field error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[INFO] user signed in request_id=req-42 tenant=acme
WARN severity: 2 true
unknown level error: logformat: unknown level "TRACE"
missing field error: logformat: missing required field "tenant"
```

### Tests

Create `logformat_test.go`:

```go
// logformat_test.go
package logformat

import (
	"strings"
	"testing"
)

func TestBuildLevelSeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		levels  []string
		wantErr string
	}{
		{"ordered levels ok", []string{"DEBUG", "INFO", "WARN", "ERROR"}, ""},
		{"empty list", nil, "empty"},
		{"blank name", []string{"DEBUG", ""}, "empty"},
		{"duplicate name", []string{"DEBUG", "DEBUG"}, "duplicate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sev, err := buildLevelSeverity(tc.levels)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if sev["INFO"] != 1 {
					t.Fatalf("INFO severity = %d, want 1", sev["INFO"])
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		tmpl     string
		wantErr  string
		wantSegs int
	}{
		{"literal and fields", "[{level}] {msg}", "", 4},
		{"unbalanced open", "[{level", "unbalanced", 0},
		{"unbalanced close", "level}]", "unbalanced", 0},
		{"empty field name", "[{}]", "empty field name", 0},
		{"duplicate field", "{msg} {msg}", "duplicate field", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			segs, err := parseTemplate(tc.tmpl)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(segs) != tc.wantSegs {
					t.Fatalf("len(segs) = %d, want %d", len(segs), tc.wantSegs)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRender(t *testing.T) {
	t.Parallel()

	got, err := Render("INFO", "hello", map[string]string{"request_id": "r1", "tenant": "acme"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "[INFO] hello request_id=r1 tenant=acme"
	if got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}

	if _, err := Render("TRACE", "hello", nil); err == nil {
		t.Fatal("expected error for unknown level")
	}

	if _, err := Render("INFO", "hello", map[string]string{"request_id": "r1"}); err == nil {
		t.Fatal("expected error for missing field")
	}
}

func TestSeverityOrdering(t *testing.T) {
	t.Parallel()

	d, _ := Severity("DEBUG")
	i, _ := Severity("INFO")
	w, _ := Severity("WARN")
	e, _ := Severity("ERROR")
	if !(d < i && i < w && w < e) {
		t.Fatalf("severities not increasing: DEBUG=%d INFO=%d WARN=%d ERROR=%d", d, i, w, e)
	}
	if _, ok := Severity("NOPE"); ok {
		t.Fatal("Severity(\"NOPE\") ok = true, want false")
	}
}
```

## Review

`parseTemplate` and `buildLevelSeverity` are correct when every malformed
input is caught before `Render` ever runs: an unbalanced brace, an empty
field name, a duplicate field, an empty level list, a blank level name, and
a duplicate level name each produce a descriptive error rather than a
`compiled` slice or `levelSeverity` map that silently misbehaves later.
`Render` is correct when it never re-parses anything — it only walks
`compiled` — and when it distinguishes the two special fields (`level`,
`msg`) from arbitrary caller fields, failing loudly on a field the template
requires but the caller forgot to supply.

The mistake to avoid is doing any of this validation lazily, inside
`Render`, "just in case" — that reintroduces the exact per-call cost this
design eliminates, and it means a broken template only fails the first time
a log line happens to be emitted, possibly in production, instead of the
instant the binary starts.

## Resources

- [Go spec: Package initialization](https://go.dev/ref/spec#Package_initialization) — why `init()` is the right place for one-time validation and precompilation.
- [strings.Builder](https://pkg.go.dev/strings#Builder) — the allocation-efficient way `Render` assembles the output line.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-rate-limit-token-bucket-lazy-per-key.md](24-rate-limit-token-bucket-lazy-per-key.md) | Next: [26-graphql-schema-definition-validator.md](26-graphql-schema-definition-validator.md)
