# Exercise 6: AES-GCM Nonce Generation and Constant-Time Tag Verification

At the crypto boundary a fixed-size nonce array flows into slice-based APIs, and a
secret tag must never be compared with `==`. This exercise builds `NewNonce()
([12]byte, error)` that fills a `[12]byte` from `crypto/rand` via the `n[:]`
array-to-slice bridge, and `VerifyTag` that compares tags with
`crypto/subtle.ConstantTimeCompare` instead of `==` to avoid a timing side channel.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
gcmnonce/                    independent module: example.com/gcmnonce
  go.mod
  nonce.go                   NonceSize=12; NewNonce() ([12]byte, error); VerifyTag(got, want []byte) bool
  cmd/
    demo/
      main.go                runnable demo: fresh nonces differ; tag verify true/false
  nonce_test.go              12 bytes, non-degenerate randomness, equal/one-byte-diff/length-mismatch
```

- Files: `nonce.go`, `cmd/demo/main.go`, `nonce_test.go`.
- Implement: `NewNonce() ([12]byte, error)` filling the array from `crypto/rand.Read(n[:])`, and `VerifyTag(got, want []byte) bool` using constant-time comparison.
- Test: a nonce is 12 bytes and two successive nonces differ; `VerifyTag` is true for equal tags, false for a one-byte difference and for a length mismatch.
- Verify: `go test -count=1 -race ./...`

### Why the nonce is an array and the compare is constant-time

An AES-GCM nonce is exactly 12 bytes — the standard GCM nonce size — so `[12]byte`
is the right type: the size is part of the contract, and a value of this type
cannot be the wrong length. But `crypto/rand.Read` takes a `[]byte`, so to fill the
array you bridge it with `n[:]`: that produces a slice header over the array's
storage, `rand.Read` writes the twelve random bytes into that storage, and the
array `n` now holds them. No allocation — the slice just points at the array's
stack bytes. `NewNonce` returns the array by value, so the caller gets an
independent 12-byte nonce.

`crypto/rand.Read` returns `(n int, err error)`; it reads from the operating
system's CSPRNG and, per its contract on modern Go, either fills the buffer
completely or returns an error. You must check that error and propagate it — a
failed read means you have no entropy, and generating a nonce from a partially- or
un-filled buffer would be a serious cryptographic bug.

The tag verification is where the timing side channel lives. A GCM authentication
tag, an HMAC, or any secret-derived MAC must be compared in *constant time*.
`bytes.Equal` and `==` short-circuit on the first differing byte, so their run time
reveals how many leading bytes matched — an attacker who can measure that can forge
a valid tag one byte at a time. `crypto/subtle.ConstantTimeCompare(x, y []byte)
int` returns 1 when the two slices are equal and 0 otherwise, in time that depends
only on the length, not the content. Critically, if the two slices have *different*
lengths it returns 0 immediately (that length difference is not secret), so
`VerifyTag` must still handle mismatched lengths — which `ConstantTimeCompare` does
by returning 0. `crypto/hmac.Equal` is a thin wrapper over the same primitive and
is the idiomatic choice specifically for MAC comparison.

Create `nonce.go`:

```go
package gcmnonce

import (
	"crypto/rand"
	"crypto/subtle"
)

// NonceSize is the standard AES-GCM nonce length in bytes.
const NonceSize = 12

// NewNonce returns a fresh random 12-byte nonce. It fills the fixed-size array
// via crypto/rand.Read over the n[:] slice view, and propagates any RNG error.
func NewNonce() ([NonceSize]byte, error) {
	var n [NonceSize]byte
	if _, err := rand.Read(n[:]); err != nil {
		return [NonceSize]byte{}, err
	}
	return n, nil
}

// VerifyTag reports whether got equals want using a constant-time comparison, so
// its run time does not leak how many leading bytes matched. A length mismatch
// returns false. Never use == or bytes.Equal for secret tags.
func VerifyTag(got, want []byte) bool {
	return subtle.ConstantTimeCompare(got, want) == 1
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/gcmnonce"
)

