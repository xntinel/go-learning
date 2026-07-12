# Exercise 13: NDJSON Idempotency Filter and the Incomparable-Key Panic

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A webhook receiver or a Kafka consumer sits in front of a handler that is
not itself idempotent -- charging a card, sending an email, writing a
ledger entry -- and its first job is to drop anything it has already
processed, keyed by whatever idempotency identifier the sender attaches to
each record. That identifier often arrives as hex- or base64-encoded bytes
rather than plain text: a signature, a content hash, a UUID's raw form. The
receiver decodes it to its natural representation, `[]byte`, and needs
something to track "have I seen this key before" -- which sounds like a
map, until you remember that a Go map key must be *comparable*, and `[]byte`
is the one common built-in type that is not.

The compiler only enforces that rule against a concrete, statically typed
map. A `map[[]byte]struct{}` is rejected at compile time -- `invalid map
key type []byte`. But a filter written generically, to accept either a
`string` key or a decoded `[]byte` key without deciding up front, reaches
for `map[any]struct{}` instead, and `any` accepts anything, including a
`[]byte`. That version compiles. It even runs, for a while: Go's map
implementation hashes the key on every access to keep its behavior
consistent regardless of the map's size, so a `[]byte` boxed into an `any`
panics with `runtime error: hash of unhashable type []uint8` on the very
first record it ever tries to track, not on some later duplicate. One
malformed key type and the whole dedup filter -- and everything downstream
of it -- goes down with `panic`, not a clean rejection of one bad record.

This module builds `dedupe`, a command-line filter that reads NDJSON from
stdin, extracts an idempotency key from a configured field, decodes it from
hex when asked, and writes only the first-seen record for each key to
stdout.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
dedupe/                    module example.com/dedupe
  go.mod                   go 1.24
  dedupe.go                package main — Deduplicator; New, Key, Seen; five sentinel errors
  dedupe_test.go           package main — Key table, hex case-unification, the incomparable-
                           key panic contrast, run() end to end
  main.go                  package main — -field/-hex flags, exit codes
```

- Files: `dedupe.go`, `dedupe_test.go`, `main.go`.
- Implement: `New(field string, hexEncoded bool) (*Deduplicator, error)` rejecting an empty field name with `ErrEmptyField`; `(*Deduplicator).Key(line []byte) (string, error)` extracting and, when `hexEncoded`, hex-decoding the configured field into a string key, returning `ErrInvalidJSON`, `ErrMissingField`, `ErrFieldNotString`, or `ErrInvalidHex`; `Seen(key string) bool` reporting and recording duplicates.
- Tool: `dedupe -field NAME [-hex]` reads NDJSON from stdin, writes every first-seen line unchanged to stdout, and writes the total duplicate count to stderr as `duplicates: N`. Exit 0 on success, exit 2 for a missing `-field`, a line that is not valid JSON, or a record missing the configured field (all usage errors), exit 1 for a runtime I/O failure reading stdin or writing stdout.
- Test: the `Key` table across a plain string field, hex decoding, and every error case; hex decoding unifying two different-case spellings of the same bytes into one key; the incomparable-key contrast proving a `[]byte` boxed into a `map[any]struct{}` panics on its very first use, not its second; `run` end to end over `strings.Reader` and two `bytes.Buffer`s, including the duplicate count and every usage-error case.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/13-idempotency-key-dedup-filter
cd go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/13-idempotency-key-dedup-filter
go mod edit -go=1.24
```

### []byte cannot be a map key, boxed or not, and the panic proves it immediately

`map[K]V` requires `K` to be comparable, and the Go spec is explicit that
slice types are not: they have no `==` operator, because two slices sharing
identical contents are still two different windows onto memory. The
compiler catches this instantly for a concrete map type:

```go
var seen map[[]byte]struct{} // compile error: invalid map key type []byte
```

