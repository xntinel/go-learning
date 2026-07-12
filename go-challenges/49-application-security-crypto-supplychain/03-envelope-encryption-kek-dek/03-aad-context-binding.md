# Exercise 3: Bind envelopes to object identity with associated data

An envelope that round-trips perfectly can still be attacked by *moving* it. If a
valid wrapped-DEK-plus-ciphertext from object A can be pasted onto object B, and
both layers verify, then B silently serves A's plaintext — an envelope-swap /
confused-deputy attack. This exercise closes that hole by binding each envelope to
its object identity with AEAD associated data (AAD), using a canonical
length-prefixed encoding so distinct contexts can never collide.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
aadbind/                    independent module: example.com/aadbind
  go.mod                    go 1.24 (NewGCMWithRandomNonce needs it)
  aadbind.go                type Context, Envelope; Seal, Open; canonicalAAD
  cmd/
    demo/
      main.go               bound decrypt vs swapped-context decrypt
  aadbind_test.go           same-context, mismatch, swap-attack, canonical-AAD tests
```

- Files: `aadbind.go`, `cmd/demo/main.go`, `aadbind_test.go`.
- Implement: `Seal(kek []byte, kekID string, plaintext []byte, ctx Context) (Envelope, error)` and `Open(kek []byte, env Envelope, ctx Context) ([]byte, error)`, binding the data ciphertext to a canonical encoding of `ctx`; plus `canonicalAAD(ctx Context) []byte` with length-prefixed fields.
- Test: same-context succeeds; mismatched context fails; a swap of a valid envelope onto another object fails; canonical AAD makes `{a, bc}` and `{ab, c}` distinct where naive concatenation would collide.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/03-envelope-encryption-kek-dek/03-aad-context-binding/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/03-envelope-encryption-kek-dek/03-aad-context-binding
go mod edit -go=1.24
```

### Associated data: authenticate the context, do not encrypt it

Every AEAD `Seal`/`Open` takes a fourth argument, the associated data. Those bytes
are not encrypted — they are not part of the ciphertext at all — but they are
folded into the authentication tag. `Open` recomputes the tag over the ciphertext
*and* the AAD you supply; if the AAD differs by a single bit from what `Seal`
used, authentication fails and you get an error and no plaintext. This is exactly
the tool for binding an envelope to *where it lives* without storing that context
inside the envelope.

The context here is an object's identity: a `{Tenant, Object}` pair. On `Seal`, we
encrypt the payload with the DEK and pass the canonical encoding of the context as
AAD, so the tag commits to it. On `Open`, the caller reconstructs the context from
where the object was fetched — the tenant that owns the row, the primary key of
the record — and passes it in. If an attacker moved the envelope to a different
object, the reconstructed context will not match the sealed one, the AAD will
differ, and `Open` fails. The context is never carried in the envelope, so the
attacker cannot rewrite it to match; it comes from the trusted storage layer.

We bind the *data* ciphertext to the context. That is sufficient to defeat the
swap: even though the DEK unwrap (under the shared KEK) still succeeds, decrypting
the payload with the wrong AAD fails, so `Open` returns an error overall. The
`NewGCMWithRandomNonce` AEAD supports the `additionalData` argument on both `Seal`
and `Open` just like ordinary GCM, so binding is a matter of passing the AAD
instead of `nil`.

### Canonical AAD: why naive concatenation is a vulnerability

The AAD must encode the context *unambiguously*. If you build it by concatenating
fields — `[]byte(ctx.Tenant + ctx.Object)` — then two different contexts can
produce identical bytes: `{Tenant: "a", Object: "bc"}` and
`{Tenant: "ab", Object: "c"}` both yield `"abc"`. An attacker who controls part of
the identity could exploit that collision to move an envelope between two contexts
that happen to serialize the same way, re-opening the very hole AAD was meant to
close.

The fix is a canonical encoding: for each field, write its length as a fixed-width
big-endian integer, then the field bytes. `{a, bc}` becomes
`00000001 'a' 00000002 'b' 'c'` and `{ab, c}` becomes `00000002 'a' 'b' 00000001 'c'` —
now provably distinct. Length-prefixing guarantees the byte string parses back to
exactly one context, so distinct contexts always yield distinct AAD.
`encoding/binary.BigEndian.PutUint32` writes the length header.

Create `aadbind.go`:

