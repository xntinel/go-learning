# Exercise 4: Parse a logfmt Line With Quoted Values Into Key-Value Pairs

Structured-log ingestion has to turn a logfmt line back into fields:
`level=info msg="user logged in" user=alice dur=3ms`. The trap is that the
obvious tool, `strings.Fields`, splits on whitespace and shreds the quoted
`msg` value into three tokens. This exercise builds the quote-aware scanner that
`Fields` cannot be, and pins the contrast with a test.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
logfmt/                         independent module: example.com/logfmt
  go.mod                        go 1.26
  logfmt.go                     ParseLogfmt (quote-aware) + naive Fields baseline
  logfmt_test.go                contrast test + round-trip property test
  cmd/
    demo/
      main.go                   runnable demo
```

Files: `logfmt.go`, `logfmt_test.go`, `cmd/demo/main.go`.
Implement: `ParseLogfmt(line string) (map[string]string, error)` and a
`naiveFields` helper that demonstrates why `strings.Fields` is wrong.
Test: the naive split corrupts a quoted value while the real parser does not;
bare key, value containing `=`, empty value, trailing space, unterminated quote
(error); a round-trip property test.
Verify: `go test -count=1 -race ./...`

### Why strings.Fields is the wrong baseline

`strings.Fields(line)` splits on every run of whitespace. For
`msg="user logged in"` it produces `msg="user`, `logged`, `in"` — the quoted
value is destroyed, and `Cut`-ting each of those on `=` yields garbage. There is
no flag to make `Fields` respect quotes; it fundamentally does not model them.
The lesson keeps `naiveFields` in the file precisely so a test can prove the
corruption, then contrasts it with the correct scanner.

The correct tokenizer is a single manual pass that tracks one bit of state:
whether the scanner is currently inside a double-quoted region. A space splits a
field only when the scanner is *not* inside quotes; inside quotes, spaces are
ordinary content. When the line ends while still inside a quote, that is an
unterminated-quote error, not a silently accepted token. Each resulting token is
then `Cut` on the first `=` into key and value; a token with no `=` is a bare key
(value `""`), and the value has one optional layer of surrounding quotes
stripped. Cutting on the *first* `=` is what lets a value like `q="a=b"` or a URL
survive.

Create `logfmt.go`:

```go
package logfmt

import (
	"fmt"
	"strings"
)

// ErrUnterminatedQuote means a value opened a double quote that never closed.
var ErrUnterminatedQuote = fmt.Errorf("logfmt: unterminated quote")

// ParseLogfmt parses a logfmt line into key-value pairs, honoring double-quoted
// values that contain spaces or '='. It does not use strings.Fields.
func ParseLogfmt(line string) (map[string]string, error) {
	tokens, err := scanTokens(line)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(tokens))
	for _, tok := range tokens {
		key, value, found := strings.Cut(tok, "=")
		if !found {
			out[key] = "" // bare key, flag-style
			continue
		}
		out[key] = unquote(value)
	}
	return out, nil
}

// scanTokens splits on unquoted spaces, treating quoted regions as opaque.
func scanTokens(line string) ([]string, error) {
	var tokens []string
	var b strings.Builder
	inQuote := false
	hasContent := false
	flush := func() {
		if hasContent {
			tokens = append(tokens, b.String())
			b.Reset()
			hasContent = false
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			b.WriteByte(c)
			hasContent = true
		case c == ' ' && !inQuote:
			flush()
		default:
			b.WriteByte(c)
			hasContent = true
		}
	}
	if inQuote {
		return nil, ErrUnterminatedQuote
	}
	flush()
	return tokens, nil
}

// unquote strips one layer of surrounding double quotes if present.
func unquote(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s[1 : len(s)-1]
	}
	return s
}

// naiveFields is the WRONG approach kept for contrast: strings.Fields splits
// inside quoted values. Do not use it to parse logfmt.
func naiveFields(line string) []string {
	return strings.Fields(line)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/logfmt"
)

