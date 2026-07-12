# Exercise 9: In-Place Secret Redaction in a Mutable Log Buffer

A log line is about to ship to a collector, and it contains a bearer token that must
never leave the host. This exercise builds the scrubber that redacts secrets inside a
`[]byte` log buffer before it is sent — exploiting the one capability a `string`
lacks: mutability. When the replacement is the same length as the secret, the
scrubber overwrites the bytes in place with zero allocation; when lengths differ it
rebuilds via `bytes.ReplaceAll`. That in-place path is the operational payoff of
choosing `[]byte`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
redact/                     independent module: example.com/redact
  go.mod                    go 1.25
  redact.go                 Redact (in-place vs rebuild), MaskInPlace, RedactPattern
  cmd/
    demo/
      main.go               scrubs a log line before shipping
  redact_test.go            in-place identity, variable-length rebuild, pattern, fuzz
```

- Files: `redact.go`, `cmd/demo/main.go`, `redact_test.go`.
- Implement: `Redact([]byte, secret, replacement []byte) []byte` overwriting in place with `copy` when lengths match and rebuilding with `bytes.ReplaceAll` otherwise; `MaskInPlace([]byte, secret []byte, mask byte) int`; and `RedactPattern([]byte, *regexp.Regexp, replacement []byte) []byte`.
- Test: in-place redaction mutates the same backing array (identity via `unsafe.SliceData`) with zero allocation; variable-length redaction rebuilds; non-secret content is untouched; a fuzz test proving no original secret substring survives masking.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Mutability is a tool, not just a hazard

The rest of this lesson treats a string's immutability as a feature and `[]byte`'s
mutability as something to handle carefully. This exercise turns it around: when you
need to modify a buffer *in place* — overwrite a secret before the buffer ships — the
mutability of `[]byte` is exactly the tool, and a string cannot do the job without
allocating a new one. A log buffer already sitting in memory can have its secrets
overwritten with no allocation, which on a high-volume logging path is the difference
between scrubbing being free and scrubbing doubling your log allocations.

`Redact` encodes the decision. When the replacement is the same length as the secret
(masking a token with a same-length placeholder, or a run of `*`), it walks the
buffer with `bytes.Index` and `copy`s the replacement over each occurrence, in place.
No new slice, no allocation, and the returned slice is the same backing array you
passed in — `unsafe.SliceData` confirms the pointer is unchanged. When the lengths
differ, an in-place overwrite is impossible (you would have to shift the tail), so it
falls back to `bytes.ReplaceAll`, which builds a new slice. The caller gets the right
behavior either way; the fast path just happens for free when it can.

`MaskInPlace` is the common convenience: overwrite every occurrence of a secret with
a repeated mask byte (`'*'`), always in place and always zero-allocation, returning
the count redacted. It is the function you reach for to blank a known credential.

`RedactPattern` handles the case where the secret is not a fixed string but a shape —
`Bearer <token>`, an AWS key, a JWT. A compiled `*regexp.Regexp` matches the shape
and `Regexp.ReplaceAll` rebuilds the buffer with each match replaced. Regexp is the
expensive tool (compile it once, at startup, never per line), so it is reserved for
the pattern case; a fixed known secret uses the cheap `MaskInPlace`.

A correctness note that the fuzz test pins: masking with a byte that does not itself
occur in the secret guarantees no occurrence of the secret can survive, even across
the boundaries of what was masked — because a surviving match would have to be made
of unmasked bytes, and the left-to-right scan masks every such run.

Create `redact.go`:

```go
package redact

import (
	"bytes"
	"regexp"
)

// Redact replaces every occurrence of secret in buf with replacement. When the two
// are the same length it overwrites in place with copy (zero allocation) and returns
// the same backing array; otherwise it rebuilds via bytes.ReplaceAll.
func Redact(buf, secret, replacement []byte) []byte {
	if len(secret) == 0 {
		return buf
	}
	if len(replacement) == len(secret) {
		for i := 0; ; {
			j := bytes.Index(buf[i:], secret)
			if j < 0 {
				break
			}
			at := i + j
			copy(buf[at:at+len(secret)], replacement)
			i = at + len(secret)
		}
		return buf
	}
	return bytes.ReplaceAll(buf, secret, replacement)
}

// MaskInPlace overwrites every occurrence of secret in buf with the mask byte,
// in place and without allocating, and returns how many occurrences were masked.
func MaskInPlace(buf, secret []byte, mask byte) int {
	if len(secret) == 0 {
		return 0
	}
	n := 0
	for i := 0; ; {
		j := bytes.Index(buf[i:], secret)
		if j < 0 {
			break
		}
		at := i + j
		for k := at; k < at+len(secret); k++ {
			buf[k] = mask
		}
		n++
		i = at + len(secret)
	}
	return n
}

// RedactPattern replaces every match of re in buf with replacement and returns the
// (rebuilt) result. Compile re once at startup; regexp is too costly per line.
func RedactPattern(buf []byte, re *regexp.Regexp, replacement []byte) []byte {
	return re.ReplaceAll(buf, replacement)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"regexp"

	"example.com/redact"
)

var bearerRE = regexp.MustCompile(`Bearer [A-Za-z0-9._~+/-]+=*`)

