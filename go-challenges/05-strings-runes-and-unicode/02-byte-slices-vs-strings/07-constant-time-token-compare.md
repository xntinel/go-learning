# Exercise 7: Constant-Time API Token and HMAC Comparison

An auth layer verifies a presented API token against the expected one, and a
recomputed HMAC against the tag on a signed request. Comparing either with `==` or
`bytes.Equal` short-circuits on the first differing byte, leaking through timing how
many leading bytes were correct. This exercise builds the verifier the right way,
with `crypto/subtle.ConstantTimeCompare` and `crypto/hmac.Equal` on `[]byte` — tying
the byte-slice choice to a security property.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
tokenauth/                  independent module: example.com/tokenauth
  go.mod                    go 1.25
  tokenauth.go              VerifyToken (subtle), SignRequest + VerifyRequest (hmac)
  cmd/
    demo/
      main.go               signs and verifies a request, checks a token
  tokenauth_test.go         accept/reject cases, tamper detection, length mismatch
```

- Files: `tokenauth.go`, `cmd/demo/main.go`, `tokenauth_test.go`.
- Implement: `VerifyToken(presented, expected []byte) bool` using `subtle.ConstantTimeCompare`; `SignRequest(key, msg []byte) []byte` using HMAC-SHA256; and `VerifyRequest(key, msg, tag []byte) bool` using `hmac.Equal`.
- Test: equal tokens accepted, one-byte-different rejected, length mismatch rejected; a known message/key verifies against a precomputed tag and a tampered tag is rejected.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why a byte-by-byte comparison is a vulnerability

`==` on strings and `bytes.Equal` on slices both return the moment they find a
mismatch. That is exactly what you want for performance and exactly what you must not
have for a secret: the time the comparison takes reveals how many leading bytes
matched. An attacker who can measure response time submits guesses, keeps the ones
that take microscopically longer (one more byte matched), and recovers the secret one
byte at a time — a timing side channel, and a practical one against network services
with enough samples.

The fix is a comparison whose running time does not depend on *where* the inputs
differ. `crypto/subtle.ConstantTimeCompare(x, y)` returns 1 if the two byte slices
are equal and 0 otherwise, ORing together the XOR of every byte pair so it always
examines all bytes — no early return. It does return 0 immediately when the lengths
differ, but a length mismatch is generally safe to reveal (token length is not the
secret), and for fixed-length tokens and MAC tags the lengths always match anyway.
It operates on `[]byte`, which is one more reason the auth layer holds tokens as
bytes rather than strings.

`hmac.Equal(mac1, mac2)` is the same idea wrapped for MAC tags — it calls
`subtle.ConstantTimeCompare` under the hood. `SignRequest` computes an HMAC-SHA256
over the message with a secret key (`hmac.New(sha256.New, key)`), and
`VerifyRequest` recomputes the tag and compares it to the presented one with
`hmac.Equal`. Recomputing and comparing in constant time is how you authenticate a
signed webhook or an internal RPC without leaking the tag.

One subtlety worth stating: constant-time comparison protects the *comparison*, but
the whole verify path should avoid data-dependent branches on the secret. Here the
only secret-dependent operation is the compare itself, which is constant-time, so the
path is sound.

Create `tokenauth.go`:

```go
package tokenauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
)

// VerifyToken reports whether presented equals expected, comparing in constant
// time so the result does not leak (via timing) how many leading bytes matched.
// A length mismatch returns false (and is safe to reveal: length is not secret).
func VerifyToken(presented, expected []byte) bool {
	return subtle.ConstantTimeCompare(presented, expected) == 1
}

// SignRequest returns the HMAC-SHA256 tag of msg under key.
func SignRequest(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}

// VerifyRequest recomputes the HMAC-SHA256 tag of msg under key and compares it to
// tag in constant time via hmac.Equal.
func VerifyRequest(key, msg, tag []byte) bool {
	want := SignRequest(key, msg)
	return hmac.Equal(want, tag)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/hex"
	"fmt"

	"example.com/tokenauth"
)

