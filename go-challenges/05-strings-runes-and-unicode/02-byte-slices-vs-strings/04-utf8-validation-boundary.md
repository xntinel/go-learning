# Exercise 4: UTF-8 Validation and Sanitization at the API Boundary

A service writes user text into a `utf8mb4` column that rejects ill-formed UTF-8.
If invalid bytes slip past the handler, the failure surfaces as an opaque driver
error at write time instead of a clean 400 at ingress. This exercise builds the
input guard: a strict `ValidateUTF8` that rejects, and a lossy `Sanitize` that
repairs ill-formed sequences with `U+FFFD` — without allocating when the input is
already valid.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
utf8guard/                  independent module: example.com/utf8guard
  go.mod                    go 1.25
  utf8guard.go              ErrInvalidUTF8; ValidateUTF8, ValidateString, Sanitize
  cmd/
    demo/
      main.go               validates and sanitizes sample inputs
  utf8guard_test.go         table cases, zero-alloc-on-valid, FuzzValidateUTF8
```

- Files: `utf8guard.go`, `cmd/demo/main.go`, `utf8guard_test.go`.
- Implement: `ValidateUTF8([]byte) error` (wraps a sentinel with the offending offset), `ValidateString(string) error`, and `Sanitize([]byte, replacement []byte) []byte` that returns the input unchanged and allocation-free when already valid.
- Test: ASCII, valid multibyte, lone continuation byte, truncated sequence, overlong encoding, BOM; a zero-alloc assertion on valid input; a fuzz test proving `Sanitize` output always satisfies `utf8.Valid`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Validate at ingress, so invalid bytes never reach the store

A Go `string` is conventionally UTF-8 but the type does not enforce it — request
bytes off a socket can be arbitrary. Datastores are often stricter: a `utf8mb4`
column, a Postgres `text` column, a Mongo string field reject or mangle ill-formed
sequences. Catching that at the datastore is the worst place: you get a driver error
deep in a transaction, far from the request that caused it. The senior move is to
decide the policy at the boundary — strict endpoints reject, lossy-ingestion
endpoints repair — so the failure is a clean, attributable 400 or a well-defined
replacement.

`ValidateUTF8` walks the bytes with `utf8.DecodeRune`. The key subtlety is
distinguishing a genuinely invalid byte from a valid encoding of the replacement
character itself: `utf8.DecodeRune` returns `(utf8.RuneError, 1)` for an invalid
byte, but `(utf8.RuneError, 3)` for the three-byte encoding of a real `U+FFFD`. So
the invalid test is `r == utf8.RuneError && size <= 1`, checking the *size*, not just
the rune. On the first bad byte it returns a sentinel error wrapped with `%w` and
the offset, so callers can `errors.Is(err, ErrInvalidUTF8)` and log where the input
went wrong.

`Sanitize` is the repair path. The one performance requirement is that it must not
allocate when the input is already valid — the common case — so it guards with
`utf8.Valid` and returns the input slice unchanged (same backing array, zero
allocation) when it is well-formed. Only ill-formed input takes the
`bytes.ToValidUTF8` path, which allocates a repaired copy with each ill-formed run
replaced. That guard is the difference between paying an allocation on every request
and paying it only on the rare bad one.

Note on the BOM: the UTF-8 byte-order-mark `EF BB BF` is a *valid* UTF-8 encoding of
`U+FEFF`, so it passes validation. Whether to strip it is a separate,
application-level decision (it is a zero-width no-break space); validation's job is
only well-formedness, and a BOM is well-formed.

Create `utf8guard.go`:

```go
package utf8guard

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"
)

// ErrInvalidUTF8 is the sentinel returned (wrapped) when input is not valid UTF-8.
var ErrInvalidUTF8 = errors.New("invalid utf-8")

// ValidateUTF8 reports the first ill-formed byte offset, if any. It returns a
// wrapped ErrInvalidUTF8 so callers can match with errors.Is.
func ValidateUTF8(b []byte) error {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			return fmt.Errorf("%w: bad byte 0x%02x at offset %d", ErrInvalidUTF8, b[i], i)
		}
		i += size
	}
	return nil
}

// ValidateString is the string-typed convenience for the strict path.
func ValidateString(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: string is not well-formed", ErrInvalidUTF8)
	}
	return nil
}

// Sanitize returns b unchanged (no allocation) when it is already valid UTF-8, and
// otherwise a repaired copy with each ill-formed run replaced by replacement (pass
// []byte("�") for the standard replacement character).
func Sanitize(b, replacement []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	return bytes.ToValidUTF8(b, replacement)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/utf8guard"
)

type sample struct {
	name string
	data []byte
}

