# Exercise 4: Reject or Repair Invalid UTF-8 Before Indexing

A Go `string` is arbitrary bytes, not guaranteed valid UTF-8. Bytes from an
upload, a proxied body, or a Kafka value can carry a lone continuation byte or a
truncated multi-byte lead, and that invalid sequence poisons JSON re-encoding and
gets a document rejected by a search-engine bulk API. This module builds the guard
stage: a strict validator that rejects with line context, and a lenient repair
transform that replaces invalid sequences with U+FFFD.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
utf8guard/                independent module: example.com/utf8guard
  go.mod                  go 1.26
  utf8guard.go            RepairUTF8 transform; ValidateFields strict guard; ErrInvalidUTF8
  cmd/
    demo/
      main.go             runnable demo over valid and invalid byte sequences
  utf8guard_test.go       hand-built invalid-byte table tests
```

- Files: `utf8guard.go`, `cmd/demo/main.go`, `utf8guard_test.go`.
- Implement: `RepairUTF8(string) string` (a `Transform` using `strings.ToValidUTF8`), `ValidateFields(line int, fields ...string) error` (strict, returns a wrapped `ErrInvalidUTF8` naming the line), and the sentinel `ErrInvalidUTF8`.
- Test: hand-built invalid sequences (lone continuation `0x80`, truncated lead `0xC3`) fail `utf8.ValidString`; strict validate returns an error wrapping `ErrInvalidUTF8` with the line; repair produces valid UTF-8 with U+FFFD; valid input passes through byte-identical.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/utf8guard/cmd/demo
cd ~/go-exercises/utf8guard
go mod init example.com/utf8guard
```

### Reject versus repair, and why validity is a boundary invariant

Downstream systems assume UTF-8. `encoding/json` will not faithfully re-encode
invalid bytes — it substitutes U+FFFD on the way out, so a record you thought you
stored verbatim is silently altered. Elasticsearch and Bleve bulk endpoints reject
a document whose fields contain invalid byte sequences outright. So validity is an
invariant you establish once, at the ingestion boundary, and rely on everywhere
after.

There are two honest policies:

- Strict: reject the record. `utf8.ValidString(s)` returns `false` for any string
  containing an invalid sequence. `ValidateFields` checks each field and, on the
  first invalid one, returns an error wrapping the package sentinel
  `ErrInvalidUTF8` with the line number, so the caller can both locate the bad
  record and match the cause with `errors.Is`. Rejection loses the record but
  never corrupts the index.
- Lenient: repair the record. `strings.ToValidUTF8(s, replacement)` replaces each
  maximal run of invalid bytes with `replacement` — conventionally
  `string(utf8.RuneError)`, the U+FFFD replacement character. Repair keeps the
  stream flowing at the cost of a lossy substitution at the exact positions of the
  bad bytes. Valid input is returned unchanged (byte-identical), so repair is safe
  to apply unconditionally in a lenient pipeline.

The invalid sequences you actually see are worth naming. A lone continuation byte
(`0x80`–`0xBF` with no lead) is a fragment of a multi-byte rune that lost its head
— common when a byte stream is split at the wrong offset. A truncated lead
(`0xC3` expects one continuation byte after it; alone, or followed by a non-
continuation byte, it is invalid) happens when a buffer is cut mid-rune. Both are
exactly what a naive byte-offset truncation elsewhere in the system produces —
which is why the guard belongs at ingestion.

Create `utf8guard.go`:

```go
package utf8guard

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalidUTF8 is the sentinel wrapped when a strict guard rejects a field.
var ErrInvalidUTF8 = errors.New("invalid UTF-8")

// RepairUTF8 is a lenient Transform: it replaces every maximal run of invalid
// bytes with U+FFFD. Valid input is returned unchanged (byte-identical).
func RepairUTF8(s string) string {
	return strings.ToValidUTF8(s, string(utf8.RuneError))
}

// ValidateFields is the strict guard: it returns an error wrapping ErrInvalidUTF8,
// naming the line and the 0-based field index, on the first invalid field.
func ValidateFields(line int, fields ...string) error {
	for i, f := range fields {
		if !utf8.ValidString(f) {
			return fmt.Errorf("line %d: field %d: %w", line, i, ErrInvalidUTF8)
		}
	}
	return nil
}
```

### The runnable demo

The demo builds an invalid string from raw bytes with `string([]byte{...})` so no
control or invalid byte appears literally in the source, then shows both policies.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/utf8guard"
)

