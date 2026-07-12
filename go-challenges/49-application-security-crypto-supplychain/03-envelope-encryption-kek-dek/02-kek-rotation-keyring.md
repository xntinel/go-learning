# Exercise 2: KEK rotation with a versioned keyring and rewrap

Rotation is where envelope encryption pays off. This exercise builds a `Keyring`
that holds several KEK versions with one designated active, seals new objects
under the active KEK, opens old objects by looking up whichever KEK wrapped them,
and — the crucial operation — `Rewrap`s an envelope onto the active KEK *without
re-encrypting the bulk ciphertext*. That is how rotation scales to billions of
objects: the cost is O(number of objects), not O(bytes of data).

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
keyring/                    independent module: example.com/keyring
  go.mod                    go 1.24 (NewGCMWithRandomNonce needs it)
  keyring.go                type Keyring; New, Add, SetActive, Remove, Seal, Open, Rewrap
  cmd/
    demo/
      main.go               rotate-then-background-rewrap loop
  keyring_test.go           unwrap-after-rotate, rewrap-keeps-ciphertext, unknown-KEK tests
```

- Files: `keyring.go`, `cmd/demo/main.go`, `keyring_test.go`.
- Implement: a `Keyring` of `map[string][]byte` KEK versions with an active id; `Seal` wraps under active and stamps its id; `Open` looks up the KEK by the envelope's `KEKID`; `Rewrap` rewraps the DEK onto the active KEK leaving the ciphertext byte-identical.
- Test: unwrap-after-rotate; rewrap keeps `Ciphertext` bytes and still opens; new seal stamps the new id; unknown-`KEKID` returns `ErrUnknownKEK`; a retired KEK's rewrapped envelopes still open.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/03-envelope-encryption-kek-dek/02-kek-rotation-keyring/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/03-envelope-encryption-kek-dek/02-kek-rotation-keyring
go mod edit -go=1.24
```

### The keyring: many KEKs, one active

A single KEK is not enough the instant you rotate, because rotation is a period,
not an event: envelopes written under the old KEK must keep opening while new ones
are written under the new KEK. The `Keyring` models exactly that — a map from
`KEKID` to KEK bytes, plus the id of the currently *active* one. `Seal` always
wraps under the active KEK and stamps that id into the envelope. `Open` ignores
"active" entirely and looks up the KEK named by the envelope's `KEKID`, so an
envelope written three rotations ago still opens as long as its KEK is still in
the ring.

An unknown `KEKID` is a first-class error: it means either the keyring is
misconfigured or a KEK was retired too early (before its envelopes were
rewrapped). `Open` returns `ErrUnknownKEK` wrapped with `%w` so callers can detect
this precise condition with `errors.Is` and treat it as an operational alarm
rather than a generic decrypt failure.

The keyring guards its map with an `sync.RWMutex` because in a real service reads
(`Open`) vastly outnumber writes (`Add`, `SetActive`, `Remove`), and rotation may
run concurrently with serving traffic. `Seal`, `Open`, and `Rewrap` take only a
read lock long enough to copy out the KEK bytes they need, then do the crypto
outside the lock.

### Rewrap: the O(objects) operation

