# Exercise 7: encoding.TextMarshaler/TextUnmarshaler for a Config LogLevel

A config value like a log level arrives from many places — a JSON file, a YAML
file, an environment variable, a command-line flag — and you do not want four
decoders. `encoding.TextMarshaler`/`TextUnmarshaler` gives you one: `encoding/json`
and most config libraries fall back to the text hooks for scalar strings, and the
same methods back `flag.Value`. One `UnmarshalText` serves them all.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
loglevel/                   independent module: example.com/loglevel
  go.mod
  loglevel.go               LogLevel with MarshalText/UnmarshalText/AppendText and flag.Value
  cmd/
    demo/
      main.go               marshals a config, decodes a case-insensitive level
  loglevel_test.go          case-insensitive parse, canonical emit, JSON via text hook, flag.Value
```

- Files: `loglevel.go`, `cmd/demo/main.go`, `loglevel_test.go`.
- Implement: `MarshalText`/`UnmarshalText` on a `LogLevel` (case-insensitive parse, canonical name emit, unknown errors), the Go 1.24 `AppendText`, and `flag.Value` (`String`/`Set`).
- Test: `UnmarshalText` parses each level name case-insensitively and errors on unknown; `MarshalText` emits the canonical name; a struct with a `LogLevel` field decodes from JSON via the text hook with no `UnmarshalJSON`; it satisfies `flag.Value`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/04-common-standard-library-interfaces/07-text-marshaler-config/cmd/demo
cd go-solutions/08-interfaces/04-common-standard-library-interfaces/07-text-marshaler-config
```

### One text hook, four decoders

The reason `MarshalText`/`UnmarshalText` is higher-leverage than `MarshalJSON` is
reach. `encoding/json` uses this rule: when a type has no `UnmarshalJSON` but the
JSON token is a string and the type implements `encoding.TextUnmarshaler`, it
calls `UnmarshalText` with the unquoted string. YAML, TOML, and env-config
libraries do the same, and `flag.Value.Set(string)` is a natural front for
`UnmarshalText([]byte(s))`. So a single `UnmarshalText` decodes a `LogLevel` from
`{"level":"warn"}`, from `level: warn` in YAML, from `LEVEL=warn` in the
environment, and from `-level warn` on the command line — without a bespoke
decoder for each.

The parse is deliberately lenient on input and strict on output:
`UnmarshalText` lowercases and trims so `WARN`, `Warn`, and ` warn ` all decode,
while `MarshalText` emits exactly one canonical spelling. Unknown text is an error,
so a typo in a config file fails loudly at load time instead of silently
defaulting. Go 1.24 added `encoding.TextAppender` (`AppendText([]byte) ([]byte,
error)`), which appends the representation to an existing buffer to avoid
allocating a fresh slice per call; `encoding/json` and others prefer it when
present. `MarshalText` here is a thin wrapper over `AppendText`. The same type
also satisfies `flag.Value` through `String()` and `Set()`, so `flag.Var(&level,
"level", ...)` just works.

Create `loglevel.go`:

```go
package loglevel

import (
	"fmt"
	"strings"
)

// LogLevel is a config-facing severity that round-trips through JSON, YAML, env,
// and flags via a single text representation.
type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelNames = [...]string{
	LevelDebug: "debug",
	LevelInfo:  "info",
	LevelWarn:  "warn",
	LevelError: "error",
}

func (l LogLevel) valid() bool { return l >= LevelDebug && l <= LevelError }

// AppendText (Go 1.24 encoding.TextAppender) appends the canonical name to b
// without allocating a fresh slice.
func (l LogLevel) AppendText(b []byte) ([]byte, error) {
	if !l.valid() {
		return nil, fmt.Errorf("invalid log level %d", int(l))
	}
	return append(b, levelNames[l]...), nil
}

// MarshalText emits the canonical lowercase name.
func (l LogLevel) MarshalText() ([]byte, error) {
	return l.AppendText(nil)
}

// UnmarshalText parses a level name case-insensitively and errors on unknown.
func (l *LogLevel) UnmarshalText(text []byte) error {
	s := strings.ToLower(strings.TrimSpace(string(text)))
	for i, name := range levelNames {
		if name == s {
			*l = LogLevel(i)
			return nil
		}
	}
	return fmt.Errorf("unknown log level %q", text)
}

// String and Set make LogLevel a flag.Value.
func (l LogLevel) String() string {
	if !l.valid() {
		return "unknown"
	}
	return levelNames[l]
}

func (l *LogLevel) Set(s string) error { return l.UnmarshalText([]byte(s)) }
```

### The runnable demo

The demo marshals a config struct to JSON — the level comes out as its canonical
name via the text hook — then decodes an uppercase level string to show the
case-insensitive parse.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/loglevel"
)

