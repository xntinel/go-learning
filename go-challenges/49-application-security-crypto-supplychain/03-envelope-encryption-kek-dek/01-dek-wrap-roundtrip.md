# Exercise 1: Wrap a DEK under a KEK and round-trip an envelope

This exercise builds the core envelope codec: `Seal` mints a fresh 32-byte DEK,
encrypts the payload under it with AES-256-GCM, wraps the DEK under the KEK with a
second AES-256-GCM, and returns a serializable envelope; `Open` reverses it. All
nonce handling is delegated to `cipher.NewGCMWithRandomNonce`, so there is no
counter to get wrong.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
envelope/                   independent module: example.com/envelope
  go.mod                    go 1.24 (NewGCMWithRandomNonce needs it)
  envelope.go               type Envelope; Seal, Open; ErrKEKSize/ErrUnwrap/ErrDecrypt
  cmd/
    demo/
      main.go               seals a JSON secret and opens it again
  envelope_test.go          round-trip, tamper, wrong-KEK, fresh-DEK, JSON tests
```

- Files: `envelope.go`, `cmd/demo/main.go`, `envelope_test.go`.
- Implement: `Seal(kek []byte, kekID string, plaintext []byte) (Envelope, error)` and `Open(kek []byte, env Envelope) ([]byte, error)`, wrapping the DEK and the data with AES-256-GCM and enforcing a 32-byte KEK.
- Test: table-driven round-trips for empty/small/large inputs; tamper detection on both layers; wrong-KEK; distinct output per Seal; a JSON marshal/unmarshal round-trip.
- Verify: `go test -count=1 -race ./...`

Set up the module. `cipher.NewGCMWithRandomNonce` was added in Go 1.24, so pin the
language version:

```bash
go mod edit -go=1.24
```

### The shape of an envelope

An envelope is the self-describing unit you persist next to nothing else — it
carries everything needed to decrypt the payload *given the KEK*. It has four
fields: a `Version` so the format can evolve, a `KEKID` naming which key wrapped
the DEK (unused by a single-KEK codec but essential the moment you rotate, which
is the next exercise), the `WrappedDEK`, and the `Ciphertext`. Both byte fields
are nonce-prefixed AEAD outputs: `NewGCMWithRandomNonce` prepends the 12-byte
nonce it generated, so each field is `nonce || ciphertext || tag` and you never
store or manage a nonce yourself.

Because the struct is plain data with `json` tags, `encoding/json` serializes the
two `[]byte` fields as base64 strings automatically, so an envelope round-trips
through JSON, a database column, or a message body without extra work.

### How Seal composes two AEADs

`Seal` runs two independent AES-256-GCM operations. First it generates a fresh
32-byte DEK from `crypto/rand` and uses it to encrypt the plaintext — this is the
bulk work, done locally. Then it uses the KEK to encrypt (wrap) those same 32 DEK
bytes. The DEK exists in plaintext only inside `Seal`, on the stack, for the
duration of the call; what leaves the function is the wrapped form.

The helper `newAEAD` centralizes two decisions. It enforces the AES-256 policy
with an explicit `len(key) != 32` check — necessary because `aes.NewCipher`
silently accepts 16, 24, and 32-byte keys, so without the check a caller could
downgrade to AES-128 by passing a short key. And it constructs the random-nonce
AEAD, so both the wrap and the bulk encryption share identical, correct nonce
handling. Passing `nil` as the nonce argument to `Seal`/`Open` is how you tell the
random-nonce AEAD to manage the nonce itself; its `NonceSize()` is zero.

### How Open fails loudly

`Open` mirrors `Seal` in reverse: unwrap the DEK with the KEK, then decrypt the
data with the DEK. The ordering matters for diagnosis. If the wrapped DEK was
tampered with, or the wrong KEK is supplied, the *first* GCM `Open` fails and the
function returns before it ever touches the payload — surfaced as `ErrUnwrap`. If
the DEK unwraps cleanly but the ciphertext was tampered with, the *second* `Open`
fails — surfaced as `ErrDecrypt`. Wrapping the underlying GCM error with a
sentinel via `%w` lets tests and callers branch on the failure kind with
`errors.Is` while still keeping no plaintext on any error path.

Create `envelope.go`:

```go
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// dekSize is 32 bytes: the DEK is an AES-256 key.
const dekSize = 32

// Sentinel errors let callers branch on the failure kind with errors.Is.
var (
	ErrKEKSize = errors.New("envelope: KEK must be 32 bytes (AES-256)")
	ErrUnwrap  = errors.New("envelope: DEK unwrap failed")
	ErrDecrypt = errors.New("envelope: data decrypt failed")
)

