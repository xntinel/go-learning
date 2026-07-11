# Exercise 17: NDJSON Field-State Classifier

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A partner integration -- a payments gateway, a shipping-rate provider, any
third-party API a service depends on but does not control -- can change its
JSON shape without a version bump, and the change that breaks a typed client
hardest is rarely a renamed field. It is the moment a field that always sent
`[]` for "no results" starts sending `null` instead, or the moment a field
that was always present starts being omitted on some responses. A client
that decodes straight into `[]T` and checks `len(results) == 0` cannot see
that change happen: absent, `null`, and `[]` all decode to the same
zero-length slice, so the exact signal a contract monitor needs is the one a
normal client discards on the way in.

This module builds `fieldstate`, a small streaming tool that watches
newline-delimited JSON on stdin and reports, per line, which of four states
one named field is in: `absent` (the key does not exist), `null` (the key is
present with the literal value `null`), `empty` (the key is present with a
non-null value that is itself empty -- `[]`, `{}`, or `""`), or `value`
(anything else). It generalizes the tri-state classification `00-concepts.md`
introduces for a PATCH body, using the same `json.RawMessage` technique, but
here as a read-only observer of an arbitrary stream instead of a mutator
applying a patch -- exactly the shape a monitoring job or an integration test
harness would run against a captured or replayed feed.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
fieldstate/                    module example.com/fieldstate
  go.mod                       go 1.24
  fieldstate.go                package main — State, Classifier; NewClassifier, Classify
  fieldstate_test.go           package main — the four-state table, invalid-JSON cases,
                                the classifyLenNaive contrast, run() end to end
  main.go                      package main — -field flag, stdin streaming, exit codes
```

- Files: `fieldstate.go`, `fieldstate_test.go`, `main.go`.
- Implement: `NewClassifier(field string) (*Classifier, error)` rejecting an empty field name with `ErrEmptyField`; `(*Classifier).Classify(line []byte) (State, error)` decoding `line` as one JSON object and reporting `StateAbsent`, `StateNull`, `StateEmpty`, or `StateValue` for the configured field, returning `ErrInvalidJSON` for a malformed line.
- Tool: `fieldstate` reads newline-delimited JSON objects from stdin, one per line, and a required `-field` flag names the top-level key to classify. It prints one state word per non-blank line to stdout, skipping blank lines. Exit 0 on success; exit 2 for a missing `-field`, an unknown flag, or any line that is not valid JSON (the stream stops at the first one); exit 1 is reserved for a runtime failure this tool never produces.
- Test: all four states across arrays, objects, scalars, and strings; malformed and wrong-shaped JSON lines rejected with `ErrInvalidJSON`; `NewClassifier` rejecting an empty field; a `classifyLenNaive` contrast proving a length-based check collapses absent, null, and empty to the same answer; `run` end to end over a multi-line stream, blank-line skipping, a missing flag, and an invalid line mid-stream.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fieldstate
cd ~/go-exercises/fieldstate
go mod init example.com/fieldstate
go mod edit -go=1.24
```

### Why len()==0 cannot see the difference a monitor needs

Decoding a JSON object field straight into a concrete `[]T` collapses three
distinct wire facts into one Go fact. An absent key, a `null` value, and an
`[]` value are three different things a partner API can send, and a contract
monitor's entire job is telling them apart -- but `json.Unmarshal` into
`[]T` maps all three to the zero value of the field, `nil`, and `len(nil)`
is `0` just like `len([]T{})` is `0`:

```go
var obj map[string]any
json.Unmarshal(line, &obj)
results, _ := obj["results"].([]any)
if len(results) == 0 {
    // fires identically whether "results" was absent, null, or []
}
```

The fix is the same technique `00-concepts.md` uses for a PATCH body's
tri-state field: decode the object one level shallow, into
`map[string]json.RawMessage`, so each field's raw bytes are available before
any type-specific decoding happens. Presence is then a plain map lookup;
`null` is the raw bytes literally spelling `null`; anything else gets decoded
one more level just far enough to ask whether it is an empty array, object,
or string. Only that ordering -- check presence, check the null literal,
*then* decode -- can recover all four states, because decoding straight into
a concrete type throws the first two away before the emptiness question is
even asked.

Create `fieldstate.go`:

