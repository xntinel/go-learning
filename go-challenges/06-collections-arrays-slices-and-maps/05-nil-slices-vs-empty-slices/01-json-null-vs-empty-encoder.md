# Exercise 1: JSON Encoder — null for absent, [] for explicit empty

The producer side of an API has to decide, per field, whether "no value" goes on
the wire as `null` or as `[]`. This exercise builds the two encoders that make
that decision explicit and pins the exact JSON bytes each produces, so the
contract cannot regress into an accident.

This module is fully self-contained: its own `go mod init`, its own `enc`
package, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
nilvsempty/                        independent module: example.com/nilvsempty
  go.mod
  internal/enc/enc.go              EncodeNames, EncodeStrict, IsNil, IsEmpty
  internal/enc/enc_test.go         golden-JSON + truth-table + append-on-nil tests
  cmd/demo/main.go                 runs both encoders on nil, empty, populated
```

Files: `internal/enc/enc.go`, `internal/enc/enc_test.go`, `cmd/demo/main.go`.
Implement: `EncodeNames` (nil passes through to `null`), `EncodeStrict` (nil
normalized to `[]string{}` so output is always an array), plus `IsNil`/`IsEmpty`.
Test: golden JSON for nil, `[]string{}`, and populated input; the append-on-nil
contract; an IsNil/IsEmpty truth table.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/01-json-null-vs-empty-encoder/internal/enc go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/01-json-null-vs-empty-encoder/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/01-json-null-vs-empty-encoder
```

### Two encoders, two contracts

`EncodeNames` marshals whatever slice it is handed. If the caller passes a nil
slice, `json.Marshal` renders `"names":null`; if the caller passes `[]string{}`,
it renders `"names":[]`. The encoder is a faithful mirror of the value's
nil-ness, which is the right behavior when the caller genuinely wants to
communicate the absent/empty distinction downstream — a tri-state field, a cache
that treats `null` specially.

`EncodeStrict` makes the opposite promise: the field is *always* an array. It
normalizes a nil input to `[]string{}` before marshaling, so the output is
`"names":[]` no matter what the caller passed. This is the contract a list
endpoint wants — a client paging through results should never have to handle
`null` where it expected a JSON array.

The subtle point is that both are correct; they encode different contracts. The
mistake is not picking one but letting the zero value pick for you. `IsNil` and
`IsEmpty` are diagnostics that make the difference visible in a test: `IsNil`
answers the identity question (`s == nil`), `IsEmpty` answers the length question
(`len(s) == 0`). A nil slice is both nil and empty; an empty slice is empty but
not nil.

Create `internal/enc/enc.go`:

```go
package enc

import "encoding/json"

// Payload is the response body. Names is a plain []string, so its nil-ness
// flows straight through json.Marshal: nil -> null, non-nil empty -> [].
type Payload struct {
	Names []string `json:"names"`
}

// EncodeNames marshals names as-is. A nil slice becomes "names":null; a non-nil
// empty slice becomes "names":[]. Use it when the absent/empty distinction is
// part of the contract the caller wants to communicate.
func EncodeNames(names []string) ([]byte, error) {
	return json.Marshal(Payload{Names: names})
}

// EncodeStrict normalizes a nil slice to a non-nil empty slice, so the field is
// always a JSON array. Use it at a list endpoint where the client expects [],
// never null.
func EncodeStrict(names []string) ([]byte, error) {
	if names == nil {
		names = []string{}
	}
	return json.Marshal(Payload{Names: names})
}

// IsNil reports the identity question: is this the nil slice value?
func IsNil(s []string) bool {
	return s == nil
}

// IsEmpty reports the length question: does this slice hold zero elements?
// A nil slice is empty; so is []string{}.
func IsEmpty(s []string) bool {
	return len(s) == 0
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/nilvsempty/internal/enc"
)

func main() {
	var absent []string    // nil
	explicit := []string{} // non-nil, empty
	populated := []string{"alice", "bob"}

	for _, tc := range []struct {
		label string
		names []string
	}{
		{"nil", absent},
		{"empty", explicit},
		{"populated", populated},
	} {
		lenient, _ := enc.EncodeNames(tc.names)
		strict, _ := enc.EncodeStrict(tc.names)
		fmt.Printf("%-9s names EncodeNames=%s EncodeStrict=%s (isNil=%v)\n",
			tc.label, lenient, strict, enc.IsNil(tc.names))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
nil       names EncodeNames={"names":null} EncodeStrict={"names":[]} (isNil=true)
empty     names EncodeNames={"names":[]} EncodeStrict={"names":[]} (isNil=false)
populated names EncodeNames={"names":["alice","bob"]} EncodeStrict={"names":["alice","bob"]} (isNil=false)
```

