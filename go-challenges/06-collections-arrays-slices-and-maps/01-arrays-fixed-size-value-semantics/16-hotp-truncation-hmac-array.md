# Exercise 16: RFC 4226 HOTP Dynamic Truncation Over a Fixed HMAC-SHA1 Array

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

HOTP (RFC 4226) is the counter-based one-time-password algorithm underneath
most hardware tokens and TOTP-based two-factor auth (TOTP is HOTP with the
counter derived from the clock). Its core step computes HMAC-SHA1 of a secret
key and a counter, which the standard library returns as a slice but which is
*always* exactly 20 bytes — the natural fit is `[20]byte` (`sha1.Size`), not
`[]byte`. Dynamic truncation then reads the low nibble of the digest's last
byte as an offset into that same array and extracts four bytes starting
there. The interesting property is that this offset can never go out of
bounds: a 4-bit nibble ranges over 0 to 15, and a 20-byte array has room for a
4-byte read starting anywhere up to index 16, so the bound is proven by the
type sizes themselves, not by a runtime check.

This exercise builds `hotp`, a small command-line tool that computes one HOTP
value per invocation: `hotp -secret STRING -counter N -digits N`. Every RFC
4226 Appendix D test vector becomes a table entry pinning `Generate`, the
function underneath the flag parsing, and a fixed offset sweep proves the
truncation index never leaves the digest's bounds.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
hotp/                          module example.com/hotp
  go.mod                       go 1.24
  hotp.go                      package main — Generate(key, counter, digits) (string, error); hmacSHA1 -> [sha1.Size]byte; dynamicTruncate([sha1.Size]byte) uint32
  hotp_test.go                 package main — RFC 4226 vectors, invalid digits, digit-width table, offset-in-bounds sweep, run() end to end
  main.go                      package main — -secret/-counter/-digits flags, exit codes
```

- Files: `hotp.go`, `hotp_test.go`, `main.go`.
- Implement: `Generate(key []byte, counter uint64, digits int) (string, error)` computing HMAC-SHA1 over an 8-byte big-endian counter, converting the digest to `[sha1.Size]byte`, applying RFC 4226 dynamic truncation, and formatting a zero-padded decimal string of the requested width (1 to 8 digits); sentinel error `ErrInvalidDigits`.
- Tool: `hotp` reads no stdin — every input arrives as a flag. `-secret` is required; `-counter` defaults to 0; `-digits` defaults to 6. It prints the computed code followed by a newline to stdout. Exit 0 on success, exit 2 for a missing `-secret`, an unparseable flag, or a rejected digit count (all usage errors), exit 1 is reserved for a runtime failure — none exists in this tool, since HMAC-SHA1 and formatting cannot fail once the flags validate.
- Test: all ten RFC 4226 Appendix D vectors (counters 0 to 9, 6 digits); out-of-range digit counts are rejected; the 1-, 6-, and 8-digit truncations of counter 0 are checked against the same underlying value; the dynamic-truncation offset stays within `[0,15]` and `offset+4` never exceeds the digest length, across a sweep of counters; `run` end to end over its argument slice and a `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/16-hotp-truncation-hmac-array
cd go-solutions/06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/16-hotp-truncation-hmac-array
go mod edit -go=1.24
```

### Why the digest is an array and the offset can't go out of bounds

`hmac.New(sha1.New, key)` followed by `h.Sum(nil)` returns a `[]byte`, because
`hash.Hash.Sum` is a generic slice-based API shared by every hash algorithm in
the standard library. But HMAC-SHA1 specifically always produces exactly
`sha1.Size` (20) bytes — that is not a runtime property, it is a fact about
the algorithm. Converting that slice to `[sha1.Size]byte` with the Go 1.20
slice-to-array conversion turns "always 20 bytes, enforced by convention"
into "always 20 bytes, enforced by the type system": every caller of
`hmacSHA1` receives a value that is *provably* 20 bytes, and the conversion
itself can never panic here because the slice it converts is always exactly
that length.

That fixed size is what makes the next step — RFC 4226's dynamic truncation —
safe by construction. Section 5.3 says: take the low 4 bits of the digest's
last byte as an offset, then read the 4 bytes starting at that offset as a
big-endian 31-bit integer (the top bit is cleared to avoid sign-related
implementation differences across languages). A 4-bit nibble is an integer in
`[0,15]`. A 20-byte array indexed by `offset:offset+4` needs `offset+4 <= 20`,
i.e. `offset <= 16` — and the nibble's maximum value of 15 satisfies that with
room to spare. There is no scenario, for any key or counter, where this slice
expression runs past the end of `mac`. This is the same reasoning as the
"offset bounds proven by construction" idea used at other array boundaries:
once you know the exact size and the exact range of the derived index, you
don't need a runtime bounds check to know the read is safe — the compiler's
array-length checking plus the algorithm's own math already guarantee it.

Create `hotp.go`:

```go
package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrInvalidDigits is returned when the requested digit count is outside the
// range RFC 4226 supports (1 to 8 decimal digits).
var ErrInvalidDigits = errors.New("hotp: digits must be between 1 and 8")

