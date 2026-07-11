# Exercise 2: Cipher-Agile AEAD Envelope: AES-GCM and XChaCha20-Poly1305

A ciphertext you write today will outlive today's algorithm choice. This
exercise builds a self-describing, versioned envelope: a one-byte algorithm
identifier is the first byte of every blob, so `Open` can read the header and
dispatch to the right decryptor. That single seam turns a cipher migration from
a flag-day rewrite into a lazy, per-record upgrade — and it is the same seam the
next lesson's key-wrapping plugs into.

This module is fully self-contained and imports `golang.org/x/crypto`, so gate it
with `GOFLAGS=-mod=mod`.

## What you'll build

```text
aeadenvelope/              independent module: example.com/aeadenvelope
  go.mod                   go 1.24; requires golang.org/x/crypto
  envelope.go             Seal(alg, key, pt, aad); Open(key, env, aad); AlgName
  cmd/
    demo/
      main.go             seal one secret under both ciphers; rewrite header
  envelope_test.go        round-trip both algs; header byte; unknown/short; header-swap
```

- Files: `envelope.go`, `cmd/demo/main.go`, `envelope_test.go`.
- Implement: `Seal(alg byte, key, plaintext, aad []byte)` dispatching to AES-256-GCM via `cipher.NewGCMWithRandomNonce` or XChaCha20-Poly1305 via `chacha20poly1305.NewX`, and `Open(key, envelope, aad []byte) (plaintext []byte, alg byte, err error)` that reads the header byte and selects the matching decryptor.
- Test: round-trip both algorithm ids through one API; the first byte equals the requested id; an unknown id fails; a truncated/empty envelope fails without panicking; an AES-GCM envelope whose header is rewritten to the XChaCha id fails to open.
- Verify: `GOFLAGS=-mod=mod go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/aeadenvelope/cmd/demo
cd ~/go-exercises/aeadenvelope
go mod init example.com/aeadenvelope
go mod edit -go=1.24
go get golang.org/x/crypto/chacha20poly1305
```

### Two ciphers, two nonce disciplines, one envelope

The point of this exercise is that the two algorithms manage their nonces
differently, and the envelope hides that difference behind a single API.

AES-256-GCM via `cipher.NewGCMWithRandomNonce` (Go 1.24) is the modern,
misuse-resistant form: `NonceSize()` is zero, `Seal` generates a fresh random
96-bit nonce and prepends it to the ciphertext itself, `Open` extracts it, and
`Overhead()` is 28 (the 12-byte nonce plus the 16-byte tag). You never touch a
nonce. The library also enforces the hard 2^32-messages-per-key limit that the
birthday bound requires.

XChaCha20-Poly1305 via `chacha20poly1305.NewX` uses a 24-byte nonce
(`NonceSizeX`). That length is the whole reason to reach for the X variant: 192
bits is large enough that a randomly generated nonce practically never collides,
so it is the right default whenever nonces are random. Unlike the random-nonce
GCM constructor, `NewX` does not generate the nonce for you — its `NonceSize()`
is 24 and you must supply one.

The envelope unifies these by using each AEAD's own `NonceSize()`. When it is
zero (random-nonce GCM), the code generates no nonce and lets `Seal` prepend its
own; when it is 24 (XChaCha), the code draws 24 random bytes and writes them into
the envelope right after the header. Either way the layout is
`[1-byte alg][nonce (0 or 24 bytes)][AEAD output]`, and `Open` reconstructs the
split from the algorithm's `NonceSize()`.

### Binding the header so it cannot be rewritten

The header byte is not just a hint; it is part of the security boundary. If an
attacker could flip the algorithm id and have `Open` accept the blob under a
different cipher, the format would be malleable. The defense is to fold the
header byte into the associated data passed to the AEAD: the bytes actually
authenticated are `alg || aad`. Rewriting the header therefore changes the AAD
that `Open` reconstructs, and authentication fails. (It would fail anyway,
because a different algorithm id dispatches to a decryptor that cannot make sense
of the bytes — but binding the header as AAD makes the rejection principled
rather than incidental, and it also authenticates the header within a single
algorithm.)

### The dispatch table