### Tests

The golden-JSON tests pin the exact bytes for each input under each encoder, so a
future "guard against nil" refactor cannot silently flip `null` to `[]` or the
reverse. `TestAppendOnNilWorks` pins the standard library's contract that
`append(nil, "x")` yields `[]string{"x"}`, guarding against a well-meaning but
wrong `if s == nil { return }` guard someone might add. The truth-table test
proves `IsNil` and `IsEmpty` answer their two different questions.

Create `internal/enc/enc_test.go`:

```go
package enc

import (
	"slices"
	"testing"
)

func TestEncodeNamesGolden(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input []string
		want  string
	}{
		{"nil passes through to null", nil, `{"names":null}`},
		{"empty slice to array", []string{}, `{"names":[]}`},
		{"populated to array", []string{"alice", "bob"}, `{"names":["alice","bob"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := EncodeNames(tc.input)
			if err != nil {
				t.Fatalf("EncodeNames: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("EncodeNames(%v) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}

func TestEncodeStrictAlwaysArray(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input []string
		want  string
	}{
		{"nil normalized to array", nil, `{"names":[]}`},
		{"empty stays array", []string{}, `{"names":[]}`},
		{"populated array", []string{"x"}, `{"names":["x"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := EncodeStrict(tc.input)
			if err != nil {
				t.Fatalf("EncodeStrict: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("EncodeStrict(%v) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}

func TestAppendOnNilWorks(t *testing.T) {
	t.Parallel()
	var s []string // nil
	s = append(s, "x")
	if !slices.Equal(s, []string{"x"}) {
		t.Fatalf("append(nil, \"x\") = %v, want [x]", s)
	}
}

func TestIsNilAndIsEmptyTruthTable(t *testing.T) {
	t.Parallel()
	var nilSlice []string
	emptySlice := []string{}
	full := []string{"a"}

	cases := []struct {
		name      string
		in        []string
		wantNil   bool
		wantEmpty bool
	}{
		{"nil", nilSlice, true, true},
		{"empty", emptySlice, false, true},
		{"populated", full, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsNil(tc.in); got != tc.wantNil {
				t.Errorf("IsNil = %v, want %v", got, tc.wantNil)
			}
			if got := IsEmpty(tc.in); got != tc.wantEmpty {
				t.Errorf("IsEmpty = %v, want %v", got, tc.wantEmpty)
			}
		})
	}
}
```

## Review

The encoders are correct when each golden test holds: `EncodeNames(nil)` is
exactly `{"names":null}`, `EncodeNames([]string{})` and `EncodeStrict(nil)` are
both exactly `{"names":[]}`, and a populated input is a JSON array under either.
The trap this exercise inoculates against is a nil slice leaking `null` onto a
list endpoint that promised an array; `EncodeStrict` is the deliberate
normalization, applied once at the producer boundary. `TestAppendOnNilWorks`
keeps the append-on-nil contract stable so nobody "fixes" a nil slice into a
regression. `IsNil` and `IsEmpty` are diagnostics for the lesson — production
code should encode the decision in the type, not branch on nil-ness everywhere.

## Resources

- [encoding/json — Marshal](https://pkg.go.dev/encoding/json#Marshal) — how nil slices and maps marshal to `null`.
- [Go Specification: Slice types](https://go.dev/ref/spec#Slice_types) — the nil slice and its zero value.
- [slices.Equal](https://pkg.go.dev/slices#Equal) — content comparison used in the append test.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-omitempty-vs-omitzero-response.md](02-omitempty-vs-omitzero-response.md)