An `any`-keyed map sidesteps the compiler entirely, because `any` is
comparable at the *static* type level -- the check the compiler can do --
even though its *dynamic* value might not be. `seen[any(decoded)] =
struct{}{}` compiles cleanly for any `decoded []byte`. What was a
compile-time error becomes a runtime one, and it does not wait for a second
key to collide with the first: Go's runtime computes a key's hash on every
single map access, read or write, specifically so a map's behavior does not
depend on how many entries it already holds. The very first line ever
inserted panics:

```go
seen := make(map[any]struct{})
seen[any(decoded)] = struct{}{} // panics immediately: hash of unhashable type []uint8
```

There is no scenario where this "works for a while." It fails the instant a
`[]byte`-boxed key touches the map, whether that is the filter's first
record of the day or its millionth. The fix is the one-line conversion Go
gives you for exactly this: `string(decoded)`. A `string` copies the bytes
into an immutable, comparable value, and two decoded byte slices with
identical contents produce equal strings -- which is also why decoding
before comparing catches a duplicate that differs only in the *encoding* of
its wire form, such as the same key hex-spelled in a different case.

Create `dedupe.go`:

```go
// Command dedupe filters duplicate NDJSON records by an idempotency key
// carried in a configured field, the standard exactly-once-delivery guard a
// webhook receiver or Kafka consumer runs in front of a handler that is not
// itself idempotent.
//
// The logic below exists to get one detail right: an idempotency key
// decoded from its wire form (hex-encoded bytes) is a []byte, and []byte is
// not comparable -- it can never be a valid map key, whether it is boxed
// into an any or not. Deduplicator always converts the decoded bytes to a
// string before using them as a key. See the package tests for what
// happens when that conversion is skipped.
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Sentinel errors returned by New and Deduplicator.Key. Callers should test
// for them with errors.Is rather than by comparing error strings.
var (
	// ErrEmptyField means New was called with an empty field name.
	ErrEmptyField = errors.New("dedupe: field name must not be empty")
	// ErrInvalidJSON means a line was not a valid JSON object.
	ErrInvalidJSON = errors.New("dedupe: invalid JSON")
	// ErrMissingField means the configured field was absent from a record.
	ErrMissingField = errors.New("dedupe: field missing from record")
	// ErrFieldNotString means the configured field was present but its
	// JSON value was not a string.
	ErrFieldNotString = errors.New("dedupe: field is not a JSON string")
	// ErrInvalidHex means hex decoding was requested and the field's
	// string value was not valid hex.
	ErrInvalidHex = errors.New("dedupe: field is not valid hex")
)

// Deduplicator filters NDJSON records, keeping only the first record seen
// for each idempotency key extracted from a configured field.
//
// Deduplicator is not safe for concurrent use: it is a single stateful
// stream filter meant to be driven by one goroutine reading one input in
// order, the same way a bufio.Scanner is.
type Deduplicator struct {
	field string
	hex   bool
	seen  map[string]struct{}
}

// New returns a Deduplicator that dedupes on the JSON field named field. If
// hexEncoded is true, the field's string value is treated as hex-encoded
// bytes and decoded before use as a key; otherwise the field's string
// value is used as the key directly. New returns ErrEmptyField if field is
// empty.
func New(field string, hexEncoded bool) (*Deduplicator, error) {
	if field == "" {
		return nil, ErrEmptyField
	}
	return &Deduplicator{field: field, hex: hexEncoded, seen: make(map[string]struct{})}, nil
}

// Key extracts the idempotency key from one JSON record.
//
// If hex decoding is enabled, the field's string value is decoded from hex
// into raw bytes and the result is converted to a string -- that
// conversion is what makes the derived key a valid, comparable value fit
// to pass to Seen. It returns ErrInvalidJSON if line is not a JSON object,
// ErrMissingField if the configured field is absent, ErrFieldNotString if
// present but not a JSON string, or ErrInvalidHex if hex decoding is
// enabled and the value is not valid hex.
func (d *Deduplicator) Key(line []byte) (string, error) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(line, &record); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	raw, ok := record[d.field]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrMissingField, d.field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: field %q", ErrFieldNotString, d.field)
	}
	if !d.hex {
		return value, nil
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("%w: field %q: %v", ErrInvalidHex, d.field, err)
	}
	// decoded is []byte, the key's natural decoded form. []byte has no ==
	// operator and cannot be a map key, boxed in an any or not; converting
	// it to a string here is what makes the value below legal at all, and
	// it is also what makes two keys that decode to the same bytes compare
	// equal even if their hex spelling differed in case.
	return string(decoded), nil
}

// Seen reports whether key has already been observed, and records it as
// seen if this is the first time. Callers use the return value directly:
// if Seen(k) is true, the record is a duplicate and should be dropped.
func (d *Deduplicator) Seen(key string) bool {
	if _, ok := d.seen[key]; ok {
		return true
	}
	d.seen[key] = struct{}{}
	return false
}
```