func main() {
	key := []byte("shared-secret-key")
	msg := []byte(`{"event":"payment.succeeded","id":"evt_123"}`)

	tag := tokenauth.SignRequest(key, msg)
	fmt.Printf("tag=%s\n", hex.EncodeToString(tag))
	fmt.Printf("verify good tag: %v\n", tokenauth.VerifyRequest(key, msg, tag))

	tampered := make([]byte, len(tag))
	copy(tampered, tag)
	tampered[0] ^= 0xFF
	fmt.Printf("verify tampered tag: %v\n", tokenauth.VerifyRequest(key, msg, tampered))

	expected := []byte("api-token-abc123")
	fmt.Printf("verify correct token: %v\n", tokenauth.VerifyToken([]byte("api-token-abc123"), expected))
	fmt.Printf("verify wrong token: %v\n", tokenauth.VerifyToken([]byte("api-token-xxxxxx"), expected))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tag=8e7c59cd32fdd21e5f21d2b7d868c57ea50fbd3d190559f3dbf5d6a63348d2e2
verify good tag: true
verify tampered tag: false
verify correct token: true
verify wrong token: false
```

The `tag` line is the real HMAC-SHA256 of that exact message and key; your run
prints the same hex.

### Tests

`TestVerifyToken` covers equal (accepted), one-byte-different (rejected), and
length-mismatch (rejected) — the three cases that matter, with the last confirming
`ConstantTimeCompare` returns 0 for unequal lengths. `TestVerifyRequest` signs a
known message and verifies it, then flips a byte of the tag and confirms rejection.
`TestKnownTag` pins the HMAC against a precomputed hex value, so a regression in the
signing path is caught, not silently accepted.

Create `tokenauth_test.go`:

```go
package tokenauth

import (
	"encoding/hex"
	"testing"
)

func TestVerifyToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                string
		presented, expected []byte
		want                bool
	}{
		{"equal", []byte("secret-token"), []byte("secret-token"), true},
		{"one byte different", []byte("secret-tokdn"), []byte("secret-token"), false},
		{"length mismatch", []byte("secret-tok"), []byte("secret-token"), false},
		{"empty equal", []byte(""), []byte(""), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := VerifyToken(tc.presented, tc.expected); got != tc.want {
				t.Fatalf("VerifyToken = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVerifyRequest(t *testing.T) {
	t.Parallel()
	key := []byte("k")
	msg := []byte("hello world")
	tag := SignRequest(key, msg)

	if !VerifyRequest(key, msg, tag) {
		t.Fatal("valid tag rejected")
	}

	tampered := append([]byte(nil), tag...)
	tampered[len(tampered)-1] ^= 0x01
	if VerifyRequest(key, msg, tampered) {
		t.Fatal("tampered tag accepted")
	}

	if VerifyRequest([]byte("wrong-key"), msg, tag) {
		t.Fatal("tag verified under the wrong key")
	}
}

func TestKnownTag(t *testing.T) {
	t.Parallel()
	// HMAC-SHA256("hello world") under key "k", precomputed and fixed. Pinning the
	// exact tag catches any regression in the signing path.
	got := hex.EncodeToString(SignRequest([]byte("k"), []byte("hello world")))
	const want = "67eedc5d50852aacd055cc940b52edde89eba69b15902b2a9a82483eab70d12d"
	if got != want {
		t.Fatalf("SignRequest tag = %s, want %s", got, want)
	}
	if len(SignRequest([]byte("k"), []byte("hello world"))) != 32 {
		t.Fatal("HMAC-SHA256 tag must be 32 bytes")
	}
}
```

## Review

The verifier is correct when equal secrets are accepted, unequal ones rejected, and
a length mismatch returns false — all pinned by the table. The security property is
the reason for the exercise: `subtle.ConstantTimeCompare` and `hmac.Equal` examine
every byte and never short-circuit, so the verify time carries no information about
where the inputs diverged. Using `==` or `bytes.Equal` here would pass every
functional test and still be the vulnerability.

The HMAC path adds authentication: `VerifyRequest` recomputes the tag and rejects a
tampered one or a wrong key, which `TestVerifyRequest` confirms. In production, hold
tokens and tags as `[]byte` end to end so you never convert a secret to a `string`
(strings linger in memory and interning can duplicate them), and always compare with
the constant-time primitives.

## Resources

- [`crypto/subtle.ConstantTimeCompare`](https://pkg.go.dev/crypto/subtle#ConstantTimeCompare) — the constant-time byte comparison.
- [`crypto/hmac`](https://pkg.go.dev/crypto/hmac) — `New`, `Equal`, and the MAC construction.
- [`crypto/sha256`](https://pkg.go.dev/crypto/sha256) — the hash used under the HMAC.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-append-encoder-hot-path.md](08-append-encoder-hot-path.md)