`aeadFor` maps an algorithm id to a constructed `cipher.AEAD`. Both ciphers here
take a 32-byte key: AES-256 needs 32 bytes, and `chacha20poly1305` requires a
256-bit key (`chacha20poly1305.KeySize` is 32). An unknown id returns
`ErrUnknownAlg`, which is how `Open` rejects a corrupted or forged header.

Create `envelope.go`:

```go
package aeadenvelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// Algorithm identifiers. The first byte of every envelope is one of these.
const (
	AlgAESGCM  byte = 1 // AES-256-GCM with a library-managed random nonce
	AlgXChaCha byte = 2 // XChaCha20-Poly1305 with a 24-byte random nonce
)

// KeySize is the key length both ciphers require.
const KeySize = 32

var (
	// ErrUnknownAlg is returned when the header byte names no known cipher.
	ErrUnknownAlg = errors.New("aeadenvelope: unknown algorithm id")
	// ErrShortEnvelope is returned when the envelope is too small to hold a
	// header and nonce.
	ErrShortEnvelope = errors.New("aeadenvelope: envelope too short")
	// ErrOpen is returned when authentication fails.
	ErrOpen = errors.New("aeadenvelope: authentication failed")
)

// AlgName returns a human-readable cipher name, or "unknown".
func AlgName(alg byte) string {
	switch alg {
	case AlgAESGCM:
		return "AES-256-GCM"
	case AlgXChaCha:
		return "XChaCha20-Poly1305"
	default:
		return "unknown"
	}
}

func aeadFor(alg byte, key []byte) (cipher.AEAD, error) {
	switch alg {
	case AlgAESGCM:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCMWithRandomNonce(block)
	case AlgXChaCha:
		return chacha20poly1305.NewX(key)
	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownAlg, alg)
	}
}

// bindHeader returns alg || aad, the bytes authenticated as associated data.
func bindHeader(alg byte, aad []byte) []byte {
	bound := make([]byte, 0, 1+len(aad))
	bound = append(bound, alg)
	return append(bound, aad...)
}

// Seal encrypts plaintext under alg, producing a self-describing envelope
// [alg][nonce][ciphertext+tag]. The header byte and aad are both authenticated.
func Seal(alg byte, key, plaintext, aad []byte) ([]byte, error) {
	aead, err := aeadFor(alg, key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if len(nonce) > 0 {
		// XChaCha needs an explicit 24-byte nonce; random-nonce GCM has
		// NonceSize()==0 and prepends its own nonce inside Seal.
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
	}
	env := make([]byte, 0, 1+len(nonce)+len(plaintext)+aead.Overhead())
	env = append(env, alg)
	env = append(env, nonce...)
	return aead.Seal(env, nonce, plaintext, bindHeader(alg, aad)), nil
}

// Open reads the header byte, selects the matching decryptor, and returns the
// plaintext, the algorithm id, and any error. A non-nil error means the
// envelope is unauthenticated and must be discarded.
func Open(key, envelope, aad []byte) ([]byte, byte, error) {
	if len(envelope) < 1 {
		return nil, 0, ErrShortEnvelope
	}
	alg := envelope[0]
	aead, err := aeadFor(alg, key)
	if err != nil {
		return nil, alg, err
	}
	ns := aead.NonceSize()
	body := envelope[1:]
	if len(body) < ns {
		return nil, alg, ErrShortEnvelope
	}
	nonce, ciphertext := body[:ns], body[ns:]
	plaintext, err := aead.Open(nil, nonce, ciphertext, bindHeader(alg, aad))
	if err != nil {
		return nil, alg, fmt.Errorf("%w: %v", ErrOpen, err)
	}
	return plaintext, alg, nil
}
```

### The runnable demo

The demo seals the same secret under both ciphers with the same key, prints each
envelope's header byte and deterministic length, round-trips both, then rewrites
an AES-GCM envelope's header to the XChaCha id and shows `Open` rejecting it. The
ciphertext bytes are random, but the lengths and outcomes are deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"

	"example.com/aeadenvelope"
)