func main() {
	n1, err := gcmnonce.NewNonce()
	if err != nil {
		panic(err)
	}
	n2, err := gcmnonce.NewNonce()
	if err != nil {
		panic(err)
	}
	fmt.Printf("nonce size: %d bytes\n", len(n1))
	fmt.Printf("fresh nonces differ: %v\n", n1 != n2)

	tag := []byte{0xde, 0xad, 0xbe, 0xef}
	good := []byte{0xde, 0xad, 0xbe, 0xef}
	bad := []byte{0xde, 0xad, 0xbe, 0x00}
	fmt.Printf("verify equal tag: %v\n", gcmnonce.VerifyTag(tag, good))
	fmt.Printf("verify one-byte diff: %v\n", gcmnonce.VerifyTag(tag, bad))
	fmt.Printf("verify length mismatch: %v\n", gcmnonce.VerifyTag(tag, good[:3]))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
nonce size: 12 bytes
fresh nonces differ: true
verify equal tag: true
verify one-byte diff: false
verify length mismatch: false
```

The `fresh nonces differ: true` line is overwhelmingly certain but not a hard
guarantee — two 12-byte CSPRNG draws collide with probability 2^-96, which is why
the test asserts inequality rather than treating it as a law.

### Tests

`TestNonceIsTwelveBytes` asserts the length. `TestNoncesDiffer` draws two nonces
and asserts they are not equal, exercising non-degenerate randomness.
`TestVerifyTag` is a table: equal tags verify true; a one-byte difference verifies
false; a length mismatch verifies false. A comment documents that the comparison
is constant-time, and the length-mismatch case pins the `ConstantTimeCompare`
semantics (mismatched lengths return 0).

Create `nonce_test.go`:

```go
package gcmnonce

import (
	"testing"
)

func TestNonceIsTwelveBytes(t *testing.T) {
	t.Parallel()

	n, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	if len(n) != NonceSize {
		t.Fatalf("nonce length = %d, want %d", len(n), NonceSize)
	}
}

func TestNoncesDiffer(t *testing.T) {
	t.Parallel()

	a, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	b, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	if a == b {
		t.Fatal("two fresh nonces must differ (collision probability 2^-96)")
	}
}

func TestVerifyTag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		got, want []byte
		ok        bool
	}{
		{"equal", []byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}, true},
		{"one-byte diff", []byte{1, 2, 3, 4}, []byte{1, 2, 3, 5}, false},
		{"length mismatch short", []byte{1, 2, 3, 4}, []byte{1, 2, 3}, false},
		{"length mismatch long", []byte{1, 2, 3}, []byte{1, 2, 3, 4}, false},
		{"both empty", []byte{}, []byte{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// VerifyTag uses subtle.ConstantTimeCompare, so its timing does not
			// leak the position of the first differing byte.
			if got := VerifyTag(tc.got, tc.want); got != tc.ok {
				t.Fatalf("VerifyTag(%v, %v) = %v, want %v", tc.got, tc.want, got, tc.ok)
			}
		})
	}
}
```

## Review

The nonce path is correct when `NewNonce` returns a full 12-byte array filled from
`crypto/rand` with the error checked, and when two draws differ. The `n[:]` bridge
is the array-to-slice mechanic at the crypto boundary: a slice-consuming RNG fills
a fixed-size array in place with no allocation, and the array is returned by value
for isolation. The verification path is correct when `VerifyTag` is true only for
byte-identical tags of equal length — and, crucially, uses constant-time comparison
so it does not leak timing. The mistake this exercise exists to prevent: comparing a
secret tag with `==` or `bytes.Equal`, either of which short-circuits and opens a
byte-by-byte forgery channel. Run `go test -race` to confirm all cases; note the
length-mismatch rows pinning that `ConstantTimeCompare` returns 0 for unequal
lengths.

## Resources

- [crypto/rand](https://pkg.go.dev/crypto/rand) — `Read` fills a buffer from the OS CSPRNG or returns an error.
- [crypto/subtle](https://pkg.go.dev/crypto/subtle#ConstantTimeCompare) — `ConstantTimeCompare` returns 1 for equal, 0 otherwise, in constant time.
- [crypto/hmac Equal](https://pkg.go.dev/crypto/hmac#Equal) — the idiomatic constant-time MAC comparison.
- [crypto/cipher NewGCM](https://pkg.go.dev/crypto/cipher#NewGCM) — where the 12-byte nonce and 16-byte tag come from.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-in-place-xor-whitening-pointer.md](05-in-place-xor-whitening-pointer.md) | Next: [07-l2-device-table-mac-key.md](07-l2-device-table-mac-key.md)