```go
// Package main implements fieldstate, a tool that watches a stream of
// newline-delimited JSON objects for one field's presence, nullness, and
// emptiness -- the tri-state (really four-state) distinction a naive
// len()==0 check on a decoded []T or map[string]T can never recover, because
// absent, null, and [] all decode to the same zero-length value.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

// State is the classification fieldstate assigns to one field on one line.
type State string

const (
	// StateAbsent means the object had no key by that name at all.
	StateAbsent State = "absent"
	// StateNull means the key was present with the literal JSON value null.
	StateNull State = "null"
	// StateEmpty means the key was present with a non-null value that is
	// itself empty: [], {}, or "".
	StateEmpty State = "empty"
	// StateValue means the key was present with a non-null, non-empty value.
	StateValue State = "value"
)

// ErrEmptyField is returned by NewClassifier when the field name is empty.
var ErrEmptyField = errors.New("fieldstate: field name must not be empty")

// ErrInvalidJSON is returned by Classify when a line is not a single valid
// JSON object. It wraps the underlying decode error for context.
var ErrInvalidJSON = errors.New("fieldstate: invalid JSON object")

// Classifier reports the state of one named top-level field across a stream
// of JSON objects, one object per Classify call.
//
// A Classifier holds only its configured field name after construction and
// is safe for concurrent use by multiple goroutines; nothing about Classify
// mutates the Classifier.
type Classifier struct {
	field string
}

// NewClassifier returns a Classifier that reports the state of field on each
// object passed to Classify. It returns ErrEmptyField if field is empty.
func NewClassifier(field string) (*Classifier, error) {
	if field == "" {
		return nil, ErrEmptyField
	}
	return &Classifier{field: field}, nil
}

// Classify decodes line as a single JSON object and reports the state of the
// Classifier's configured field within it.
//
// Classify does not retain line or any of its bytes after it returns: the
// returned State is a fresh value with no aliasing relationship to the
// input. Classify returns ErrInvalidJSON, wrapping the decoder's error, if
// line is not a well-formed single JSON object.
func (c *Classifier) Classify(line []byte) (State, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(line, &obj); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	raw, present := obj[c.field]
	if !present {
		return StateAbsent, nil
	}
	if string(raw) == "null" {
		return StateNull, nil
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("%w: field %q: %v", ErrInvalidJSON, c.field, err)
	}
	if isEmptyValue(v) {
		return StateEmpty, nil
	}
	return StateValue, nil
}

// isEmptyValue reports whether a decoded JSON value is a length-zero array,
// object, or string. Numbers, booleans, and non-empty containers are never
// empty.
func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case string:
		return x == ""
	default:
		return false
	}
}
```

### The tool

`fieldstate` streams: `bufio.Scanner` reads stdin one line at a time and
`Classify` runs per line, so watching a live feed piped through the tool
never requires buffering the whole stream in memory, which matters for a
monitor meant to run continuously against production traffic. `run` takes
the argument slice and an `io.Reader`/`io.Writer` pair rather than touching
`os.Stdin`/`os.Stdout` directly, so a test drives it with a `strings.Reader`
and a `bytes.Buffer` with no process boundary involved. Every failure `run`
can produce -- a bad flag, a missing `-field`, a line that is not valid
JSON -- is something the caller fixes by changing the input, so all three
wrap the `errUsage` sentinel and `main` maps that to exit code 2, matching
the module's stated contract of exactly two outcomes: 0 for a clean stream,
2 the moment anything in it is malformed. Blank lines are skipped rather than
treated as errors, since a stray blank line at the end of a captured stream
is routine and carries no field to classify.

Create `main.go`:

```go
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the input: a bad
// flag, a missing -field, or a line that is not valid JSON. main maps it to
// exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run parses args, streams newline-delimited JSON objects from stdin, and
// writes one classification word per non-blank line to stdout. It never
// touches os.Stdin, os.Stdout, or os.Exit directly, so it can be exercised in
// a test with a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("fieldstate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	field := fs.String("field", "", "top-level JSON key to classify (required)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	c, err := NewClassifier(*field)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	scanner := bufio.NewScanner(stdin)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		state, err := c.Classify(line)
		if err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, lineNum, err)
		}
		fmt.Fprintln(stdout, state)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fieldstate -field NAME")
		fmt.Fprintln(os.Stderr, "reads newline-delimited JSON objects from stdin and classifies")
		fmt.Fprintln(os.Stderr, "one field per line as absent, null, empty, or value.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fieldstate:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '{"id":1}\n{"id":2,"results":null}\n{"id":3,"results":[]}\n{"id":4,"results":["a","b"]}\n' | go run . -field results
printf '{"id":1,"results":[]}\nnot json\n' | go run . -field results
```