`Rewrap` is the heart of rotation. It unwraps the DEK using whichever KEK
originally wrapped it (found via the envelope's `KEKID`), re-wraps that same DEK
under the *active* KEK, and updates the envelope's `KEKID` and `WrappedDEK`. What
it pointedly does *not* do is decrypt or re-encrypt the payload: the `Ciphertext`
field is carried through untouched, byte for byte. The DEK never changed, so the
data it protects never needs to move. A background job can walk the entire object
store calling `Rewrap` on each envelope; each call is a pair of 32-byte AEAD
operations regardless of whether the object is a kilobyte or a gigabyte.

Once every envelope that referenced the old KEK has been rewrapped, the old KEK
can be removed from the ring. Retiring it earlier would strand any not-yet-rewrapped
envelope as undecryptable ciphertext, which is data loss — so `Remove` is the last
step of a rotation, never the first.

The private helpers `wrap` and `unwrap` isolate the AEAD mechanics so `Seal`,
`Open`, and `Rewrap` all share one correct implementation. As in Exercise 1,
`newAEAD` enforces the 32-byte AES-256 policy and uses `NewGCMWithRandomNonce`, so
nonces are generated and prepended automatically and every unwrap failure is
surfaced as `ErrUnwrap`.

Create `keyring.go`:

```go
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
)

const dekSize = 32

// Sentinel errors let callers branch on the failure kind with errors.Is.
var (
	ErrKEKSize    = errors.New("keyring: KEK must be 32 bytes (AES-256)")
	ErrUnknownKEK = errors.New("keyring: unknown KEK id")
	ErrUnwrap     = errors.New("keyring: DEK unwrap failed")
	ErrDecrypt    = errors.New("keyring: data decrypt failed")
)

// Envelope carries the id of the KEK that wrapped its DEK, so a rolling keyring
// can decrypt old data during and after rotation.
type Envelope struct {
	Version    int    `json:"version"`
	KEKID      string `json:"kek_id"`
	WrappedDEK []byte `json:"wrapped_dek"`
	Ciphertext []byte `json:"ciphertext"`
}

// Keyring holds multiple KEK versions with one designated active KEK.
type Keyring struct {
	mu     sync.RWMutex
	keks   map[string][]byte
	active string
}

// New creates a keyring whose only KEK is active from the start.
func New(activeID string, activeKEK []byte) (*Keyring, error) {
	if len(activeKEK) != 32 {
		return nil, ErrKEKSize
	}
	return &Keyring{
		keks:   map[string][]byte{activeID: activeKEK},
		active: activeID,
	}, nil
}

// Add registers another KEK version without changing which one is active.
func (k *Keyring) Add(id string, kek []byte) error {
	if len(kek) != 32 {
		return ErrKEKSize
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keks[id] = kek
	return nil
}

// SetActive designates an already-registered KEK as the one new seals use.
func (k *Keyring) SetActive(id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.keks[id]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKEK, id)
	}
	k.active = id
	return nil
}

// Remove retires a KEK. Only safe once every envelope that referenced it has
// been rewrapped; the active KEK cannot be removed.
func (k *Keyring) Remove(id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if id == k.active {
		return fmt.Errorf("keyring: cannot remove the active KEK %q", id)
	}
	if _, ok := k.keks[id]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKEK, id)
	}
	delete(k.keks, id)
	return nil
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

func wrap(kek, dek []byte) ([]byte, error) {
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nil, dek, nil), nil
}

func unwrap(kek, wrapped []byte) ([]byte, error) {
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	dek, err := aead.Open(nil, nil, wrapped, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnwrap, err)
	}
	return dek, nil
}

// lookup copies out the KEK bytes for id under a read lock.
func (k *Keyring) lookup(id string) ([]byte, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	kek, ok := k.keks[id]
	return kek, ok
}

// activeKEK copies out the active id and its bytes under a read lock.
func (k *Keyring) activeKEK() (string, []byte) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.active, k.keks[k.active]
}

// Seal mints a fresh DEK, encrypts plaintext under it, and wraps the DEK under
// the active KEK, stamping the active id into the envelope.
func (k *Keyring) Seal(plaintext []byte) (Envelope, error) {
	activeID, activeKEK := k.activeKEK()

	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return Envelope{}, fmt.Errorf("keyring: generate DEK: %w", err)
	}

	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return Envelope{}, err
	}
	ciphertext := dataAEAD.Seal(nil, nil, plaintext, nil)

	wrapped, err := wrap(activeKEK, dek)
	if err != nil {
		return Envelope{}, err
	}

	return Envelope{
		Version:    1,
		KEKID:      activeID,
		WrappedDEK: wrapped,
		Ciphertext: ciphertext,
	}, nil
}

// Open looks the KEK up by the envelope's KEKID, unwraps the DEK, and decrypts
// the payload. An unknown KEKID returns ErrUnknownKEK.
func (k *Keyring) Open(env Envelope) ([]byte, error) {
	kek, ok := k.lookup(env.KEKID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKEK, env.KEKID)
	}
	dek, err := unwrap(kek, env.WrappedDEK)
	if err != nil {
		return nil, err
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

// Rewrap unwraps the DEK with the KEK that originally wrapped it and re-wraps it
// under the active KEK, updating KEKID and WrappedDEK. The bulk Ciphertext is
// returned untouched: rotation is O(number of objects), not O(bytes of data).
func (k *Keyring) Rewrap(env Envelope) (Envelope, error) {
	oldKEK, ok := k.lookup(env.KEKID)
	if !ok {
		return Envelope{}, fmt.Errorf("%w: %q", ErrUnknownKEK, env.KEKID)
	}
	activeID, activeKEK := k.activeKEK()

	dek, err := unwrap(oldKEK, env.WrappedDEK)
	if err != nil {
		return Envelope{}, err
	}
	wrapped, err := wrap(activeKEK, dek)
	if err != nil {
		return Envelope{}, err
	}

	env.KEKID = activeID
	env.WrappedDEK = wrapped
	// env.Ciphertext is intentionally left as-is.
	return env, nil
}
```

### The runnable demo

The demo seals three secrets under `kek-v1`, rotates to `kek-v2`, runs a
background-style rewrap loop over the envelopes (printing that each ciphertext is
unchanged), then retires `kek-v1` and confirms every envelope still decrypts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"log"

	"example.com/keyring"
)

