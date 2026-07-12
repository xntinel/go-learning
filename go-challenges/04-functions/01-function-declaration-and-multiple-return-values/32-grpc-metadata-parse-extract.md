# Exercise 32: gRPC Metadata Extractor With Validation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye casos borde).

gRPC metadata stores every header as a slice of strings, because HTTP/2
allows repeated headers — and that shape hides three distinct ways a
tracing header can go wrong: it was never sent, it was sent but repeated
(ambiguous), or it was sent once but is not a well-formed trace id. This
exercise builds `ExtractTraceID(md MD, key string) (value string, found
bool, error)`, using the comma-ok form on the underlying map so "absent"
and "present but empty" can never be confused.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
grpcmeta/                  independent module: example.com/grpc-metadata-parse-extract
  go.mod                   go 1.24
  grpcmeta.go              package grpcmeta; MD type; TraceIDHeader; ExtractTraceID(md,key) (value,found,error)
  cmd/
    demo/
      main.go              valid trace id, absent header, malformed value, duplicated header
  grpcmeta_test.go          valid; absent is not an error; present-but-empty is an error; malformed formats table; duplicated header; absent vs present-empty distinction
```

- Files: `grpcmeta.go`, `cmd/demo/main.go`, `grpcmeta_test.go`.
- Implement: `ExtractTraceID(md MD, key string) (value string, found bool, err error)` reading `md[key]` with the comma-ok form, returning `("", false, nil)` when the key is absent, an error when it is present with zero or more than one value, and validating the single remaining value as a 32-character lowercase-hex trace id before reporting `(value, true, nil)`.
- Test: a valid trace id round-trips; an absent header returns `found == false, err == nil`; a header present with zero values is an error, not a silent absence; malformed formats (wrong length, uppercase, non-hex) all error; a repeated header with more than one value errors; the absent-key and present-empty-key cases are asserted side by side to prove the comma-ok form actually distinguishes them.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a plain index expression breaks this exact lookup

`google.golang.org/grpc/metadata.MD` is `map[string][]string`. A plain
index expression, `values := md[key]`, returns the zero value (`nil`, a
`nil` slice) whether the key is entirely absent from the map or present
with an explicitly empty slice — Go's map indexing cannot tell those
apart without the comma-ok form. For most maps that distinction is
academic. For gRPC metadata it is not: a proxy or client library can
legitimately send a header with an empty value list as an artifact of
header stripping or normalization, and treating that the same as "this
request was never traced" would make a debugging session blind to exactly
the requests worth investigating — the ones where tracing broke *after*
being requested, rather than never being requested at all. `values, ok :=
md[key]` is the only way to keep `ok == false` (never sent) and `ok == true,
len(values) == 0` (sent broken) as separate, reportable outcomes.

Create `grpcmeta.go`:

```go
package grpcmeta

import "fmt"

// MD mirrors google.golang.org/grpc/metadata.MD: a gRPC context carries
// each header as a slice of values, since HTTP/2 headers may legally
// repeat.
type MD map[string][]string

// TraceIDHeader is the conventional gRPC metadata key carrying a trace id.
const TraceIDHeader = "x-trace-id"

// ExtractTraceID looks up the tracing header on md, distinguishing three
// outcomes: the header is absent entirely (a normal, untraced request),
// the header is present but malformed (a client bug, worth an error), and
// the header is present and valid.
//
// The lookup uses the comma-ok form so "key not present at all" and "key
// present with an empty value" are never confused -- a plain index
// expression would return the same empty string for both.
func ExtractTraceID(md MD, key string) (value string, found bool, err error) {
	values, ok := md[key]
	if !ok {
		return "", false, nil
	}
	if len(values) == 0 {
		return "", false, fmt.Errorf("metadata %q: present but has no values", key)
	}
	if len(values) > 1 {
		return "", false, fmt.Errorf("metadata %q: %d values, want exactly 1", key, len(values))
	}

	id := values[0]
	if !isValidTraceID(id) {
		return "", false, fmt.Errorf("metadata %q: %q is not a valid 32-character hex trace id", key, id)
	}
	return id, true, nil
}

// isValidTraceID reports whether id is a 32-character lowercase hex string,
// the OpenTelemetry trace-id format.
func isValidTraceID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	grpcmeta "example.com/grpc-metadata-parse-extract"
)