Expected output:

```text
absent
null
empty
value
empty
fieldstate: usage: line 2: fieldstate: invalid JSON object: invalid character 'o' in literal null (expecting 'u')
```

The first four lines are the four states, in the order the partner's four
representative responses would produce them: no key at all, an explicit
`null`, an explicit `[]`, and a real two-element result. The last two lines
are the second invocation: line 1 classifies cleanly as `empty`, and line 2
is not JSON at all, so `run` stops the stream there and returns an error
wrapping `errUsage`, which `main` reports on stderr and turns into exit code
2 -- the tool never guesses past a line it cannot parse.

### Tests

`TestClassify` is the four-state table: absent, `null`, and empty spelled
three ways (`[]`, `{}`, `""`), against non-empty arrays, objects, and two
kinds of scalar, all through the one field name `results`.
`TestClassifyInvalidJSON` covers truncated JSON, non-JSON text, and
well-formed JSON whose top level is not an object -- all three must produce
`ErrInvalidJSON`, checkable with `errors.Is`. `TestNewClassifierRejectsEmptyField`
is the constructor's edge case.

`TestLenNaiveCollapsesAbsentNullAndEmpty` is the antipattern contrast at the
center of this module. `classifyLenNaive` is unexported and unreachable from
the tool's own classification path; the test decodes the same three lines --
absent, `null`, `[]` -- through it and shows all three report length zero,
then decodes the identical three lines through `Classify` and shows all
three states differ from each other. `TestRun` drives the command end to
end: a clean multi-line stream producing all four states in order, blank
lines skipped, a missing `-field` rejected, and an invalid line mid-stream
halting the run -- each outcome checked against `errUsage` or the exact
stdout produced.

