# Exercise 1: A Hybrid KEM: Combining X25519 and ML-KEM-768

Before trusting `crypto/tls` to negotiate a hybrid group for you, build the
primitive by hand so the mechanism is not a black box. This exercise reconstructs
what `X25519MLKEM768` does internally: run a classical X25519 ECDH and an
ML-KEM-768 encapsulation in the same exchange, then combine both shared secrets
into one session key with HKDF.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
hybridkem/                 independent module: example.com/hybridkem
  go.mod                   go 1.26 (crypto/mlkem, crypto/hkdf)
  hybridkem.go             Offer/Accept/Complete; combine via HKDF; seed regen
  cmd/
    demo/
      main.go              runnable demo: full three-message exchange
  hybridkem_test.go        round-trip, wire sizes, regen, tamper, malformed inputs
```

- Files: `hybridkem.go`, `cmd/demo/main.go`, `hybridkem_test.go`.
- Implement: `Offer`, `Accept`, `(*Party).Complete`, `(*Party).MLKEMSeed`, and `RegenerateEncapKey`, combining the ML-KEM and X25519 shared secrets with `hkdf.Key`.
- Test: round-trip key equality, wire-size assertions against the `mlkem` constants, deterministic seed regeneration, a tamper test proving implicit rejection, and sentinel-error wrapping for malformed inputs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/01-post-quantum-hybrid-tls/01-hybrid-kem-handshake/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/01-post-quantum-hybrid-tls/01-hybrid-kem-handshake
go mod edit -go=1.26
```

### The three-message shape

A hybrid KEM handshake has three logical messages and two parties. Call the party
that publishes a public key first the *initiator* and the one that responds the
*responder*.

1. `Offer` (initiator): generate an ML-KEM-768 decapsulation key and an X25519
   private key. Publish the *public* halves — the 1184-byte ML-KEM encapsulation
   key and the 32-byte X25519 public key — as the offer message. Keep the two
   private keys in a `Party` value.
2. `Accept` (responder): parse the initiator's encapsulation key and call
   `Encapsulate()`, which returns a fresh 32-byte ML-KEM shared secret and its
   1088-byte ciphertext. Generate an X25519 private key and compute the ECDH
   shared secret against the initiator's X25519 public key. Combine both shared
   secrets into the session key, and publish the ciphertext plus the responder's
   own X25519 public key as the accept message.
3. `Complete` (initiator): `Decapsulate` the ciphertext to recover the same
   ML-KEM shared secret, compute the ECDH shared secret against the responder's
   X25519 public key, and combine to derive the identical session key.

The asymmetry of a KEM is visible here: only the responder calls `Encapsulate`,
only the initiator calls `Decapsulate`, and the ML-KEM shared secret flows from
responder to initiator inside a ciphertext — unlike X25519, where both sides
symmetrically compute the same value from a public-key exchange.

### Why concatenate-then-KDF

Both sides derive the session key the same way: concatenate the ML-KEM shared
secret with the X25519 shared secret and run the concatenation through HKDF. This
is the combiner that gives the security-if-either-holds property. An attacker who
later breaks X25519 still faces an unknown ML-KEM secret in the KDF input, and
vice versa. The order is fixed — ML-KEM secret first, then X25519 — matching the
ordering the TLS `X25519MLKEM768` construction uses, and both sides must agree on
it or the derived keys will not match. XOR would be wrong: if either secret is
ever recovered it can be stripped off, and the "either holds" guarantee collapses.

`hkdf.Key` is the one-shot HKDF helper: it takes the hash constructor
(`sha256.New`), the input keying material, an optional salt, an `info` string that
domain-separates this use of the KDF from any other, and the desired output
length. Passing a distinctive `info` string means the same secret used for a
different purpose derives a different key.

### Implicit rejection, made concrete

`Decapsulate` does not error on a ciphertext that was tampered with or produced
under the wrong key — it returns a *different*, pseudo-random shared secret. The
tamper test below flips one bit of the ciphertext and asserts that `Complete`
returns *no error* but derives a session key that *differs* from the responder's.
That is implicit rejection in action: integrity cannot come from a decapsulation
error, so it must come from the fact that the two sides now hold different keys
and any subsequent MAC/AEAD will fail. `Decapsulate` only returns an error for a
structurally invalid ciphertext (wrong length), which is a malformed-input case,
not a tamper-detection mechanism.

### Deterministic regeneration from a seed