func main() {
	valid := grpcmeta.MD{
		grpcmeta.TraceIDHeader: {"4bf92f3577b34da6a3ce929d0e0e4736"},
	}
	value, found, err := grpcmeta.ExtractTraceID(valid, grpcmeta.TraceIDHeader)
	fmt.Printf("valid header:    value=%s found=%t err=%v\n", value, found, err)

	absent := grpcmeta.MD{}
	value, found, err = grpcmeta.ExtractTraceID(absent, grpcmeta.TraceIDHeader)
	fmt.Printf("absent header:   value=%q found=%t err=%v\n", value, found, err)

	malformed := grpcmeta.MD{
		grpcmeta.TraceIDHeader: {"not-a-trace-id"},
	}
	value, found, err = grpcmeta.ExtractTraceID(malformed, grpcmeta.TraceIDHeader)
	fmt.Printf("malformed:       value=%q found=%t err=%v\n", value, found, err)

	duplicated := grpcmeta.MD{
		grpcmeta.TraceIDHeader: {"4bf92f3577b34da6a3ce929d0e0e4736", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	value, found, err = grpcmeta.ExtractTraceID(duplicated, grpcmeta.TraceIDHeader)
	fmt.Printf("duplicated:      value=%q found=%t err=%v\n", value, found, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid header:    value=4bf92f3577b34da6a3ce929d0e0e4736 found=true err=<nil>
absent header:   value="" found=false err=<nil>
malformed:       value="" found=false err=metadata "x-trace-id": "not-a-trace-id" is not a valid 32-character hex trace id
duplicated:      value="" found=false err=metadata "x-trace-id": 2 values, want exactly 1
```

### Tests

Create `grpcmeta_test.go`:

```go
package grpcmeta

import "testing"

const validID = "4bf92f3577b34da6a3ce929d0e0e4736"

func TestExtractTraceIDValid(t *testing.T) {
	t.Parallel()
	md := MD{TraceIDHeader: {validID}}

	value, found, err := ExtractTraceID(md, TraceIDHeader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if value != validID {
		t.Fatalf("value = %q, want %q", value, validID)
	}
}

func TestExtractTraceIDAbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	md := MD{}

	value, found, err := ExtractTraceID(md, TraceIDHeader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("found = true, want false")
	}
	if value != "" {
		t.Fatalf("value = %q, want empty", value)
	}
}

func TestExtractTraceIDPresentButEmpty(t *testing.T) {
	t.Parallel()
	md := MD{TraceIDHeader: {}}

	_, found, err := ExtractTraceID(md, TraceIDHeader)
	if err == nil {
		t.Fatal("want an error when the header key exists with zero values")
	}
	if found {
		t.Fatal("found = true, want false")
	}
}

func TestExtractTraceIDMalformed(t *testing.T) {
	t.Parallel()
	cases := []string{
		"not-a-trace-id",
		"4bf92f3577b34da6a3ce929d0e0e47",   // too short
		"4BF92F3577B34DA6A3CE929D0E0E4736", // uppercase not allowed
	}
	for _, id := range cases {
		md := MD{TraceIDHeader: {id}}
		_, found, err := ExtractTraceID(md, TraceIDHeader)
		if err == nil {
			t.Errorf("id %q: want an error", id)
		}
		if found {
			t.Errorf("id %q: found = true, want false", id)
		}
	}
}

func TestExtractTraceIDMultipleValuesIsAnError(t *testing.T) {
	t.Parallel()
	md := MD{TraceIDHeader: {validID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}

	_, found, err := ExtractTraceID(md, TraceIDHeader)
	if err == nil {
		t.Fatal("want an error when the header repeats with more than one value")
	}
	if found {
		t.Fatal("found = true, want false")
	}
}

func TestExtractTraceIDKeyNotPresentVsEmptyValue(t *testing.T) {
	t.Parallel()

	// A key entirely absent from the map and a key present with an empty
	// value list must not be conflated -- the comma-ok form is what makes
	// this distinguishable in the implementation.
	absent := MD{}
	_, foundAbsent, errAbsent := ExtractTraceID(absent, TraceIDHeader)

	presentEmpty := MD{TraceIDHeader: {}}
	_, foundPresentEmpty, errPresentEmpty := ExtractTraceID(presentEmpty, TraceIDHeader)

	if foundAbsent != false || errAbsent != nil {
		t.Fatalf("absent key: found=%t err=%v, want false/nil", foundAbsent, errAbsent)
	}
	if foundPresentEmpty != false || errPresentEmpty == nil {
		t.Fatalf("present-but-empty key: found=%t err=%v, want false/non-nil", foundPresentEmpty, errPresentEmpty)
	}
}
```

## Review

`ExtractTraceID` is correct when "never sent" stays a quiet `(_, false,
nil)` while every other absence of a usable value becomes a loud error —
present-but-empty, present-but-repeated, and present-but-malformed all
signal a client bug worth surfacing, not a routine untraced request.
`TestExtractTraceIDKeyNotPresentVsEmptyValue` is the load-bearing test: it
puts both cases side by side and proves the comma-ok lookup is what keeps
them apart, exactly the distinction a plain `md[key]` index cannot make.

The mistake to avoid is validating the trace id format *before* checking
`len(values) != 1` — validating `values[0]` first when `values` might be
empty panics on an out-of-range index, and when `values` has more than one
entry, validating only the first silently ignores the ambiguity of a
repeated header instead of reporting it.

## Resources

- [google.golang.org/grpc/metadata](https://pkg.go.dev/google.golang.org/grpc/metadata) — the real `MD` type and `FromIncomingContext` this exercise's `MD` models.
- [W3C Trace Context: trace-id](https://www.w3.org/TR/trace-context/#trace-id) — the 32-character lowercase-hex format this exercise validates against.
- [gRPC: Metadata](https://grpc.io/docs/guides/metadata/) — why gRPC headers are represented as repeatable values in the first place.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-graceful-shutdown-coordinated.md](31-graceful-shutdown-coordinated.md) | Next: [33-binary-search-sorted-list.md](33-binary-search-sorted-list.md)