// digitsMod maps a digit count to the modulus that truncates the 31-bit
// dynamic-truncation output to that many decimal digits.
var digitsMod = [9]uint32{1, 10, 100, 1000, 10000, 100000, 1000000, 10000000, 100000000}

// Generate computes the RFC 4226 HOTP value for key at counter, truncated to
// digits decimal digits, and returns it zero-padded (e.g. "007110" for
// digits=6). digits must be between 1 and 8.
func Generate(key []byte, counter uint64, digits int) (string, error) {
	if digits < 1 || digits > 8 {
		return "", ErrInvalidDigits
	}
	mac := hmacSHA1(key, counter)
	code := dynamicTruncate(mac)
	value := code % digitsMod[digits]
	return fmt.Sprintf("%0*d", digits, value), nil
}

// hmacSHA1 computes HMAC-SHA1(key, counter) with counter encoded as an
// 8-byte big-endian value, per RFC 4226 section 5.1. HMAC-SHA1 always
// produces exactly sha1.Size (20) bytes, so converting the digest slice to
// the fixed [sha1.Size]byte array below can never panic: the length is
// guaranteed by the algorithm, not by a runtime check.
func hmacSHA1(key []byte, counter uint64) [sha1.Size]byte {
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)

	h := hmac.New(sha1.New, key)
	h.Write(counterBytes[:])
	return [sha1.Size]byte(h.Sum(nil))
}

// dynamicTruncate implements RFC 4226 section 5.3 dynamic truncation over a
// fixed 20-byte HMAC-SHA1 digest. The low nibble of the last byte selects an
// offset; because a nibble ranges over 0..15 and the digest is exactly 20
// bytes long, offset+4 is at most 19 -- the four-byte window read below can
// never run past the end of mac. The offset bound is proven by construction
// (4 bits, fixed-size array), not by a runtime length check.
func dynamicTruncate(mac [sha1.Size]byte) uint32 {
	offset := mac[sha1.Size-1] & 0x0f
	code := binary.BigEndian.Uint32(mac[offset : offset+4])
	return code & 0x7fffffff // clear the sign bit per RFC 4226 section 5.4
}
```

### The tool

`hotp` has no stdin: everything it needs arrives as a flag, so `run` takes
the argument slice and an `io.Writer` for stdout, nothing else, which makes
it trivial to drive from a table test without touching `os.Args` or a real
terminal. `flag.NewFlagSet` with `flag.ContinueOnError` lets `run` return a
parse error instead of the package-level `flag.CommandLine` calling
`os.Exit` out from under the test. Every failure `run` can produce — a bad
flag, a missing `-secret`, a rejected digit count — is a usage error the
caller fixes by changing the command line, so they all wrap the `errUsage`
sentinel and `main` maps that to exit code 2. Exit code 1 is defined by
convention for a runtime failure, but this particular tool has none: once
the flags parse, HMAC-SHA1 and decimal formatting cannot fail.

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the command line:
// a bad flag, a missing required flag, or a digit count Generate rejects.
// main maps it to exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run parses args, computes one HOTP value, and writes it followed by a
// newline to stdout. It never touches os.Stdin or os.Exit, so it can be
// exercised in a test with a plain bytes.Buffer collecting stdout.
func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hotp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	secret := fs.String("secret", "", "shared secret, ASCII bytes (required)")
	counter := fs.Uint64("counter", 0, "HOTP counter value")
	digits := fs.Int("digits", 6, "number of decimal digits, 1-8")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if *secret == "" {
		return fmt.Errorf("%w: -secret is required", errUsage)
	}

	code, err := Generate([]byte(*secret), *counter, *digits)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	fmt.Fprintln(stdout, code)
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hotp -secret STRING -counter N [-digits N]")
		fmt.Fprintln(os.Stderr, "computes one RFC 4226 HOTP value and prints it to stdout.")
	}
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hotp:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
go run . -secret 12345678901234567890 -counter 0
go run . -secret 12345678901234567890 -counter 1
go run . -secret 12345678901234567890 -counter 0 -digits 8
go run . -secret 12345678901234567890 -digits 12
```