type Config struct {
	Service string            `json:"service"`
	Level   loglevel.LogLevel `json:"level"`
}

func main() {
	cfg := Config{Service: "api", Level: loglevel.LevelWarn}
	b, _ := json.Marshal(cfg)
	fmt.Println(string(b))

	var back Config
	if err := json.Unmarshal([]byte(`{"service":"api","level":"ERROR"}`), &back); err != nil {
		fmt.Println("decode error:", err)
		return
	}
	fmt.Printf("level=%s\n", back.Level)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"service":"api","level":"warn"}
level=error
```

The struct has no `MarshalJSON`/`UnmarshalJSON` at all — `encoding/json` routes the
`level` string field straight through `MarshalText`/`UnmarshalText`, and `"ERROR"`
decodes to `error` because the parse is case-insensitive.

### Tests

`TestUnmarshalCaseInsensitive` is table-driven over mixed-case and padded names
plus an unknown that must error. `TestMarshalCanonical` asserts the canonical
lowercase emit. `TestJSONViaTextHook` decodes a struct with a `LogLevel` field
from JSON with no custom JSON method, proving the text fallback fires.
`TestFlagValue` wires the level into a `flag.FlagSet` and parses `-level info`.

Create `loglevel_test.go`:

```go
package loglevel

import (
	"encoding/json"
	"flag"
	"fmt"
	"testing"
)

func TestUnmarshalCaseInsensitive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want LogLevel
		ok   bool
	}{
		{"debug", LevelDebug, true},
		{"INFO", LevelInfo, true},
		{"Warn", LevelWarn, true},
		{"  error  ", LevelError, true},
		{"trace", 0, false},
	}
	for _, tc := range tests {
		var l LogLevel
		err := l.UnmarshalText([]byte(tc.in))
		if tc.ok {
			if err != nil {
				t.Errorf("UnmarshalText(%q) errored: %v", tc.in, err)
				continue
			}
			if l != tc.want {
				t.Errorf("UnmarshalText(%q) = %v, want %v", tc.in, l, tc.want)
			}
		} else if err == nil {
			t.Errorf("UnmarshalText(%q) should have errored", tc.in)
		}
	}
}

func TestMarshalCanonical(t *testing.T) {
	t.Parallel()

	b, err := LevelWarn.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "warn" {
		t.Fatalf("MarshalText = %q, want %q", b, "warn")
	}

	if _, err := LogLevel(99).MarshalText(); err == nil {
		t.Fatal("MarshalText of an invalid level must error")
	}
}

func TestJSONViaTextHook(t *testing.T) {
	t.Parallel()

	type cfg struct {
		Level LogLevel `json:"level"`
	}
	var c cfg
	if err := json.Unmarshal([]byte(`{"level":"info"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Level != LevelInfo {
		t.Fatalf("decoded level = %v, want LevelInfo", c.Level)
	}

	out, err := json.Marshal(cfg{Level: LevelError})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"level":"error"}` {
		t.Fatalf("marshaled = %s, want %s", out, `{"level":"error"}`)
	}
}

func TestFlagValue(t *testing.T) {
	t.Parallel()

	var level LogLevel
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	fs.Var(&level, "level", "log level")
	if err := fs.Parse([]string{"-level", "info"}); err != nil {
		t.Fatal(err)
	}
	if level != LevelInfo {
		t.Fatalf("flag level = %v, want LevelInfo", level)
	}

	// flag.Value is an interface; assert the type satisfies it.
	var _ flag.Value = &level
}

func ExampleLogLevel_MarshalText() {
	b, _ := LevelDebug.MarshalText()
	fmt.Println(string(b))
	// Output: debug
}
```

## Review

The level is correct when one `UnmarshalText` decodes it from JSON, from a flag,
and (by the same fallback) from YAML and env, parsing case-insensitively while
`MarshalText` emits a single canonical spelling, and when an unknown name is a
loud error rather than a silent zero. The leverage is real: no `UnmarshalJSON`,
no per-format decoder, one method. `AppendText` is the Go 1.24 allocation-free
form that `MarshalText` delegates to. The mistake to avoid is duplicating this as
a separate `UnmarshalJSON` — that shadows the text hook for JSON and drifts from
the other formats. Run `go test -race`.

## Resources

- [encoding.TextMarshaler / TextUnmarshaler](https://pkg.go.dev/encoding#TextMarshaler) — the text boundary interfaces.
- [encoding.TextAppender](https://pkg.go.dev/encoding#TextAppender) — the Go 1.24 allocation-free append form.
- [flag.Value](https://pkg.go.dev/flag#Value) — `String`/`Set`, the flag-parsing interface.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-json-marshaler-unmarshaler.md](06-json-marshaler-unmarshaler.md) | Next: [08-sort-interface-multikey.md](08-sort-interface-multikey.md)