### The tool

`dedupe` streams: it reads one line at a time with `bufio.Scanner` and
writes each first-seen line to stdout as it goes, rather than buffering the
whole input, which matters for the same reason streaming matters for any
NDJSON filter meant to sit in a pipeline ahead of a handler processing
records as they arrive. `run` takes the argument slice, an `io.Reader` for
stdin, and two `io.Writer`s for stdout and stderr, so a test can drive it
end to end with a `strings.Reader` and a pair of `bytes.Buffer`s and never
touch `os.Args`, a real stdin, or `os.Exit`. Every failure `run` can
produce before it starts streaming -- a bad flag, an empty `-field` -- and
every failure per line -- invalid JSON, a missing field -- is something the
caller fixes by changing the command line or the input, so all of them wrap
the `errUsage` sentinel and `main` maps that to exit code 2. A failure
reading stdin or writing stdout wraps a plain error instead, and `main`
maps anything that does not wrap `errUsage` to exit code 1.

Create `main.go`:

```go
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the command line
// or the input stream: a bad flag, an empty -field, a line that is not
// valid JSON, or a record missing the configured field. main maps it to
// exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run reads NDJSON records from stdin, writes every first-seen record to
// stdout unchanged, and writes the total duplicate count to stderr. It
// never touches os.Stdin, os.Stdout, os.Stderr, or os.Exit directly, so it
// can be driven end to end in a test with a strings.Reader and two
// bytes.Buffers.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dedupe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	field := fs.String("field", "", "JSON field carrying the idempotency key (required)")
	hexFlag := fs.Bool("hex", false, "treat the field's string value as hex-encoded bytes")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	d, err := New(*field, *hexFlag)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	duplicates := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		key, err := d.Key(line)
		if err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, lineNo, err)
		}
		if d.Seen(key) {
			duplicates++
			continue
		}
		if _, err := fmt.Fprintln(stdout, string(line)); err != nil {
			return fmt.Errorf("dedupe: writing output: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("dedupe: reading input: %w", err)
	}

	fmt.Fprintf(stderr, "duplicates: %d\n", duplicates)
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: dedupe -field NAME [-hex]")
		fmt.Fprintln(os.Stderr, "reads NDJSON from stdin, writes unique records to stdout,")
		fmt.Fprintln(os.Stderr, "and a duplicate count to stderr.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "dedupe:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '{"event_id":"3f0a1b","type":"payment.created"}\n{"event_id":"3f0a1b","type":"payment.created"}\n{"event_id":"a1b2c3","type":"payment.updated"}\n{"event_id":"3F0A1B","type":"payment.created"}\n' | go run . -field event_id -hex
printf '{"type":"payment.created"}\n' | go run . -field event_id
```

Expected output:

```text
{"event_id":"3f0a1b","type":"payment.created"}
{"event_id":"a1b2c3","type":"payment.updated"}
duplicates: 2
dedupe: usage: line 1: dedupe: field missing from record: "event_id"
```

The first invocation feeds four records: an exact repeat of `3f0a1b`, a
distinct key `a1b2c3`, and the *same* key as the first spelled `3F0A1B` --
different hex text, identical decoded bytes. Two lines reach stdout, and
`duplicates: 2` on stderr counts both the exact repeat and the re-cased one,
which is exactly the guarantee decoding to bytes before keying on them is
supposed to buy. The second invocation, run against a record with no
`event_id` field at all, hits `ErrMissingField` on line 1 and exits 2 --
`go run` itself appends the `exit status 2` line to a terminal, which is not
part of the program's own output and is omitted above.