Key rotation and disaster recovery need a decapsulation key to be reconstructable
from stored material. In `crypto/mlkem` the value returned by
`DecapsulationKey768.Bytes()` is the 64-byte seed, and `NewDecapsulationKey768`
rebuilds the key from that seed deterministically — feeding the same seed back in
reproduces the identical encapsulation key bytes. `RegenerateEncapKey` exercises
this: persist the seed, restore the key, and the public encapsulation key comes
back byte-for-byte. Note there is no seed for `Encapsulate`: it draws its own
randomness internally and is non-deterministic by design, so the responder's side
cannot be seeded.

Create `hybridkem.go`:

```go
package hybridkem

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// hkdfInfo domain-separates this KDF use from any other use of the same secret.
const hkdfInfo = "hybridkem/v1 X25519MLKEM768 session key"

// Sentinel errors, wrapped with %w so callers can match with errors.Is.
var (
	ErrMalformedEncapKey   = errors.New("hybridkem: malformed ML-KEM encapsulation key")
	ErrMalformedCiphertext = errors.New("hybridkem: malformed ML-KEM ciphertext")
	ErrMalformedPublicKey  = errors.New("hybridkem: malformed X25519 public key")
	ErrMalformedSeed       = errors.New("hybridkem: malformed decapsulation-key seed")
)

// OfferMessage is the initiator's public material on the wire: the ML-KEM
// encapsulation key (EncapsulationKeySize768 = 1184 bytes) and the X25519
// public key (32 bytes).
type OfferMessage struct {
	MLKEMEncapKey []byte
	X25519Public  []byte
}

// AcceptMessage is the responder's reply: the ML-KEM ciphertext
// (CiphertextSize768 = 1088 bytes) and the responder's X25519 public key.
type AcceptMessage struct {
	MLKEMCiphertext []byte
	X25519Public    []byte
}

// Party holds the initiator's private state between Offer and Complete.
type Party struct {
	mlkemDK *mlkem.DecapsulationKey768
	x25519  *ecdh.PrivateKey
}

// Offer generates the initiator's ML-KEM and X25519 keypairs and returns the
// private state plus the public offer message.
func Offer() (*Party, *OfferMessage, error) {
	dk, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, nil, fmt.Errorf("hybridkem: generate ML-KEM key: %w", err)
	}
	xk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("hybridkem: generate X25519 key: %w", err)
	}
	msg := &OfferMessage{
		MLKEMEncapKey: dk.EncapsulationKey().Bytes(),
		X25519Public:  xk.PublicKey().Bytes(),
	}
	return &Party{mlkemDK: dk, x25519: xk}, msg, nil
}

// Accept is run by the responder. It encapsulates against the initiator's
// ML-KEM key, completes X25519 against the initiator's public key, derives the
// session key, and returns the accept message to send back.
func Accept(offer *OfferMessage) (*AcceptMessage, []byte, error) {
	ek, err := mlkem.NewEncapsulationKey768(offer.MLKEMEncapKey)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrMalformedEncapKey, err)
	}
	mlkemShared, ciphertext := ek.Encapsulate()

	peerPub, err := ecdh.X25519().NewPublicKey(offer.X25519Public)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrMalformedPublicKey, err)
	}
	xk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("hybridkem: generate X25519 key: %w", err)
	}
	x25519Shared, err := xk.ECDH(peerPub)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrMalformedPublicKey, err)
	}

	session, err := combine(mlkemShared, x25519Shared)
	if err != nil {
		return nil, nil, err
	}
	msg := &AcceptMessage{
		MLKEMCiphertext: ciphertext,
		X25519Public:    xk.PublicKey().Bytes(),
	}
	return msg, session, nil
}

// Complete is run by the initiator on the responder's accept message. It
// decapsulates the ML-KEM secret, completes X25519, and derives the same
// session key. Decapsulate does NOT error on a tampered ciphertext; it returns
// a different key (implicit rejection), so a mismatch surfaces only downstream.
func (p *Party) Complete(accept *AcceptMessage) ([]byte, error) {
	mlkemShared, err := p.mlkemDK.Decapsulate(accept.MLKEMCiphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedCiphertext, err)
	}
	peerPub, err := ecdh.X25519().NewPublicKey(accept.X25519Public)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedPublicKey, err)
	}
	x25519Shared, err := p.x25519.ECDH(peerPub)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedPublicKey, err)
	}
	return combine(mlkemShared, x25519Shared)
}

// MLKEMSeed returns the 64-byte seed of the initiator's decapsulation key.
// Persist this to reconstruct the key later with RegenerateEncapKey.
func (p *Party) MLKEMSeed() []byte {
	return p.mlkemDK.Bytes()
}

// RegenerateEncapKey rebuilds a decapsulation key from its 64-byte seed and
// returns the matching encapsulation-key bytes, proving deterministic restore.
func RegenerateEncapKey(seed []byte) ([]byte, error) {
	dk, err := mlkem.NewDecapsulationKey768(seed)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedSeed, err)
	}
	return dk.EncapsulationKey().Bytes(), nil
}

// combine concatenates the ML-KEM secret (first) with the X25519 secret and
// derives a 32-byte session key with HKDF. Concatenate-then-KDF is what makes
// the key safe if EITHER primitive holds; XOR would not.
func combine(mlkemShared, x25519Shared []byte) ([]byte, error) {
	secret := make([]byte, 0, len(mlkemShared)+len(x25519Shared))
	secret = append(secret, mlkemShared...)
	secret = append(secret, x25519Shared...)
	key, err := hkdf.Key(sha256.New, secret, nil, hkdfInfo, mlkem.SharedKeySize)
	if err != nil {
		return nil, fmt.Errorf("hybridkem: derive session key: %w", err)
	}
	return key, nil
}
```