func main() {
	line := []byte(`ts=2026-07-02 msg="auth ok" authz="Bearer eyJhbGci.OiJ9.sig" api_key=AKIA1234567890`)

	// Pattern redaction (shape not known verbatim): rebuilds the buffer.
	line = redact.RedactPattern(line, bearerRE, []byte("Bearer [REDACTED]"))

	// Fixed known secret: overwrite in place with a same-length mask, zero alloc.
	n := redact.MaskInPlace(line, []byte("AKIA1234567890"), '*')

	fmt.Printf("masked=%d\n", n)
	fmt.Printf("%s\n", line)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
masked=1
ts=2026-07-02 msg="auth ok" authz="Bearer [REDACTED]" api_key=**************
```

### Tests

`TestRedactInPlace` proves the fast path: an equal-length redaction mutates the same
backing array (checked with `unsafe.SliceData` before and after) and allocates
nothing. `TestRedactRebuilds` proves the fallback: a different-length replacement
returns a new slice with the correct content. `TestNonSecretUntouched` confirms
surrounding bytes are preserved. `FuzzMaskRemovesSecret` asserts the invariant no
table could: for any buffer and any secret not containing the mask byte, masking
leaves no occurrence of the secret behind.

Create `redact_test.go`:

```go
package redact

import (
	"bytes"
	"regexp"
	"testing"
	"unsafe"
)

func TestRedactInPlace(t *testing.T) {
	// No t.Parallel(): AllocsPerRun must not run under a parallel test.
	buf := []byte("token=SECRET42 next=SECRET42 done")
	before := unsafe.SliceData(buf)

	out := Redact(buf, []byte("SECRET42"), []byte("REDACTED"))
	if unsafe.SliceData(out) != before {
		t.Fatal("equal-length redaction did not stay in place (backing array changed)")
	}
	if want := "token=REDACTED next=REDACTED done"; string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}

	allocs := testing.AllocsPerRun(1000, func() {
		b := []byte("k=SECRET42")
		_ = Redact(b, []byte("SECRET42"), []byte("REDACTED"))
	})
	// The only allocation is the fresh []byte literal; Redact itself adds none.
	if allocs > 1 {
		t.Fatalf("in-place Redact allocated %.2f/op beyond the input; want <= 1", allocs)
	}
}

func TestMaskInPlace(t *testing.T) {
	t.Parallel()
	buf := []byte("Authorization: Bearer abc123 abc123")
	n := MaskInPlace(buf, []byte("abc123"), '*')
	if n != 2 {
		t.Fatalf("masked %d occurrences, want 2", n)
	}
	if want := "Authorization: Bearer ****** ******"; string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

func TestRedactRebuilds(t *testing.T) {
	t.Parallel()
	buf := []byte("id=alice, id=bob")
	out := Redact(buf, []byte("id"), []byte("USER"))
	if want := "USER=alice, USER=bob"; string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	// A different-length replacement must NOT reuse the input's storage.
	if unsafe.SliceData(out) == unsafe.SliceData(buf) {
		t.Fatal("variable-length redaction reused the input backing array")
	}
}

func TestNonSecretUntouched(t *testing.T) {
	t.Parallel()
	buf := []byte("prefix SECRET suffix")
	MaskInPlace(buf, []byte("SECRET"), 'X')
	if want := "prefix XXXXXX suffix"; string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

func TestRedactPattern(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(`Bearer [A-Za-z0-9._-]+`)
	buf := []byte("authz=Bearer abc.def.ghi rest")
	out := RedactPattern(buf, re, []byte("Bearer [REDACTED]"))
	if want := "authz=Bearer [REDACTED] rest"; string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func FuzzMaskRemovesSecret(f *testing.F) {
	f.Add([]byte("authorization: Bearer abc123"), []byte("abc123"))
	f.Add([]byte("aaa"), []byte("aa"))
	f.Add([]byte("prefix SECRET SECRET suffix"), []byte("SECRET"))
	f.Fuzz(func(t *testing.T, buf, secret []byte) {
		const mask = '*'
		if len(secret) == 0 || bytes.IndexByte(secret, mask) >= 0 {
			t.Skip()
		}
		cp := append([]byte(nil), buf...)
		MaskInPlace(cp, secret, mask)
		if bytes.Contains(cp, secret) {
			t.Fatalf("secret survived masking: buf=%q secret=%q -> %q", buf, secret, cp)
		}
	})
}
```

## Review

The redactor is correct when secrets are gone and everything else is intact, which
the tests pin from both directions: `TestNonSecretUntouched` for the surrounding
bytes, and `FuzzMaskRemovesSecret` for the guarantee that no occurrence of the secret
survives — a property that holds because a byte the scan masked can never be part of
a later match (the mask byte is not in the secret) and the left-to-right scan reaches
every unmasked occurrence.

The operational lesson is the in-place path. `TestRedactInPlace` proves that an
equal-length redaction mutates the buffer's own backing array and adds no allocation
of its own, which is only possible because `[]byte` is mutable — a `string` would
force a new allocation for every scrub. Reserve the rebuild path (`bytes.ReplaceAll`)
and the regexp path for when the shape forces it: a different-length replacement, or
a secret matched by pattern rather than by exact bytes, with the `Regexp` compiled
once at startup, never per line.

## Resources

- [`bytes.Index` / `bytes.ReplaceAll`](https://pkg.go.dev/bytes#Index) — the search and rebuild primitives.
- [`regexp.Regexp.ReplaceAll`](https://pkg.go.dev/regexp#Regexp.ReplaceAll) — pattern-based redaction on `[]byte`.
- [`unsafe.SliceData`](https://pkg.go.dev/unsafe#SliceData) — the backing-array pointer used to prove in-place mutation.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../03-runes-and-unicode-code-points/00-concepts.md](../03-runes-and-unicode-code-points/00-concepts.md)