func main() {
	// "café" with a lone continuation byte 0x80 spliced in after the 'f'.
	bad := "caf" + string([]byte{0x80}) + "e"

	fmt.Printf("strict reject: %v\n",
		errors.Is(utf8guard.ValidateFields(7, bad), utf8guard.ErrInvalidUTF8))

	repaired := utf8guard.RepairUTF8(bad)
	fmt.Printf("repaired: %q\n", repaired)

	good := "café"
	fmt.Printf("valid passes strict: %v\n", utf8guard.ValidateFields(1, good) == nil)
	fmt.Printf("valid unchanged by repair: %v\n", utf8guard.RepairUTF8(good) == good)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
strict reject: true
repaired: "caf�e"
valid passes strict: true
valid unchanged by repair: true
```

### Tests

The table hand-builds each invalid sequence with `string([]byte{...})` so the test
is explicit about the bytes. `TestValidString` pins the classifier. `TestStrictRejects`
asserts the error wraps `ErrInvalidUTF8` and names the line. `TestRepairProducesValid`
asserts the repaired output is valid UTF-8 and contains U+FFFD. `TestValidPassesThrough`
asserts valid input is returned byte-identical by repair and accepted by strict.

Create `utf8guard_test.go`:

```go
package utf8guard

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    string
		valid bool
	}{
		{"ascii", "hello", true},
		{"multibyte", "café", true},
		{"lone continuation", string([]byte{0x80}), false},
		{"truncated lead", string([]byte{0xC3}), false},
		{"lead then ascii", string([]byte{0xC3, 0x41}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := utf8.ValidString(tt.in); got != tt.valid {
				t.Fatalf("ValidString(% x) = %v, want %v", tt.in, got, tt.valid)
			}
		})
	}
}

func TestStrictRejects(t *testing.T) {
	t.Parallel()

	bad := "caf" + string([]byte{0x80}) + "e"
	err := ValidateFields(42, "clean", bad)
	if err == nil {
		t.Fatal("expected an error for invalid UTF-8")
	}
	if !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("error = %v, want it to wrap ErrInvalidUTF8", err)
	}
	if !strings.Contains(err.Error(), "line 42") {
		t.Fatalf("error = %v, want it to mention line 42", err)
	}
	if !strings.Contains(err.Error(), "field 1") {
		t.Fatalf("error = %v, want it to mention field 1", err)
	}
}

func TestRepairProducesValid(t *testing.T) {
	t.Parallel()

	bad := "a" + string([]byte{0xFF}) + "b" + string([]byte{0xC3}) + "c"
	got := RepairUTF8(bad)
	if !utf8.ValidString(got) {
		t.Fatalf("RepairUTF8 output is still invalid: %q", got)
	}
	if !strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("RepairUTF8 output %q lacks U+FFFD", got)
	}
}

func TestValidPassesThrough(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "ascii", "café", "日本語"} {
		if got := RepairUTF8(s); got != s {
			t.Fatalf("RepairUTF8(%q) = %q, want byte-identical", s, got)
		}
		if err := ValidateFields(1, s); err != nil {
			t.Fatalf("ValidateFields(%q) = %v, want nil", s, err)
		}
	}
}

func ExampleRepairUTF8() {
	bad := "caf" + string([]byte{0x80}) + "e"
	fmt.Printf("%q\n", RepairUTF8(bad))
	// Output: "caf�e"
}
```

## Review

The guard is correct when strict rejection and lenient repair agree on what is
valid: `utf8.ValidString` is the single source of truth, `ValidateFields` wraps
`ErrInvalidUTF8` with locatable context, and `RepairUTF8` returns valid UTF-8 for
any input and the identical string for already-valid input. The trap to avoid is
assuming Go strings are valid UTF-8 and skipping this stage — the invalid bytes
then travel until JSON re-encoding silently substitutes U+FFFD or a bulk index
rejects the batch, and by then you have lost the line context that made the bad
record findable. Choose reject or repair per source and document it; do not
silently do both. Run `go test -race` to confirm the stage is clean.

## Resources

- [unicode/utf8.ValidString and Valid](https://pkg.go.dev/unicode/utf8#ValidString) — the UTF-8 validity classifier.
- [strings.ToValidUTF8](https://pkg.go.dev/strings#ToValidUTF8) — replaces invalid byte runs with a replacement string.
- [unicode/utf8.RuneError](https://pkg.go.dev/unicode/utf8#pkg-constants) — U+FFFD, the replacement character.
- [The Go Blog: strings, bytes, runes and characters](https://go.dev/blog/strings) — why a Go string is not guaranteed valid UTF-8.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-jsonl-streaming-ingester.md](03-jsonl-streaming-ingester.md) | Next: [05-nfc-normalization-for-dedup-keys.md](05-nfc-normalization-for-dedup-keys.md)