### The runnable demo

The demo runs the full three-message exchange in one process and prints the wire
sizes and whether both derived session keys agree.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log"

	"example.com/hybridkem"
)

func main() {
	party, offer, err := hybridkem.Offer()
	if err != nil {
		log.Fatal(err)
	}

	accept, responderKey, err := hybridkem.Accept(offer)
	if err != nil {
		log.Fatal(err)
	}

	initiatorKey, err := party.Complete(accept)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("ML-KEM encapsulation key: %d bytes\n", len(offer.MLKEMEncapKey))
	fmt.Printf("ML-KEM ciphertext: %d bytes\n", len(accept.MLKEMCiphertext))
	fmt.Printf("derived session key: %d bytes\n", len(initiatorKey))
	fmt.Printf("session keys match: %v\n", bytes.Equal(initiatorKey, responderKey))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ML-KEM encapsulation key: 1184 bytes
ML-KEM ciphertext: 1088 bytes
derived session key: 32 bytes
session keys match: true
```

### Tests

The tests pin every claim the prose makes. Round-trip proves both sides derive
the same key. The wire-size table pins the framing against the exported `mlkem`
constants so a future change is caught. The regen test proves seed-based restore.
The tamper test proves implicit rejection: a flipped ciphertext byte yields *no*
error but a *different* key. The malformed-input table proves each sentinel error
is wrapped and matchable with `errors.Is`.

Create `hybridkem_test.go`:

```go
package hybridkem

import (
	"bytes"
	"crypto/mlkem"
	"errors"
	"fmt"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	party, offer, err := Offer()
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	accept, responderKey, err := Accept(offer)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	initiatorKey, err := party.Complete(accept)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !bytes.Equal(initiatorKey, responderKey) {
		t.Fatal("initiator and responder derived different session keys")
	}
	if len(initiatorKey) != mlkem.SharedKeySize {
		t.Fatalf("session key = %d bytes; want %d", len(initiatorKey), mlkem.SharedKeySize)
	}
}

func TestWireSizes(t *testing.T) {
	t.Parallel()
	_, offer, err := Offer()
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	accept, _, err := Accept(offer)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"encapsulation key", len(offer.MLKEMEncapKey), mlkem.EncapsulationKeySize768},
		{"ciphertext", len(accept.MLKEMCiphertext), mlkem.CiphertextSize768},
		{"x25519 offer public", len(offer.X25519Public), 32},
		{"x25519 accept public", len(accept.X25519Public), 32},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %d bytes; want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestDeterministicRegen(t *testing.T) {
	t.Parallel()
	party, offer, err := Offer()
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	seed := party.MLKEMSeed()
	if len(seed) != mlkem.SeedSize {
		t.Fatalf("seed = %d bytes; want %d", len(seed), mlkem.SeedSize)
	}
	regen, err := RegenerateEncapKey(seed)
	if err != nil {
		t.Fatalf("RegenerateEncapKey: %v", err)
	}
	if !bytes.Equal(regen, offer.MLKEMEncapKey) {
		t.Fatal("regenerated encapsulation key differs from the original")
	}
}

func TestTamperedCiphertextYieldsDifferentKey(t *testing.T) {
	t.Parallel()
	party, offer, err := Offer()
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	accept, responderKey, err := Accept(offer)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	// Flip one bit of the ciphertext. Length is unchanged, so Decapsulate does
	// not error; implicit rejection returns a different pseudo-random secret.
	accept.MLKEMCiphertext[0] ^= 0x01
	initiatorKey, err := party.Complete(accept)
	if err != nil {
		t.Fatalf("Complete returned an error on a tampered ciphertext; "+
			"implicit rejection should return a different key, not an error: %v", err)
	}
	if bytes.Equal(initiatorKey, responderKey) {
		t.Fatal("tampered ciphertext produced the same session key")
	}
}

func TestMalformedInputs(t *testing.T) {
	t.Parallel()
	_, goodOffer, err := Offer()
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	party, offer2, err := Offer()
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	goodAccept, _, err := Accept(offer2)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{
			name: "short encapsulation key",
			call: func() error {
				_, _, e := Accept(&OfferMessage{MLKEMEncapKey: []byte{1, 2, 3}, X25519Public: goodOffer.X25519Public})
				return e
			},
			want: ErrMalformedEncapKey,
		},
		{
			name: "short offer public key",
			call: func() error {
				_, _, e := Accept(&OfferMessage{MLKEMEncapKey: goodOffer.MLKEMEncapKey, X25519Public: []byte{1, 2, 3}})
				return e
			},
			want: ErrMalformedPublicKey,
		},
		{
			name: "short ciphertext",
			call: func() error {
				_, e := party.Complete(&AcceptMessage{MLKEMCiphertext: []byte{1, 2, 3}, X25519Public: goodAccept.X25519Public})
				return e
			},
			want: ErrMalformedCiphertext,
		},
		{
			name: "short seed",
			call: func() error {
				_, e := RegenerateEncapKey([]byte{1, 2, 3})
				return e
			},
			want: ErrMalformedSeed,
		},
	}
	for _, tt := range tests {
		if err := tt.call(); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v; want errors.Is %v", tt.name, err, tt.want)
		}
	}
}