### Tests

`TestKey` is the table across the field-extraction logic: a plain string
field, hex decoding to bytes, and one subtest per sentinel error --
missing field, non-string field, invalid JSON, invalid hex.
`TestHexDecodingUnifiesCaseVariants` isolates the property the first
invocation above demonstrates end to end: `"3f0a1b"` and `"3F0A1B"` decode
to the same three bytes and therefore the same key, which a filter that
deduped on the raw hex string would miss entirely.

`TestNaiveAnyKeyPanicsImmediately` is the heart of the module. `putNaive` is
unexported and unreachable from the package API; the test calls it exactly
once, with `defer`/`recover`, and asserts that the *single* call panics with
a message mentioning an unhashable type -- proving the failure is not
"works once, breaks on the duplicate" but "never works at all." `TestRun`
(as `TestRunDedupesAndCountsDuplicates` and `TestRunRejectsBadInput`) drives
the whole command end to end: the exact stdout and duplicate count for a
batch including a re-cased duplicate, and every usage-error input --
missing `-field`, invalid JSON, a record missing the field -- mapped to
`errUsage`.

Create `dedupe_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// putNaive is the mutation this tool tends to ship with the first time
// someone generalizes "the key might be a string or might be raw bytes":
// key it on an any so either shape fits, without converting []byte to
// string first. It compiles -- any([]byte(...)) is a legal conversion --
// and it is never exported and never reachable from Deduplicator; it
// exists so the tests can pin exactly what using it does.
func putNaive(seen map[any]struct{}, decoded []byte) {
	seen[any(decoded)] = struct{}{}
}

func TestNewRejectsEmptyField(t *testing.T) {
	t.Parallel()

	if _, err := New("", false); !errors.Is(err, ErrEmptyField) {
		t.Fatalf("New(\"\", false) error = %v, want ErrEmptyField", err)
	}
}

func TestKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		field   string
		hex     bool
		line    string
		want    string
		wantErr error
	}{
		{name: "plain string field", field: "event_id", line: `{"event_id":"evt-1","type":"created"}`, want: "evt-1"},
		{name: "hex field decodes to bytes", field: "event_id", hex: true, line: `{"event_id":"3f0a1b"}`, want: "\x3f\x0a\x1b"},
		{name: "hex field is case-insensitive by value", field: "event_id", hex: true, line: `{"event_id":"3F0A1B"}`, want: "\x3f\x0a\x1b"},
		{name: "missing field", field: "event_id", line: `{"type":"created"}`, wantErr: ErrMissingField},
		{name: "field not a string", field: "event_id", line: `{"event_id":42}`, wantErr: ErrFieldNotString},
		{name: "invalid JSON", field: "event_id", line: `not json`, wantErr: ErrInvalidJSON},
		{name: "invalid hex", field: "event_id", hex: true, line: `{"event_id":"zz"}`, wantErr: ErrInvalidHex},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d, err := New(tc.field, tc.hex)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := d.Key([]byte(tc.line))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Key(%q) error = %v, want %v", tc.line, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Key(%q): %v", tc.line, err)
			}
			if got != tc.want {
				t.Fatalf("Key(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// TestHexDecodingUnifiesCaseVariants shows why decoding to bytes (and then
// to string) is not just about avoiding a panic: "3f0a1b" and "3F0A1B" are
// different strings but decode to the identical three bytes, so a receiver
// that dedupes on the decoded key correctly treats a retried webhook with
// re-cased hex as the duplicate it is. Deduping on the raw hex string
// would miss this.
func TestHexDecodingUnifiesCaseVariants(t *testing.T) {
	t.Parallel()

	d, err := New("event_id", true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	k1, err := d.Key([]byte(`{"event_id":"3f0a1b"}`))
	if err != nil {
		t.Fatalf("Key (lowercase): %v", err)
	}
	k2, err := d.Key([]byte(`{"event_id":"3F0A1B"}`))
	if err != nil {
		t.Fatalf("Key (uppercase): %v", err)
	}
	if k1 != k2 {
		t.Fatalf("keys differ: %q vs %q, want equal (same bytes, different hex case)", k1, k2)
	}
}

// TestNaiveAnyKeyPanicsImmediately is the heart of the module. A []byte
// boxed into an any is not comparable, and Go's runtime computes the hash
// of a map key on every access -- read or write -- to keep that behavior
// consistent regardless of how many entries the map already holds. The
// naive version therefore does not survive to a "duplicate": it panics on
// the very first record it ever tries to track.
func TestNaiveAnyKeyPanicsImmediately(t *testing.T) {
	t.Parallel()

	seen := make(map[any]struct{})
	decoded := []byte{0x3f, 0x0a, 0x1b}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("putNaive did not panic on the first insert; want a panic hashing an unhashable type")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "unhashable type") {
			t.Fatalf("panic = %q, want it to mention an unhashable type", msg)
		}
	}()
	putNaive(seen, decoded)
	t.Fatal("unreachable: putNaive should have panicked")
}

func TestRunDedupesAndCountsDuplicates(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`{"event_id":"3f0a1b","type":"payment.created"}`,
		`{"event_id":"3f0a1b","type":"payment.created"}`,
		`{"event_id":"a1b2c3","type":"payment.updated"}`,
		`{"event_id":"3F0A1B","type":"payment.created"}`,
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	err := run([]string{"-field", "event_id", "-hex"}, strings.NewReader(input), &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	wantStdout := `{"event_id":"3f0a1b","type":"payment.created"}` + "\n" +
		`{"event_id":"a1b2c3","type":"payment.updated"}` + "\n"
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	if got := stderr.String(); got != "duplicates: 2\n" {
		t.Fatalf("stderr = %q, want %q", got, "duplicates: 2\n")
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		args  []string
		input string
	}{
		{name: "missing -field flag", args: nil, input: ""},
		{name: "invalid JSON line", args: []string{"-field", "event_id"}, input: "not json\n"},
		{name: "record missing field", args: []string{"-field", "event_id"}, input: `{"type":"created"}` + "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.input), &stdout, &stderr)
			if !errors.Is(err, errUsage) {
				t.Fatalf("run(%v): error = %v, want errUsage", tc.args, err)
			}
		})
	}
}
```