func main() {
	repl := []byte("�")

	samples := []sample{
		{"ascii", []byte("hello")},
		{"multibyte", []byte("café 日本語")},
		{"lone-cont", []byte{'a', 0x80, 'b'}},
		{"truncated", []byte{0xE2, 0x82}},
		{"overlong", []byte{0xC0, 0x80}},
	}

	for _, s := range samples {
		err := utf8guard.ValidateUTF8(s.data)
		san := utf8guard.Sanitize(s.data, repl)
		fmt.Printf("%-10s valid=%-5v invalidErr=%-5v sanitized=%q\n",
			s.name, err == nil, errors.Is(err, utf8guard.ErrInvalidUTF8), string(san))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ascii      valid=true  invalidErr=false sanitized="hello"
multibyte  valid=true  invalidErr=false sanitized="café 日本語"
lone-cont  valid=false invalidErr=true  sanitized="a�b"
truncated  valid=false invalidErr=true  sanitized="�"
overlong   valid=false invalidErr=true  sanitized="�"
```

### Tests

The table covers the boundary cases: pure ASCII and valid multibyte pass and are
returned unchanged; a lone continuation byte, a truncated three-byte sequence, and
an overlong encoding are rejected and repaired; the BOM passes as valid.
`TestValidInputZeroAlloc` pins the performance contract — `Sanitize` on valid input
allocates nothing and returns the same backing array. `FuzzValidateUTF8` asserts the
repair invariant: whatever bytes the fuzzer throws at `Sanitize`, its output always
satisfies `utf8.Valid`.

Create `utf8guard_test.go`:

```go
package utf8guard

import (
	"errors"
	"testing"
	"unicode/utf8"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    []byte
		valid bool
	}{
		{"ascii", []byte("hello"), true},
		{"multibyte", []byte("café 日本語"), true},
		{"4-byte", []byte("\U00020000"), true},
		{"bom", []byte{0xEF, 0xBB, 0xBF}, true},
		{"lone continuation", []byte{'a', 0x80, 'b'}, false},
		{"truncated 3-byte", []byte{0xE2, 0x82}, false},
		{"overlong", []byte{0xC0, 0x80}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateUTF8(tc.in)
			if tc.valid && err != nil {
				t.Fatalf("ValidateUTF8(%q) = %v, want nil", tc.in, err)
			}
			if !tc.valid {
				if err == nil {
					t.Fatalf("ValidateUTF8(% x) = nil, want error", tc.in)
				}
				if !errors.Is(err, ErrInvalidUTF8) {
					t.Fatalf("error %v does not wrap ErrInvalidUTF8", err)
				}
			}
			// Sanitize output must always be valid.
			if !utf8.Valid(Sanitize(tc.in, []byte("�"))) {
				t.Fatalf("Sanitize(%q) produced invalid UTF-8", tc.in)
			}
		})
	}
}

func TestValidInputReturnedUnchanged(t *testing.T) {
	t.Parallel()
	in := []byte("café")
	got := Sanitize(in, []byte("�"))
	if &got[0] != &in[0] {
		t.Fatal("valid input was copied; Sanitize should return the same backing array")
	}
}

func TestValidInputZeroAlloc(t *testing.T) {
	// No t.Parallel(): AllocsPerRun must not run under a parallel test.
	in := []byte("a valid ascii request body")
	repl := []byte("�")
	allocs := testing.AllocsPerRun(1000, func() {
		_ = Sanitize(in, repl)
	})
	if allocs != 0 {
		t.Fatalf("Sanitize on valid input allocated %.2f/op; want 0", allocs)
	}
}

func FuzzValidateUTF8(f *testing.F) {
	f.Add([]byte("hello"))
	f.Add([]byte("café"))
	f.Add([]byte{0xC0, 0x80})
	f.Add([]byte{0xE2, 0x82})
	repl := []byte("�")
	f.Fuzz(func(t *testing.T, b []byte) {
		out := Sanitize(b, repl)
		if !utf8.Valid(out) {
			t.Fatalf("Sanitize output not valid UTF-8 for input % x", b)
		}
		// A valid input must be preserved verbatim.
		if utf8.Valid(b) && string(out) != string(b) {
			t.Fatalf("valid input altered: in %q out %q", b, out)
		}
	})
}
```

## Review

Validation is correct when it accepts exactly the well-formed inputs and rejects the
rest, which the table pins across the classic failure modes: a lone continuation
byte, a truncated multibyte sequence, and an overlong encoding are all rejected,
while ASCII, multibyte, a 4-byte rune, and the BOM pass. The `size <= 1` check in
`ValidateUTF8` is the subtle part — without it, a genuine `U+FFFD` in the input would
be misread as invalid.

The performance contract is the operational half: `Sanitize` must not tax the common
case. `TestValidInputReturnedUnchanged` proves it returns the same backing array for
valid input, and `TestValidInputZeroAlloc` proves that path allocates nothing; only
ill-formed input pays for the repair. The fuzz test guarantees the repair invariant
holds for inputs no table could enumerate: whatever comes in, what goes to the store
is valid UTF-8.

## Resources

- [`unicode/utf8`](https://pkg.go.dev/unicode/utf8) — `Valid`, `ValidString`, `DecodeRune`, `RuneError`.
- [`bytes.ToValidUTF8`](https://pkg.go.dev/bytes#ToValidUTF8) / [`strings.ToValidUTF8`](https://pkg.go.dev/strings#ToValidUTF8) — the repair functions.
- [The Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — how UTF-8 sits under Go strings.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-streaming-line-scanner.md](05-streaming-line-scanner.md)