func main() {
	line := `level=info msg="user logged in" user=alice dur=3ms`
	fields, err := logfmt.ParseLogfmt(line)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%q\n", k, fields[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dur="3ms"
level="info"
msg="user logged in"
user="alice"
```

### Tests

Create `logfmt_test.go`:

```go
package logfmt

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"testing"
)

func TestNaiveFieldsCorruptsQuotedValue(t *testing.T) {
	t.Parallel()

	line := `msg="user logged in"`
	// The naive split shatters the quoted value into three tokens.
	if got := naiveFields(line); len(got) != 3 {
		t.Fatalf("naiveFields(%q) = %v (len %d), want 3 tokens", line, got, len(got))
	}
	// The correct parser keeps it as a single field.
	fields, err := ParseLogfmt(line)
	if err != nil {
		t.Fatalf("ParseLogfmt: %v", err)
	}
	if got := fields["msg"]; got != "user logged in" {
		t.Fatalf("msg = %q, want %q", got, "user logged in")
	}
}

func TestParseLogfmt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		want    map[string]string
		wantErr error
	}{
		{
			name: "typical line",
			line: `level=info msg="user logged in" user=alice dur=3ms`,
			want: map[string]string{"level": "info", "msg": "user logged in", "user": "alice", "dur": "3ms"},
		},
		{
			name: "value contains equals",
			line: `url="http://h/p?a=1&b=2" ok=true`,
			want: map[string]string{"url": "http://h/p?a=1&b=2", "ok": "true"},
		},
		{
			name: "bare key and empty value",
			line: `debug ready= n=1`,
			want: map[string]string{"debug": "", "ready": "", "n": "1"},
		},
		{
			name: "trailing space",
			line: `a=1 b=2   `,
			want: map[string]string{"a": "1", "b": "2"},
		},
		{name: "unterminated quote", line: `msg="oops`, wantErr: ErrUnterminatedQuote},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLogfmt(tc.line)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseLogfmt(%q) err = %v, want %v", tc.line, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLogfmt(%q) unexpected err: %v", tc.line, err)
			}
			if !maps.Equal(got, tc.want) {
				t.Fatalf("ParseLogfmt(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestParseLogfmtRoundTrip(t *testing.T) {
	t.Parallel()

	want := map[string]string{"level": "warn", "msg": "disk almost full", "pct": "92"}
	line := encode(want)
	got, err := ParseLogfmt(line)
	if err != nil {
		t.Fatalf("ParseLogfmt(%q): %v", line, err)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("round-trip = %v, want %v", got, want)
	}
}

// encode builds a logfmt line, quoting any value that contains a space.
func encode(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if strings.ContainsRune(v, ' ') {
			v = `"` + v + `"`
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " ")
}

func ExampleParseLogfmt() {
	fields, _ := ParseLogfmt(`level=info msg="ok now"`)
	fmt.Printf("%s|%s\n", fields["level"], fields["msg"])
	// Output: info|ok now
}
```

## Review

The parser is correct when a quoted value with spaces or `=` survives as one
field and an unterminated quote is an error. The point of the module is the
contrast test: `naiveFields` proves `strings.Fields` returns three tokens for
`msg="user logged in"`, which is exactly why you cannot use it here. The
round-trip test is the property version of the same claim: encode a map with a
spaced value, parse it back, and get the map. Real logfmt has more corners
(escaped quotes inside values, `=` in keys); this scanner covers the common shape
and shows where a hand-written tokenizer beats a library primitive. Confirm with
`go test -race`.

## Resources

- [logfmt overview](https://brandur.org/logfmt) — the key=value / quoted-value format.
- [strings.FieldsFunc](https://pkg.go.dev/strings#FieldsFunc) and [strings.Fields](https://pkg.go.dev/strings#Fields).
- [strings.Cut](https://pkg.go.dev/strings#Cut) — split each token on the first `=`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-content-type-header-parser.md](03-content-type-header-parser.md) | Next: [05-dotenv-config-loader.md](05-dotenv-config-loader.md)