Create `fieldstate_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// classifyLenNaive is the check this tool replaces: decode the field into a
// concrete []T and ask whether its length is zero. It is never exported and
// never reachable from the tool's own classification path; it exists so the
// tests can pin exactly what it cannot tell apart. Absent, null, and []
// (once successfully decoded) all produce a zero-length slice, so this
// check reports the same thing for three different wire shapes.
func classifyLenNaive(obj map[string]any, field string) int {
	v, ok := obj[field]
	if !ok {
		v = nil // absent decodes the same as an explicit null under this check
	}
	s, _ := v.([]any)
	return len(s)
}

func TestClassify(t *testing.T) {
	t.Parallel()

	c, err := NewClassifier("results")
	if err != nil {
		t.Fatalf("NewClassifier: %v", err)
	}

	tests := []struct {
		name string
		line string
		want State
	}{
		{name: "absent", line: `{"id":1}`, want: StateAbsent},
		{name: "null", line: `{"id":1,"results":null}`, want: StateNull},
		{name: "empty array", line: `{"id":1,"results":[]}`, want: StateEmpty},
		{name: "empty object", line: `{"id":1,"results":{}}`, want: StateEmpty},
		{name: "empty string", line: `{"id":1,"results":""}`, want: StateEmpty},
		{name: "array value", line: `{"id":1,"results":["a","b"]}`, want: StateValue},
		{name: "object value", line: `{"id":1,"results":{"a":1}}`, want: StateValue},
		{name: "scalar number value", line: `{"id":1,"results":5}`, want: StateValue},
		{name: "scalar bool value", line: `{"id":1,"results":false}`, want: StateValue},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := c.Classify([]byte(tc.line))
			if err != nil {
				t.Fatalf("Classify(%q): %v", tc.line, err)
			}
			if got != tc.want {
				t.Fatalf("Classify(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestClassifyInvalidJSON(t *testing.T) {
	t.Parallel()

	c, err := NewClassifier("results")
	if err != nil {
		t.Fatalf("NewClassifier: %v", err)
	}

	tests := []string{
		`{"id":1`,          // truncated
		`not json at all`,  // not JSON
		`["array","not","object"]`, // valid JSON, wrong top-level shape
	}
	for _, line := range tests {
		if _, err := c.Classify([]byte(line)); !errors.Is(err, ErrInvalidJSON) {
			t.Errorf("Classify(%q) error = %v, want ErrInvalidJSON", line, err)
		}
	}
}

func TestNewClassifierRejectsEmptyField(t *testing.T) {
	t.Parallel()

	if _, err := NewClassifier(""); !errors.Is(err, ErrEmptyField) {
		t.Fatalf("NewClassifier(\"\") error = %v, want ErrEmptyField", err)
	}
}

// TestLenNaiveCollapsesAbsentNullAndEmpty is the antipattern contrast: it
// shows that decoding straight into []T (or, as here, reading a decoded
// map's field as []any) and checking len()==0 gives the identical answer,
// 0, for three lines that are not the same fact about the wire -- exactly
// the ambiguity fieldstate exists to remove.
func TestLenNaiveCollapsesAbsentNullAndEmpty(t *testing.T) {
	t.Parallel()

	decode := func(line string) map[string]any {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("decode(%q): %v", line, err)
		}
		return obj
	}

	absent := classifyLenNaive(decode(`{"id":1}`), "results")
	null := classifyLenNaive(decode(`{"id":1,"results":null}`), "results")
	empty := classifyLenNaive(decode(`{"id":1,"results":[]}`), "results")

	if absent != 0 || null != 0 || empty != 0 {
		t.Fatalf("naive lengths = absent:%d null:%d empty:%d, want all 0", absent, null, empty)
	}

	c, err := NewClassifier("results")
	if err != nil {
		t.Fatalf("NewClassifier: %v", err)
	}
	gotAbsent, _ := c.Classify([]byte(`{"id":1}`))
	gotNull, _ := c.Classify([]byte(`{"id":1,"results":null}`))
	gotEmpty, _ := c.Classify([]byte(`{"id":1,"results":[]}`))
	if gotAbsent == gotNull || gotNull == gotEmpty || gotAbsent == gotEmpty {
		t.Fatalf("Classify collapsed distinct states: absent=%q null=%q empty=%q", gotAbsent, gotNull, gotEmpty)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		stdin   string
		want    string
		wantErr bool
	}{
		{
			name: "four states in order",
			args: []string{"-field", "results"},
			stdin: `{"id":1}
{"id":2,"results":null}
{"id":3,"results":[]}
{"id":4,"results":["a"]}
`,
			want: "absent\nnull\nempty\nvalue\n",
		},
		{
			name:  "blank lines are skipped",
			stdin: "\n{\"id\":1,\"results\":[]}\n\n",
			args:  []string{"-field", "results"},
			want:  "empty\n",
		},
		{
			name:    "missing -field is a usage error",
			args:    []string{},
			stdin:   `{"id":1}`,
			wantErr: true,
		},
		{
			name:    "invalid JSON line is a usage error",
			args:    []string{"-field", "results"},
			stdin:   "{\"id\":1,\"results\":[]}\nnot json\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), tc.want)
			}
		})
	}
}
```

## Review

`Classify` is correct when the four states it reports match the four
distinct facts a partner's JSON can encode: no key, an explicit `null`, an
explicit empty container, and a real value. The mechanism is decoding one
level shallow into `map[string]json.RawMessage` before deciding anything, so
presence and the `null` literal are both visible before any type-specific
unmarshaling happens -- the same ordering the tri-state PATCH exercise in
this lesson relies on. `NewClassifier` rejects an empty field name with
`ErrEmptyField`; `Classify` rejects a malformed line with `ErrInvalidJSON`,
both checkable with `errors.Is`. `run` streams one line at a time rather than
buffering the input, stops at the first malformed line rather than guessing
past it, and maps every input problem to exit code 2, reserving 1 for a
runtime failure this particular tool never produces. Run
`go test -count=1 -race ./...` to confirm the state table, the invalid-JSON
cases, the naive-length contrast, and `run`'s end-to-end behavior.

## Resources

- [encoding/json: RawMessage](https://pkg.go.dev/encoding/json#RawMessage) — the delayed-decoding type this classifier is built on.
- [encoding/json package overview](https://pkg.go.dev/encoding/json) — how Unmarshal maps JSON null, arrays, and objects onto Go's `any`.
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — the line-oriented streaming reader `run` uses.
- [RFC 8259: The JSON Data Interchange Format](https://www.rfc-editor.org/rfc/rfc8259) — the definitions of a JSON object, array, and the null literal this classifier distinguishes.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-typed-nil-any-kv-store.md](16-typed-nil-any-kv-store.md) | Next: [18-csv-null-marker-importer.md](18-csv-null-marker-importer.md)