func Example() {
	party, offer, err := Offer()
	if err != nil {
		panic(err)
	}
	accept, responderKey, err := Accept(offer)
	if err != nil {
		panic(err)
	}
	initiatorKey, err := party.Complete(accept)
	if err != nil {
		panic(err)
	}
	fmt.Println("encapsulation key bytes:", len(offer.MLKEMEncapKey))
	fmt.Println("ciphertext bytes:", len(accept.MLKEMCiphertext))
	fmt.Println("session key bytes:", len(initiatorKey))
	fmt.Println("session keys match:", bytes.Equal(initiatorKey, responderKey))
	// Output:
	// encapsulation key bytes: 1184
	// ciphertext bytes: 1088
	// session key bytes: 32
	// session keys match: true
}
```

## Review

The construction is correct when both sides derive an identical 32-byte key from
the same concatenate-then-KDF combiner and nothing else. The most valuable thing
to internalize is the tamper test: `Decapsulate` returning a *different* key
rather than an *error* on a corrupted ciphertext is not a bug you should try to
"fix" with an error check — it is the specified implicit-rejection behavior, and
it is why integrity must live in the KDF/AEAD downstream. Code that inspects a
decapsulation error to decide whether a message was tampered with is broken.

The mistakes to avoid: combining the two secrets with XOR (which loses the
security-if-either-holds property), swapping the concatenation order on one side
(the keys silently stop matching), and conflating the 64-byte seed with some
other serialization when persisting a key for rotation — `Bytes()` returns the
seed, and that is exactly what `NewDecapsulationKey768` consumes. Run
`go test -race` to confirm the round trip and every negative case; the wire-size
table will catch any drift in the ML-KEM framing constants.

## Resources

- [`crypto/mlkem`](https://pkg.go.dev/crypto/mlkem) — `GenerateKey768`, `Encapsulate`, `Decapsulate`, `NewDecapsulationKey768`, and the size constants.
- [`crypto/ecdh`](https://pkg.go.dev/crypto/ecdh) — `X25519`, `GenerateKey`, `NewPublicKey`, and `ECDH`.
- [`crypto/hkdf`](https://pkg.go.dev/crypto/hkdf) — the `Key` one-shot extract-and-expand helper.
- [FIPS 203, ML-KEM](https://csrc.nist.gov/pubs/fips/203/final) — the standard behind `crypto/mlkem`, including implicit rejection.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-negotiate-pq-tls.md](02-negotiate-pq-tls.md)