func main() {
	key := make([]byte, aeadenvelope.KeySize)
	if _, err := rand.Read(key); err != nil {
		log.Fatal(err)
	}
	secret := []byte("service-account-token")
	const aad = "tenant:acme"

	for _, alg := range []byte{aeadenvelope.AlgAESGCM, aeadenvelope.AlgXChaCha} {
		env, err := aeadenvelope.Seal(alg, key, secret, []byte(aad))
		if err != nil {
			log.Fatal(err)
		}
		got, gotAlg, err := aeadenvelope.Open(key, env, []byte(aad))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%-18s header=%d len=%d decrypted=%s\n",
			aeadenvelope.AlgName(gotAlg), env[0], len(env), got)
	}

	// Rewrite an AES-GCM header to the XChaCha id: Open must reject it.
	env, err := aeadenvelope.Seal(aeadenvelope.AlgAESGCM, key, secret, []byte(aad))
	if err != nil {
		log.Fatal(err)
	}
	env[0] = aeadenvelope.AlgXChaCha
	if _, _, err := aeadenvelope.Open(key, env, []byte(aad)); errors.Is(err, aeadenvelope.ErrOpen) {
		fmt.Println("header-swap:       rejected")
	}
}
```

Run it:

```bash
GOFLAGS=-mod=mod go run ./cmd/demo
```

Expected output:

```
AES-256-GCM        header=1 len=50 decrypted=service-account-token
XChaCha20-Poly1305 header=2 len=62 decrypted=service-account-token
header-swap:       rejected
```

### Tests

The tests drive both algorithm ids through the single `Seal`/`Open` API and
assert the header byte matches the request. They then exercise the failure
surface: an unknown id, an empty envelope, a one-byte envelope, and the
header-swap attack. The header-swap case is the important one — it proves the
algorithm byte is authenticated, not merely advisory.

Create `envelope_test.go`:

```go
package aeadenvelope

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return key
}

func TestRoundTripBothAlgs(t *testing.T) {
	t.Parallel()
	key := mustKey(t)
	cases := []struct {
		name string
		alg  byte
	}{
		{"aes-gcm", AlgAESGCM},
		{"xchacha", AlgXChaCha},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pt := []byte("portable secret")
			env, err := Seal(tc.alg, key, pt, []byte("aad"))
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if env[0] != tc.alg {
				t.Fatalf("header byte = %d, want %d", env[0], tc.alg)
			}
			got, alg, err := Open(key, env, []byte("aad"))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if alg != tc.alg {
				t.Fatalf("Open alg = %d, want %d", alg, tc.alg)
			}
			if !bytes.Equal(got, pt) {
				t.Fatalf("round-trip = %q, want %q", got, pt)
			}
		})
	}
}

func TestWrongAADFails(t *testing.T) {
	t.Parallel()
	key := mustKey(t)
	for _, alg := range []byte{AlgAESGCM, AlgXChaCha} {
		env, err := Seal(alg, key, []byte("secret"), []byte("tenant:a"))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(key, env, []byte("tenant:b")); !errors.Is(err, ErrOpen) {
			t.Fatalf("alg %d: AAD mismatch err = %v, want ErrOpen", alg, err)
		}
	}
}

func TestUnknownAlgFails(t *testing.T) {
	t.Parallel()
	key := mustKey(t)
	if _, err := Seal(0x09, key, []byte("x"), nil); !errors.Is(err, ErrUnknownAlg) {
		t.Fatalf("Seal unknown alg: err = %v, want ErrUnknownAlg", err)
	}
	env := []byte{0x09, 0x00, 0x01, 0x02}
	if _, _, err := Open(key, env, nil); !errors.Is(err, ErrUnknownAlg) {
		t.Fatalf("Open unknown alg: err = %v, want ErrUnknownAlg", err)
	}
}

func TestShortEnvelopeFails(t *testing.T) {
	t.Parallel()
	key := mustKey(t)
	// Empty envelope: no header at all.
	if _, _, err := Open(key, nil, nil); !errors.Is(err, ErrShortEnvelope) {
		t.Fatalf("empty: err = %v, want ErrShortEnvelope", err)
	}
	// XChaCha header but body shorter than the 24-byte nonce.
	short := append([]byte{AlgXChaCha}, make([]byte, 10)...)
	if _, _, err := Open(key, short, nil); !errors.Is(err, ErrShortEnvelope) {
		t.Fatalf("short body: err = %v, want ErrShortEnvelope", err)
	}
}