func mustKEK() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		log.Fatal(err)
	}
	return k
}

func main() {
	kr, err := keyring.New("kek-v1", mustKEK())
	if err != nil {
		log.Fatal(err)
	}

	secrets := [][]byte{
		[]byte("password: hunter2"),
		[]byte("token: abc123"),
		[]byte("pin: 4242"),
	}
	envs := make([]keyring.Envelope, len(secrets))
	for i, s := range secrets {
		if envs[i], err = kr.Seal(s); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("sealed %d envelopes under %s\n", len(envs), envs[0].KEKID)

	// Rotate: introduce v2 and make it active.
	if err := kr.Add("kek-v2", mustKEK()); err != nil {
		log.Fatal(err)
	}
	if err := kr.SetActive("kek-v2"); err != nil {
		log.Fatal(err)
	}
	fmt.Println("rotated active KEK to kek-v2")

	// Background rewrap loop: rewrap each envelope onto the active KEK without
	// re-encrypting the bulk ciphertext.
	for i := range envs {
		before := envs[i].Ciphertext
		if envs[i], err = kr.Rewrap(envs[i]); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("rewrapped envelope %d: now %s, ciphertext unchanged: %v\n",
			i, envs[i].KEKID, bytes.Equal(before, envs[i].Ciphertext))
	}

	// Retire the old KEK; every envelope must still open.
	if err := kr.Remove("kek-v1"); err != nil {
		log.Fatal(err)
	}
	allOK := true
	for i := range envs {
		pt, err := kr.Open(envs[i])
		if err != nil || !bytes.Equal(pt, secrets[i]) {
			allOK = false
		}
	}
	fmt.Printf("retired kek-v1; all envelopes still decrypt: %v\n", allOK)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sealed 3 envelopes under kek-v1
rotated active KEK to kek-v2
rewrapped envelope 0: now kek-v2, ciphertext unchanged: true
rewrapped envelope 1: now kek-v2, ciphertext unchanged: true
rewrapped envelope 2: now kek-v2, ciphertext unchanged: true
retired kek-v1; all envelopes still decrypt: true
```

### Tests

`TestUnwrapAfterRotate` seals under v1, rotates to v2, and confirms the old
envelope still opens — the core requirement. `TestRewrapKeepsCiphertext` is the
one that proves the economics: after `Rewrap`, the `KEKID` and `WrappedDEK` change
but the `Ciphertext` bytes are identical (`bytes.Equal`) and the recovered
plaintext is unchanged. `TestSealStampsActive` shows a post-rotation seal carries
the new id. `TestUnknownKEK` asserts `ErrUnknownKEK` via `errors.Is`.
`TestRemoveAfterRewrap` walks the full lifecycle: rewrap, then retire the old KEK,
then still decrypt.

Create `keyring_test.go`:

```go
package keyring

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

func newRotatedRing(t *testing.T) *Keyring {
	t.Helper()
	kr, err := New("kek-v1", mustKEK(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := kr.Add("kek-v2", mustKEK(t)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return kr
}

func TestUnwrapAfterRotate(t *testing.T) {
	t.Parallel()
	kr := newRotatedRing(t)
	env, err := kr.Seal([]byte("old secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if env.KEKID != "kek-v1" {
		t.Fatalf("sealed under %q; want kek-v1", env.KEKID)
	}
	if err := kr.SetActive("kek-v2"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	got, err := kr.Open(env)
	if err != nil {
		t.Fatalf("Open after rotate: %v", err)
	}
	if !bytes.Equal(got, []byte("old secret")) {
		t.Fatalf("Open = %q; want old secret", got)
	}
}

func TestRewrapKeepsCiphertext(t *testing.T) {
	t.Parallel()
	kr := newRotatedRing(t)
	env, err := kr.Seal([]byte("payload"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	original := append([]byte(nil), env.Ciphertext...)
	if err := kr.SetActive("kek-v2"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	rewrapped, err := kr.Rewrap(env)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if rewrapped.KEKID != "kek-v2" {
		t.Fatalf("rewrapped KEKID = %q; want kek-v2", rewrapped.KEKID)
	}
	if bytes.Equal(rewrapped.WrappedDEK, env.WrappedDEK) {
		t.Fatal("WrappedDEK unchanged after Rewrap; expected a new wrap")
	}
	if !bytes.Equal(rewrapped.Ciphertext, original) {
		t.Fatal("Ciphertext changed during Rewrap; rotation must not re-encrypt data")
	}
	got, err := kr.Open(rewrapped)
	if err != nil {
		t.Fatalf("Open after Rewrap: %v", err)
	}
	if !bytes.Equal(got, []byte("payload")) {
		t.Fatalf("Open = %q; want payload", got)
	}
}

func TestSealStampsActive(t *testing.T) {
	t.Parallel()
	kr := newRotatedRing(t)
	if err := kr.SetActive("kek-v2"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	env, err := kr.Seal([]byte("new secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if env.KEKID != "kek-v2" {
		t.Fatalf("sealed under %q; want kek-v2", env.KEKID)
	}
}

func TestUnknownKEK(t *testing.T) {
	t.Parallel()
	kr := newRotatedRing(t)
	env, err := kr.Seal([]byte("x"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	env.KEKID = "kek-v99"
	if _, err := kr.Open(env); !errors.Is(err, ErrUnknownKEK) {
		t.Fatalf("Open with unknown KEKID = %v; want ErrUnknownKEK", err)
	}
}

func TestRemoveAfterRewrap(t *testing.T) {
	t.Parallel()
	kr := newRotatedRing(t)
	env, err := kr.Seal([]byte("survives retirement"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := kr.SetActive("kek-v2"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if env, err = kr.Rewrap(env); err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if err := kr.Remove("kek-v1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, err := kr.Open(env)
	if err != nil {
		t.Fatalf("Open after Remove: %v", err)
	}
	if !bytes.Equal(got, []byte("survives retirement")) {
		t.Fatalf("Open = %q; want survives retirement", got)
	}
}

func Example() {
	// Fixed all-zero KEKs are used ONLY for a deterministic demo; never in
	// production.
	kr, _ := New("kek-v1", make([]byte, 32))
	env, _ := kr.Seal([]byte("secret"))

	_ = kr.Add("kek-v2", make([]byte, 32))
	_ = kr.SetActive("kek-v2")
	rewrapped, _ := kr.Rewrap(env)

	pt, _ := kr.Open(rewrapped)
	fmt.Printf("%s -> %s: %s\n", env.KEKID, rewrapped.KEKID, pt)
	// Output: kek-v1 -> kek-v2: secret
}
```

## Review

The keyring is correct when three invariants hold together. First, `Open` is
governed by the envelope's `KEKID`, never by which KEK happens to be active — that
is what keeps old data readable through rotation, and `TestUnwrapAfterRotate`
proves it. Second, `Rewrap` changes the wrap but not the payload: if
`TestRewrapKeepsCiphertext` ever sees the `Ciphertext` bytes move, you are
re-encrypting data and have thrown away the whole point of the pattern. Third, an
unknown `KEKID` is a distinguishable, wrapped error, so a prematurely retired KEK
shows up as `ErrUnknownKEK` rather than a vague failure.

The traps here are ordering and lifecycle. Do not remove a KEK before its
envelopes are rewrapped — `Remove` refuses to drop the active KEK, but it cannot
know whether stragglers still reference an inactive one, so that ordering is your
responsibility (rewrap first, retire last). Do not let `Rewrap` re-derive or
regenerate the DEK; it must unwrap the *existing* DEK so the existing ciphertext
stays valid. Two fixed all-zero KEKs in the `Example` decrypt correctly only
because the id-to-KEK mapping is what matters, not the key values — never do that
outside a demo. Run `go test -race` to confirm the `RWMutex` actually serializes
concurrent rotation against serving traffic.

## Resources

- [`crypto/cipher`](https://pkg.go.dev/crypto/cipher) — `AEAD` and `NewGCMWithRandomNonce`, the primitive behind both wrap and unwrap.
- [Google Cloud KMS: Envelope encryption](https://docs.cloud.google.com/kms/docs/envelope-encryption) — KEK/DEK, key rotation, and rewrap as an operational lifecycle.
- [AWS KMS concepts: envelope encryption](https://docs.aws.amazon.com/kms/latest/developerguide/concepts.html) — the same two-key hierarchy and why bulk data never goes to the KMS.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-dek-wrap-roundtrip.md](01-dek-wrap-roundtrip.md) | Next: [03-aad-context-binding.md](03-aad-context-binding.md)