```go
package aadbind

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

const dekSize = 32

var (
	ErrKEKSize = errors.New("aadbind: KEK must be 32 bytes (AES-256)")
	ErrUnwrap  = errors.New("aadbind: DEK unwrap failed")
	ErrDecrypt = errors.New("aadbind: data decrypt failed (wrong context or tampering)")
)

// Context is the object identity an envelope is bound to. It is supplied by the
// caller on both Seal and Open and is never stored inside the envelope.
type Context struct {
	Tenant string
	Object string
}

// Envelope holds the wrapped DEK and the context-bound ciphertext. It does not
// carry the Context: the caller reconstructs that from where the object lives.
type Envelope struct {
	Version    int    `json:"version"`
	KEKID      string `json:"kek_id"`
	WrappedDEK []byte `json:"wrapped_dek"`
	Ciphertext []byte `json:"ciphertext"`
}

// canonicalAAD encodes ctx unambiguously as length-prefixed fields, so distinct
// contexts always map to distinct bytes (naive concatenation would let
// {"a","bc"} and {"ab","c"} collide).
func canonicalAAD(ctx Context) []byte {
	var aad []byte
	aad = appendField(aad, ctx.Tenant)
	aad = appendField(aad, ctx.Object)
	return aad
}

func appendField(dst []byte, field string) []byte {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(field)))
	dst = append(dst, hdr[:]...)
	return append(dst, field...)
}

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

// Seal encrypts plaintext under a fresh DEK bound to ctx via AAD, wraps the DEK
// under kek, and stamps kekID into the envelope.
func Seal(kek []byte, kekID string, plaintext []byte, ctx Context) (Envelope, error) {
	kekAEAD, err := newAEAD(kek)
	if err != nil {
		return Envelope{}, err
	}

	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return Envelope{}, fmt.Errorf("aadbind: generate DEK: %w", err)
	}

	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return Envelope{}, err
	}
	ciphertext := dataAEAD.Seal(nil, nil, plaintext, canonicalAAD(ctx))
	wrapped := kekAEAD.Seal(nil, nil, dek, nil)

	return Envelope{
		Version:    1,
		KEKID:      kekID,
		WrappedDEK: wrapped,
		Ciphertext: ciphertext,
	}, nil
}

// Open unwraps the DEK, then decrypts the payload, recomputing the expected AAD
// from ctx. If ctx does not match what Seal bound (an envelope-swap or a
// mismatched caller), the data decrypt fails as ErrDecrypt.
func Open(kek []byte, env Envelope, ctx Context) ([]byte, error) {
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
	plaintext, err := dataAEAD.Open(nil, nil, env.Ciphertext, canonicalAAD(ctx))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return plaintext, nil
}
```

### The runnable demo

The demo seals a balance for one account, opens it with the matching context, then
attempts to open the same envelope under a different object id and a different
tenant — both rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/rand"
	"fmt"
	"log"

	"example.com/aadbind"
)