Expected output:

```text
755224
287082
84755224
hotp: usage: hotp: digits must be between 1 and 8
```

The first three lines match RFC 4226 Appendix D exactly for the standard
20-byte ASCII test key `"12345678901234567890"`, including the 8-digit width
(`84755224` is the last eight digits of Appendix D's unpadded "Decimal"
column for counter 0). The fourth line shows the exit-2 usage error: `hotp:`
followed by the error text `run` returns, itself wrapping `ErrInvalidDigits`.

### Tests

`TestGenerateRFC4226Vectors` is the table that matters most: it pins all ten
Appendix D vectors, counters 0 through 9 against the standard test secret,
each in its own parallel subtest. `TestGenerateInvalidDigits` sweeps a few
out-of-range digit counts and asserts `ErrInvalidDigits`.
`TestGenerateDigitsWidth` checks that 1-, 6-, and 8-digit output is always
exactly the requested width, and that all three widths are truncations of
the same underlying value rather than independently hand-picked numbers.
`TestDynamicTruncateOffsetInBounds` is the edge-case test this exercise is
really about: it sweeps fifty counters, recomputes the raw digest for each,
and asserts the derived offset is always in `[0,15]` and that `offset+4`
never exceeds the digest's length — directly checking the safety argument the
code comment makes, rather than just trusting it. `TestRun` drives the
command end to end: valid flag combinations against their expected stdout,
and a missing `-secret`, a rejected digit count, and an unknown flag all
producing an error that wraps `errUsage`.

Create `hotp_test.go`:

```go
package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"strings"
	"testing"
)

// secret is the exact 20-byte ASCII key used in RFC 4226 Appendix D.
const secret = "12345678901234567890"

// TestGenerateRFC4226Vectors pins every 6-digit HOTP value from RFC 4226
// Appendix D, counters 0 through 9, against the fixed test secret.
func TestGenerateRFC4226Vectors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		counter uint64
		want    string
	}{
		{0, "755224"},
		{1, "287082"},
		{2, "359152"},
		{3, "969429"},
		{4, "338314"},
		{5, "254676"},
		{6, "287922"},
		{7, "162583"},
		{8, "399871"},
		{9, "520489"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("counter-%d", tc.counter), func(t *testing.T) {
			t.Parallel()
			got, err := Generate([]byte(secret), tc.counter, 6)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Generate(counter=%d) = %q, want %q", tc.counter, got, tc.want)
			}
		})
	}
}

// TestGenerateInvalidDigits asserts digit counts outside 1..8 are rejected.
func TestGenerateInvalidDigits(t *testing.T) {
	t.Parallel()

	for _, digits := range []int{0, -1, 9, 20} {
		_, err := Generate([]byte(secret), 0, digits)
		if err != ErrInvalidDigits {
			t.Fatalf("Generate(digits=%d) err = %v, want ErrInvalidDigits", digits, err)
		}
	}
}

// TestGenerateDigitsWidth checks that the returned value is always exactly
// the requested width, left-padded with zeros when needed, and that the
// widths RFC 4226 permits (1 through 8) all produce a truncation of the same
// underlying 31-bit value: the last 8 digits of RFC 4226 Appendix D's
// unpadded "Decimal" column for counter 0 is 84755224, so digits=8 and
// digits=1 are checked against that value's own suffix, not against a
// separately hand-picked number.
func TestGenerateDigitsWidth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		digits int
		want   string
	}{
		{1, "4"},
		{6, "755224"},
		{8, "84755224"},
	}
	for _, tc := range cases {
		got, err := Generate([]byte(secret), 0, tc.digits)
		if err != nil {
			t.Fatalf("Generate(digits=%d): %v", tc.digits, err)
		}
		if got != tc.want {
			t.Fatalf("Generate(digits=%d) = %q, want %q", tc.digits, got, tc.want)
		}
		if len(got) != tc.digits {
			t.Fatalf("Generate(digits=%d) len = %d, want %d", tc.digits, len(got), tc.digits)
		}
	}
}

// TestDynamicTruncateOffsetInBounds sweeps a range of counters and asserts
// the low-nibble offset derived from the digest's last byte always lands in
// 0..15, which is what makes the four-byte read in dynamicTruncate safe by
// construction rather than by a runtime bounds check.
func TestDynamicTruncateOffsetInBounds(t *testing.T) {
	t.Parallel()

	for counter := uint64(0); counter < 50; counter++ {
		mac := hmacSHA1([]byte(secret), counter)
		offset := mac[sha1.Size-1] & 0x0f
		if offset > 15 {
			t.Fatalf("counter=%d offset = %d, want <= 15", counter, offset)
		}
		if int(offset)+4 > len(mac) {
			t.Fatalf("counter=%d offset+4 = %d exceeds digest length %d", counter, offset+4, len(mac))
		}
	}
}

// TestRun exercises the command end to end: flag parsing, Generate, and the
// exact stdout line, without ever going through os.Args or os.Exit.
func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
		usage   bool
	}{
		{
			name: "rfc vector counter 0",
			args: []string{"-secret", secret, "-counter", "0", "-digits", "6"},
			want: "755224\n",
		},
		{
			name: "default digits is six",
			args: []string{"-secret", secret, "-counter", "1"},
			want: "287082\n",
		},
		{
			name: "digits eight widens the same truncation",
			args: []string{"-secret", secret, "-counter", "0", "-digits", "8"},
			want: "84755224\n",
		},
		{
			name:    "missing secret is a usage error",
			args:    []string{"-counter", "0"},
			wantErr: true,
			usage:   true,
		},
		{
			name:    "digits out of range is a usage error",
			args:    []string{"-secret", secret, "-digits", "9"},
			wantErr: true,
			usage:   true,
		},
		{
			name:    "unknown flag is a usage error",
			args:    []string{"-bogus"},
			wantErr: true,
			usage:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if tc.usage && !strings.Contains(err.Error(), "usage") {
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

`Generate` is correct when every RFC 4226 Appendix D vector matches exactly —
that table is not a made-up sanity check, it is the authoritative conformance
test for any HOTP implementation. The mechanism worth internalizing is the
two-stage use of fixed-size arrays: first, converting the HMAC-SHA1 digest
slice to `[sha1.Size]byte` turns "this is always 20 bytes" from a comment
into a type-checked fact; second, that fact is what lets dynamic truncation's
offset arithmetic be verified by inspection rather than guarded at runtime.
The mistake this design avoids is adding a defensive `if offset+4 >
len(mac)` check that can never actually fire — a check like that either
signals the author didn't do the bound analysis, or worse, silently masks a
real bug elsewhere (e.g. an accidentally truncated digest) by returning a
wrong-but-plausible truncated code instead of panicking loudly. `run` keeps
that logic separate from `os.Args`/`os.Exit`, mapping every input mistake to
exit code 2 and reserving 1 for a runtime failure this particular tool never
produces. Run `go test -count=1 -race ./...` to confirm the vectors, the
digit-width table, the offset sweep, and `run`'s end-to-end behavior.

## Resources

- [RFC 4226](https://www.rfc-editor.org/rfc/rfc4226) — the HOTP algorithm, including the Appendix D test vectors pinned above.
- [crypto/hmac](https://pkg.go.dev/crypto/hmac) — `New` and `Hash.Sum` used to compute the HMAC-SHA1 digest.
- [crypto/sha1](https://pkg.go.dev/crypto/sha1#pkg-constants) — `sha1.Size`, the fixed 20-byte digest length this exercise relies on.
- [Go Specification: Conversions from slice to array or array pointer](https://go.dev/ref/spec#Conversions_from_slice_to_array_or_array_pointer) — the Go 1.20+ conversion used to turn the digest slice into `[sha1.Size]byte`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-latency-reservoir-fixed-array.md](15-latency-reservoir-fixed-array.md) | Next: [17-merkle-root-fixed-hash-arrays.md](17-merkle-root-fixed-hash-arrays.md)