## Review

The filter is correct when every genuinely distinct idempotency key reaches
stdout exactly once and every repeat, however it was spelled on the wire,
is counted on stderr rather than reprocessed. `New` rejects an empty field
name with `ErrEmptyField`; `Key` maps every extraction failure to a
specific sentinel -- `ErrInvalidJSON`, `ErrMissingField`,
`ErrFieldNotString`, `ErrInvalidHex` -- all checkable with `errors.Is`. The
trap this module isolates is sharper than "compiles but is wrong": a
`[]byte` idempotency key boxed into a `map[any]struct{}` panics the instant
it is used, not on some later duplicate, because Go hashes a map key on
every access to keep that behavior independent of how full the map already
is. Converting the decoded bytes to a `string` before ever touching the map
is the entire fix, and it has a second benefit `TestHexDecodingUnifiesCaseVariants`
pins: it makes two differently-cased spellings of the same bytes compare
equal, which a filter deduping on raw hex text would miss. `run` streams
one record at a time, maps every usage mistake to exit 2 and any I/O
failure to exit 1, and never touches `os.Args` or `os.Exit` directly. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — the rule that slice types have no `==`, and why that only shows up at runtime for an interface-typed map key.
- [`encoding/hex`](https://pkg.go.dev/encoding/hex) — `DecodeString`, used to turn the wire-form key into raw bytes.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-at-a-time streaming reader this tool is built on.
- [Idempotency keys (Stripe API docs)](https://stripe.com/docs/api/idempotent_requests) — the production pattern this filter implements the receiving side of.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-outlier-detector-map-addressability.md](12-outlier-detector-map-addressability.md) | Next: [14-resource-state-reconciler-diff.md](14-resource-state-reconciler-diff.md)