func main() {
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		log.Fatal(err)
	}

	ctxA := aadbind.Context{Tenant: "acme", Object: "account-42"}
	env, err := aadbind.Seal(kek, "kek-1", []byte("balance=100"), ctxA)
	if err != nil {
		log.Fatal(err)
	}

	pt, err := aadbind.Open(kek, env, ctxA)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("correct context: %s\n", pt)

	// Same envelope, different object: an envelope-swap attempt.
	swapped := aadbind.Context{Tenant: "acme", Object: "account-43"}
	if _, err := aadbind.Open(kek, env, swapped); err != nil {
		fmt.Println("swapped object: rejected")
	}

	// Same envelope, different tenant: a cross-tenant attempt.
	crossTenant := aadbind.Context{Tenant: "evil", Object: "account-42"}
	if _, err := aadbind.Open(kek, env, crossTenant); err != nil {
		fmt.Println("wrong tenant: rejected")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
correct context: balance=100
swapped object: rejected
wrong tenant: rejected
```

### Tests

`TestBoundRoundTrip` confirms the happy path: seal and open with the same context.
`TestContextMismatch` opens with a different object and expects `ErrDecrypt`.
`TestEnvelopeSwap` is the attack in miniature — a valid envelope for object A,
presented as object B, must fail. `TestCanonicalAAD` is the interesting one: it
shows that the canonical encodings of `{a, bc}` and `{ab, c}` differ, while a
naive concatenation of the same fields collides, and then proves the difference
matters end to end by sealing under one and failing to open under the other.

Create `aadbind_test.go`:

```go
package aadbind

import (
	"bytes"
	"crypto/rand"
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

func TestBoundRoundTrip(t *testing.T) {
	t.Parallel()
	kek := mustKEK(t)
	ctx := Context{Tenant: "acme", Object: "42"}
	env, err := Seal(kek, "kek-1", []byte("secret"), ctx)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := Open(kek, env, ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, []byte("secret")) {
		t.Fatalf("Open = %q; want secret", got)
	}
}

func TestContextMismatch(t *testing.T) {
	t.Parallel()
	kek := mustKEK(t)
	sealCtx := Context{Tenant: "acme", Object: "42"}
	env, err := Seal(kek, "kek-1", []byte("secret"), sealCtx)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	cases := []struct {
		name string
		ctx  Context
	}{
		{"different object", Context{Tenant: "acme", Object: "43"}},
		{"different tenant", Context{Tenant: "evil", Object: "42"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Open(kek, env, tc.ctx); !errors.Is(err, ErrDecrypt) {
				t.Fatalf("Open with %+v = %v; want ErrDecrypt", tc.ctx, err)
			}
		})
	}
}

func TestEnvelopeSwap(t *testing.T) {
	t.Parallel()
	kek := mustKEK(t)
	ctxA := Context{Tenant: "acme", Object: "A"}
	ctxB := Context{Tenant: "acme", Object: "B"}

	envA, err := Seal(kek, "kek-1", []byte("A's data"), ctxA)
	if err != nil {
		t.Fatalf("Seal A: %v", err)
	}
	// Attacker moves A's valid envelope onto object B.
	if _, err := Open(kek, envA, ctxB); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("swap A->B = %v; want ErrDecrypt", err)
	}
	// A still opens under its own context.
	if _, err := Open(kek, envA, ctxA); err != nil {
		t.Fatalf("Open A under ctxA: %v", err)
	}
}

func TestCanonicalAAD(t *testing.T) {
	t.Parallel()
	x := Context{Tenant: "a", Object: "bc"}
	y := Context{Tenant: "ab", Object: "c"}

	// Canonical (length-prefixed) encodings differ.
	if bytes.Equal(canonicalAAD(x), canonicalAAD(y)) {
		t.Fatal("canonicalAAD collided for distinct contexts")
	}
	// Naive concatenation would collide, which is why canonical encoding matters.
	naiveX := []byte(x.Tenant + x.Object)
	naiveY := []byte(y.Tenant + y.Object)
	if !bytes.Equal(naiveX, naiveY) {
		t.Fatal("expected naive concatenation to collide for this example")
	}

	// End to end: sealing under x and opening under y must fail.
	kek := mustKEK(t)
	env, err := Seal(kek, "kek-1", []byte("data"), x)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(kek, env, y); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Open under colliding-if-naive context = %v; want ErrDecrypt", err)
	}
}

func Example() {
	// An all-zero KEK keeps the example deterministic; never use a fixed key in
	// production.
	kek := make([]byte, 32)
	ctxA := Context{Tenant: "acme", Object: "42"}
	ctxB := Context{Tenant: "acme", Object: "43"}

	env, _ := Seal(kek, "kek-1", []byte("balance=100"), ctxA)

	_, err := Open(kek, env, ctxA)
	fmt.Println("same context:", err == nil)
	_, err = Open(kek, env, ctxB)
	fmt.Println("swapped context:", err == nil)
	// Output:
	// same context: true
	// swapped context: false
}
```

## Review

Context binding is correct when opening succeeds if and only if the caller's
context matches the one sealed. `TestEnvelopeSwap` is the load-bearing test: a
valid envelope for A must fail under B's context even though the KEK and the DEK
are perfectly intact, because the data-layer AAD no longer matches. If that test
passes only when you also corrupt the ciphertext, you have not actually bound the
context.

The subtle failure is the canonical encoding. It is tempting to build AAD as
`ctx.Tenant + "|" + ctx.Object` or a bare concatenation; both are attackable when
an attacker controls part of the identity (a separator can appear inside a field;
a bare concatenation collides outright). `TestCanonicalAAD` demonstrates the
collision concretely and proves the length-prefixed form avoids it. Two more
things to keep straight: the context is deliberately *not* stored in the envelope,
so it cannot be tampered into agreement — it must be reconstructed from trusted
storage on every `Open`; and any mismatch is a hard `ErrDecrypt` with no
plaintext, never a warning you can ignore. Run `go test -race` to confirm.

## Resources

- [`crypto/cipher`](https://pkg.go.dev/crypto/cipher) — the `AEAD.Seal`/`AEAD.Open` `additionalData` parameter that carries the AAD.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `BigEndian.PutUint32`, used to length-prefix each AAD field.
- [Go 1.24 release notes](https://go.dev/doc/go1.24) — `crypto/cipher.NewGCMWithRandomNonce`, `crypto/hkdf`, and other crypto additions.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-kek-rotation-keyring.md](02-kek-rotation-keyring.md) | Next: [../04-password-hashing-argon2/00-concepts.md](../04-password-hashing-argon2/00-concepts.md)