// Envelope is the serializable output of Seal. Both byte fields are
// nonce-prefixed AES-256-GCM outputs (nonce || ciphertext || tag).
type Envelope struct {
	Version    int    `json:"version"`
	KEKID      string `json:"kek_id"`
	WrappedDEK []byte `json:"wrapped_dek"`
	Ciphertext []byte `json:"ciphertext"`
}

// newAEAD enforces the AES-256 key-size policy and returns a random-nonce GCM
// AEAD. NewGCMWithRandomNonce generates and prepends a 96-bit nonce per Seal,
// so callers pass nil as the nonce argument.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrKEKSize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// Seal generates a fresh DEK, encrypts plaintext under it, wraps the DEK under
// kek, and returns a versioned envelope stamped with kekID.
func Seal(kek []byte, kekID string, plaintext []byte) (Envelope, error) {
	kekAEAD, err := newAEAD(kek)
	if err != nil {
		return Envelope{}, err
	}

	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return Envelope{}, fmt.Errorf("envelope: generate DEK: %w", err)
	}

	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return Envelope{}, err
	}
	ciphertext := dataAEAD.Seal(nil, nil, plaintext, nil)
	wrapped := kekAEAD.Seal(nil, nil, dek, nil)

	return Envelope{
		Version:    1,
		KEKID:      kekID,
		WrappedDEK: wrapped,
		Ciphertext: ciphertext,
	}, nil
}

// Open unwraps the DEK with kek, then decrypts the payload with the DEK. A
// tampered or wrong-KEK wrap fails as ErrUnwrap before the payload is touched;
// a tampered payload fails as ErrDecrypt. No plaintext is returned on error.
func Open(kek []byte, env Envelope) ([]byte, error) {
	kekAEAD, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	dek, err := kekAEAD.Open(nil, nil, env.WrappedDEK, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnwrap, err)
	}

	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	plaintext, err := dataAEAD.Open(nil, nil, env.Ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return plaintext, nil
}
```

### The runnable demo

The demo seals a small JSON secret under a random KEK, prints the sizes of the two
authenticated fields (which are fixed by the AEAD overhead of 28 bytes, so they
are deterministic), then opens it and prints the recovered plaintext.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/rand"
	"fmt"
	"log"

	"example.com/envelope"
)

func main() {
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		log.Fatal(err)
	}

	secret := []byte(`{"db_password":"s3cr3t","api_key":"AKIA..."}`)
	env, err := envelope.Seal(kek, "kek-2024-01", secret)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("kek id:      %s\n", env.KEKID)
	fmt.Printf("wrapped DEK: %d bytes\n", len(env.WrappedDEK))
	fmt.Printf("ciphertext:  %d bytes\n", len(env.Ciphertext))

	recovered, err := envelope.Open(kek, env)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("recovered:   %s\n", recovered)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
kek id:      kek-2024-01
wrapped DEK: 60 bytes
ciphertext:  72 bytes
recovered:   {"db_password":"s3cr3t","api_key":"AKIA..."}
```

### Tests

The tests pin down each property from the exercise brief. `TestRoundTrip` covers
empty, small, and large payloads. `TestTamperDetection` flips one byte of each
authenticated field and asserts the *specific* sentinel: a corrupted ciphertext
is `ErrDecrypt`, a corrupted wrapped DEK is `ErrUnwrap` (it fails before the
payload is decrypted). `TestWrongKEK` confirms a different KEK cannot unwrap.
`TestFreshDEKPerSeal` proves each Seal produces distinct outputs — fresh DEK plus
random nonces mean two seals of the same plaintext never match. `TestKEKSize`
proves the AES-256 policy is enforced. `TestJSONRoundTrip` serializes the
envelope and back before opening, exercising the on-the-wire form.

Create `envelope_test.go`:

```go
package envelope

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func mustKEK(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return k
}

func clone(e Envelope) Envelope {
	return Envelope{
		Version:    e.Version,
		KEKID:      e.KEKID,
		WrappedDEK: append([]byte(nil), e.WrappedDEK...),
		Ciphertext: append([]byte(nil), e.Ciphertext...),
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	kek := mustKEK(t)
	cases := []struct {
		name string
		pt   []byte
	}{
		{"empty", []byte{}},
		{"small", []byte("hello")},
		{"large", bytes.Repeat([]byte("A"), 1<<16)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, err := Seal(kek, "kek-1", tc.pt)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			got, err := Open(kek, env)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(got, tc.pt) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(tc.pt))
			}
		})
	}
}

func TestTamperDetection(t *testing.T) {
	t.Parallel()
	kek := mustKEK(t)
	env, err := Seal(kek, "kek-1", []byte("top secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	t.Run("flip ciphertext", func(t *testing.T) {
		bad := clone(env)
		bad.Ciphertext[0] ^= 0xFF
		if _, err := Open(kek, bad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("Open = %v; want ErrDecrypt", err)
		}
	})

	t.Run("flip wrapped DEK", func(t *testing.T) {
		bad := clone(env)
		bad.WrappedDEK[0] ^= 0xFF
		if _, err := Open(kek, bad); !errors.Is(err, ErrUnwrap) {
			t.Fatalf("Open = %v; want ErrUnwrap", err)
		}
	})
}

func TestWrongKEK(t *testing.T) {
	t.Parallel()
	kek := mustKEK(t)
	other := mustKEK(t)
	env, err := Seal(kek, "kek-1", []byte("data"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(other, env); !errors.Is(err, ErrUnwrap) {
		t.Fatalf("Open with wrong KEK = %v; want ErrUnwrap", err)
	}
}

func TestFreshDEKPerSeal(t *testing.T) {
	t.Parallel()
	kek := mustKEK(t)
	pt := []byte("same plaintext")
	a, err := Seal(kek, "kek-1", pt)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := Seal(kek, "kek-1", pt)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(a.WrappedDEK, b.WrappedDEK) {
		t.Fatal("two seals produced identical WrappedDEK")
	}
	if bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("two seals produced identical Ciphertext")
	}
}

func TestKEKSize(t *testing.T) {
	t.Parallel()
	short := make([]byte, 16)
	if _, err := Seal(short, "kek-1", []byte("x")); !errors.Is(err, ErrKEKSize) {
		t.Fatalf("Seal with 16-byte KEK = %v; want ErrKEKSize", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	t.Parallel()
	kek := mustKEK(t)
	env, err := Seal(kek, "kek-1", []byte("persist me"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Envelope
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, err := Open(kek, back)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, []byte("persist me")) {
		t.Fatalf("round-trip via JSON mismatch: %q", got)
	}
}

func Example() {
	// An all-zero KEK is used ONLY to make the printed sizes deterministic;
	// never use a fixed or zero key in production.
	kek := make([]byte, 32)
	env, err := Seal(kek, "kek-2024", []byte("hello"))
	if err != nil {
		panic(err)
	}
	fmt.Println(env.KEKID, len(env.WrappedDEK), len(env.Ciphertext))
	// Output: kek-2024 60 33
}
```

## Review

The codec is correct when `Open(kek, Seal(kek, id, pt))` returns `pt` for any
payload and any byte-level change to either authenticated field turns into a
non-nil error rather than corrupted output. The sizes are a useful sanity check:
a wrapped 32-byte DEK is always 60 bytes (32 + 28 of AEAD overhead) and a
ciphertext is always `len(plaintext) + 28`, because `NewGCMWithRandomNonce`
contributes a 12-byte nonce and a 16-byte tag. If your wrapped DEK is not 60
bytes, you are not wrapping exactly the DEK.

The mistakes to avoid are the ones the tests are built to catch. Do not skip the
`len(kek) != 32` check thinking `aes.NewCipher` will reject a short key — it will
not; it will happily give you AES-128. Do not try to manage nonces yourself
alongside `NewGCMWithRandomNonce`; pass `nil` and let it prepend and extract the
nonce. Do not return the plaintext buffer on an `Open` error path — GCM already
returns `nil` on authentication failure, so propagate the wrapped sentinel and
nothing else. Run `go test -race` to confirm the whole thing, and note that
`TestFreshDEKPerSeal` is what proves you are minting a new DEK every time rather
than caching one.

## Resources

- [`crypto/cipher`](https://pkg.go.dev/crypto/cipher) — the `AEAD` interface and `NewGCMWithRandomNonce` (its zero `NonceSize`, 28-byte `Overhead`, and 2^32-message limit).
- [`crypto/aes`](https://pkg.go.dev/crypto/aes) — `NewCipher`, which accepts 16, 24, or 32-byte keys, so AES-256 is a policy you enforce.
- [`crypto/rand`](https://pkg.go.dev/crypto/rand) — `Read`, the source for DEK material.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-kek-rotation-keyring.md](02-kek-rotation-keyring.md)