func TestHeaderSwapFails(t *testing.T) {
	t.Parallel()
	key := mustKey(t)
	env, err := Seal(AlgAESGCM, key, []byte("secret payload"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	env[0] = AlgXChaCha // rewrite the authenticated header
	if _, _, err := Open(key, env, []byte("aad")); !errors.Is(err, ErrOpen) {
		t.Fatalf("header swap: err = %v, want ErrOpen", err)
	}
}

func TestTamperFails(t *testing.T) {
	t.Parallel()
	key := mustKey(t)
	for _, alg := range []byte{AlgAESGCM, AlgXChaCha} {
		env, err := Seal(alg, key, []byte("secret payload"), nil)
		if err != nil {
			t.Fatal(err)
		}
		env[len(env)-1] ^= 0x01
		if _, _, err := Open(key, env, nil); !errors.Is(err, ErrOpen) {
			t.Fatalf("alg %d: tamper err = %v, want ErrOpen", alg, err)
		}
	}
}

func TestXChaChaEnvelopeIsTwelveBytesLonger(t *testing.T) {
	t.Parallel()
	key := mustKey(t)
	pt := []byte("portable secret")
	gcm, err := Seal(AlgAESGCM, key, pt, []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	xc, err := Seal(AlgXChaCha, key, pt, []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	// XChaCha carries a 24-byte explicit nonce; random-nonce GCM prepends a
	// 12-byte one, so the XChaCha envelope is exactly 12 bytes larger.
	if got := len(xc) - len(gcm); got != 12 {
		t.Fatalf("XChaCha envelope length delta = %d, want 12", got)
	}
}

func Example() {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	env, err := Seal(AlgXChaCha, key, []byte("hello"), []byte("ctx"))
	if err != nil {
		panic(err)
	}
	pt, alg, err := Open(key, env, []byte("ctx"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s via %s\n", pt, AlgName(alg))
	// Output: hello via XChaCha20-Poly1305
}
```

`TestXChaChaEnvelopeIsTwelveBytesLonger` pins the layout difference directly: it
seals the same plaintext under both algorithms and asserts the XChaCha envelope
is exactly 12 bytes longer — the gap between the 24-byte explicit nonce and GCM's
12-byte prepended nonce.

## Review

The envelope is correct when `Open` recovers the plaintext for both algorithms,
reports the algorithm actually used, and refuses anything whose header,
associated data, or ciphertext has been altered. The header-swap test is the
proof that the algorithm byte is authenticated: because `Seal` binds `alg` into
the AAD, flipping the header changes the reconstructed AAD and `Open` fails with
`ErrOpen`.

The mistakes to avoid are format ones. Do not store the algorithm id outside the
authenticated bytes (for example in a separate unauthenticated column); binding
it as AAD is what makes it tamper-evident. Do not assume every AEAD manages its
own nonce — `NewGCMWithRandomNonce` does (`NonceSize()==0`), but `NewX` does not
(`NonceSize()==24`), and the code must branch on `NonceSize()` rather than
hard-code a length. Do not forget the short-envelope guard: an attacker who
sends a one-byte or empty blob must get `ErrShortEnvelope`, never a slice
out-of-range panic. Run `GOFLAGS=-mod=mod go test -race ./...` to confirm both
paths and the failure surface.

## Resources

- [crypto/cipher: NewGCMWithRandomNonce](https://pkg.go.dev/crypto/cipher#NewGCMWithRandomNonce) — the Go 1.24 random-nonce GCM, `NonceSize()==0`, and the 2^32 limit.
- [golang.org/x/crypto/chacha20poly1305](https://pkg.go.dev/golang.org/x/crypto/chacha20poly1305) — `New`, `NewX`, `KeySize`, `NonceSizeX`, `Overhead`.
- [golang/go issue #69981](https://github.com/golang/go/issues/69981) — the proposal and rationale for `NewGCMWithRandomNonce`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-aes-gcm-field-encryption.md](01-aes-gcm-field-encryption.md) | Next: [03-chunked-stream-aead.md](03-chunked-stream-aead.md)
