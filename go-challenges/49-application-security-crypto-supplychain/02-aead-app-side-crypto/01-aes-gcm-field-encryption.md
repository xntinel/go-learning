# Exercise 1: AES-256-GCM Authenticated Field Encryption with Associated Data

The most common application-side use of AEAD is encrypting a single sensitive
column — an email, an SSN, a phone number — on a database row. This exercise
builds a production-flavored field encryptor with AES-256-GCM using the classic
manual-nonce idiom, and binds each ciphertext to its owning record so it cannot
be silently moved to another row.

This module is fully self-contained: its own `go mod init`, its own demo, and its
own tests. Nothing here imports another exercise.

## What you'll build

```text
fieldcrypt/                 independent module: example.com/fieldcrypt
  go.mod                    go 1.24
  fieldcrypt.go             type Encryptor; New, Encrypt(recordID, pt), Decrypt(recordID, blob)
  cmd/
    demo/
      main.go               encrypt a field, tamper-detect, reject wrong record
  fieldcrypt_test.go        round-trip, integrity, AAD-mismatch, key-mismatch, length
```

- Files: `fieldcrypt.go`, `cmd/demo/main.go`, `fieldcrypt_test.go`.
- Implement: an `Encryptor` over AES-256-GCM with `Encrypt(recordID, plaintext)` and `Decrypt(recordID, blob)`, generating a fresh 12-byte random nonce per call, prepending it to the ciphertext, and binding `recordID` as associated data.
- Test: round-trip; single-byte tamper fails; wrong `recordID` fails; wrong key fails; two encrypts of the same input differ but both decrypt; blob length equals `NonceSize()+len(pt)+Overhead()`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/02-aead-app-side-crypto/01-aes-gcm-field-encryption/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/02-aead-app-side-crypto/01-aes-gcm-field-encryption
go mod edit -go=1.24
```

### Why AES-256-GCM, and why the manual-nonce idiom

`aes.NewCipher(key)` returns a block cipher; a 32-byte key selects AES-256.
`cipher.NewGCM(block)` wraps it in Galois Counter Mode, which is an AEAD: it
encrypts, authenticates the ciphertext, and authenticates the associated data in
one operation. GCM's standard nonce is 12 bytes and its tag is 16 bytes, so the
blob you store is exactly `12 + len(plaintext) + 16` bytes.

The classic idiom is `blob := aead.Seal(nonce, nonce, plaintext, aad)`. `Seal`
appends the ciphertext to its first argument (`dst`) and returns the grown
slice. Passing the freshly generated `nonce` slice as `dst` means the returned
blob is the nonce followed by the ciphertext-plus-tag — the nonce is prepended
for free, and `Decrypt` recovers it by slicing off the first `NonceSize()`
bytes. This works because `Seal` reads the nonce argument before it appends, and
the appended bytes land in the slice's spare capacity, which does not overlap the
plaintext. The nonce is not secret; it only has to be unique and recoverable,
and storing it in the clear next to the ciphertext satisfies both.

### Why the record ID is associated data

If you sealed the field with no AAD, an attacker (or a buggy migration) with
write access to the table could copy Bob's encrypted email into Alice's row, and
your code would decrypt it happily — a ciphertext-substitution attack. Passing
the record's stable identifier as associated data binds the ciphertext to that
row: the AAD is authenticated but not encrypted, and `Open` fails unless the same
`recordID` is presented at decrypt time. The record ID is not hidden — it is the
primary key, in plain sight — but the binding is unforgeable without the key.

The cost is a discipline: the AAD must be reconstructed byte-for-byte at decrypt
time. Here it is just `[]byte(recordID)`, so as long as callers pass the same
string, it round-trips. That is why `Decrypt` takes the `recordID` as a
parameter rather than trying to recover it from the blob.

### Handling the Open error

`Open` returns `(plaintext, error)`. A non-nil error means the ciphertext, the
tag, the nonce, the key, or the AAD did not agree — the data is unauthenticated
and must be discarded. We wrap the underlying failure in a package sentinel,
`ErrOpen`, so callers can branch on `errors.Is(err, ErrOpen)` without depending
on the exact string the standard library returns.

Create `fieldcrypt.go`:

```go
package fieldcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeySize is the required key length in bytes. 32 bytes selects AES-256.
const KeySize = 32

// Sentinel errors, wrapped with %w so callers can match with errors.Is.
var (
	// ErrKeySize is returned by New when the key is not KeySize bytes.
	ErrKeySize = errors.New("fieldcrypt: key must be 32 bytes")
	// ErrShortBlob is returned by Decrypt when the blob cannot hold a nonce.
	ErrShortBlob = errors.New("fieldcrypt: blob shorter than nonce")
	// ErrOpen is returned by Decrypt when authentication fails: wrong key,
	// wrong record ID (AAD), tampered ciphertext, or a corrupt nonce.
	ErrOpen = errors.New("fieldcrypt: authentication failed")
)

// Encryptor seals and opens a single sensitive field with AES-256-GCM, binding
// each ciphertext to its owning record via associated data.
type Encryptor struct {
	aead cipher.AEAD
}

// New builds an Encryptor from a 32-byte key.
func New(key []byte) (*Encryptor, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: got %d", ErrKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Encryptor{aead: aead}, nil
}

// Encrypt seals plaintext, binding recordID as associated data. The returned
// blob is nonce || ciphertext || tag. Two calls on identical input differ
// because the nonce is fresh and random each time.
func (e *Encryptor) Encrypt(recordID string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// Passing nonce as dst prepends it to the ciphertext in one call.
	return e.aead.Seal(nonce, nonce, plaintext, []byte(recordID)), nil
}

// Decrypt reverses Encrypt. recordID must equal the value passed to Encrypt or
// Open fails with ErrOpen. Any single-byte change to blob also fails.
func (e *Encryptor) Decrypt(recordID string, blob []byte) ([]byte, error) {
	ns := e.aead.NonceSize()
	if len(blob) < ns {
		return nil, ErrShortBlob
	}
	nonce, ciphertext := blob[:ns], blob[ns:]
	plaintext, err := e.aead.Open(nil, nonce, ciphertext, []byte(recordID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOpen, err)
	}
	return plaintext, nil
}

// Overhead reports the fixed number of bytes a blob adds over the plaintext:
// the nonce plus the authentication tag.
func (e *Encryptor) Overhead() int {
	return e.aead.NonceSize() + e.aead.Overhead()
}
```

### The runnable demo

The demo encrypts an email for a user record, prints the deterministic blob size
(nonce + payload + tag), decrypts it, then shows that both a flipped byte and a
mismatched record ID are rejected. The ciphertext itself is random, so the demo
prints sizes and outcomes rather than raw bytes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"

	"example.com/fieldcrypt"
)

func main() {
	key := make([]byte, fieldcrypt.KeySize)
	if _, err := rand.Read(key); err != nil {
		log.Fatal(err)
	}
	enc, err := fieldcrypt.New(key)
	if err != nil {
		log.Fatal(err)
	}

	const recordID = "user:42"
	email := []byte("alice@example.com")

	blob, err := enc.Encrypt(recordID, email)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("plaintext:  %s\n", email)
	fmt.Printf("blob:       %d bytes (nonce 12 + payload %d + tag 16)\n", len(blob), len(email))

	got, err := enc.Decrypt(recordID, blob)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("decrypted:  %s\n", got)

	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := enc.Decrypt(recordID, tampered); errors.Is(err, fieldcrypt.ErrOpen) {
		fmt.Println("tamper:     rejected")
	}

	if _, err := enc.Decrypt("user:99", blob); errors.Is(err, fieldcrypt.ErrOpen) {
		fmt.Println("wrong row:  rejected")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
plaintext:  alice@example.com
blob:       45 bytes (nonce 12 + payload 17 + tag 16)
decrypted:  alice@example.com
tamper:     rejected
wrong row:  rejected
```

### Tests

The tests pin every property a reviewer would ask about. Round-trip proves
correctness. The integrity test flips each byte of the blob in turn and asserts
`Open` fails on every one. The AAD test decrypts with a different record ID and
requires failure even though the key and nonce are untouched. The key test
proves a different key cannot open the blob. The randomness test seals identical
input twice and asserts the blobs differ yet both decrypt. The length test pins
`len(blob) == NonceSize()+len(pt)+Overhead()`.

Create `fieldcrypt_test.go`:

```go
package fieldcrypt

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

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	enc, err := New(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		recordID string
		pt       []byte
	}{
		{"empty", "user:1", []byte{}},
		{"short", "user:2", []byte("x")},
		{"email", "user:3", []byte("alice@example.com")},
		{"binary", "user:4", []byte{0x00, 0xff, 0x00, 0x7f}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			blob, err := enc.Encrypt(tc.recordID, tc.pt)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			got, err := enc.Decrypt(tc.recordID, blob)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, tc.pt) {
				t.Fatalf("round-trip = %q, want %q", got, tc.pt)
			}
		})
	}
}

func TestTamperEveryByteFails(t *testing.T) {
	t.Parallel()
	enc, err := New(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := enc.Encrypt("user:42", []byte("sensitive"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range blob {
		flipped := append([]byte(nil), blob...)
		flipped[i] ^= 0x01
		if _, err := enc.Decrypt("user:42", flipped); !errors.Is(err, ErrOpen) {
			t.Fatalf("flip byte %d: err = %v, want ErrOpen", i, err)
		}
	}
}

func TestWrongRecordIDFails(t *testing.T) {
	t.Parallel()
	enc, err := New(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := enc.Encrypt("user:42", []byte("sensitive"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Decrypt("user:43", blob); !errors.Is(err, ErrOpen) {
		t.Fatalf("AAD mismatch: err = %v, want ErrOpen", err)
	}
}

func TestWrongKeyFails(t *testing.T) {
	t.Parallel()
	enc, err := New(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := enc.Encrypt("user:42", []byte("sensitive"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := New(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Decrypt("user:42", blob); !errors.Is(err, ErrOpen) {
		t.Fatalf("wrong key: err = %v, want ErrOpen", err)
	}
}

func TestNonceIsFreshPerCall(t *testing.T) {
	t.Parallel()
	enc, err := New(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("same input")
	a, err := enc.Encrypt("user:42", pt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := enc.Encrypt("user:42", pt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two Encrypt calls produced identical blobs; nonce not fresh")
	}
	for _, blob := range [][]byte{a, b} {
		got, err := enc.Decrypt("user:42", blob)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("Decrypt = %q,%v; want %q,nil", got, err, pt)
		}
	}
}

func TestBlobLength(t *testing.T) {
	t.Parallel()
	enc, err := New(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("alice@example.com")
	blob, err := enc.Encrypt("user:42", pt)
	if err != nil {
		t.Fatal(err)
	}
	want := len(pt) + enc.Overhead()
	if len(blob) != want {
		t.Fatalf("blob length = %d, want %d", len(blob), want)
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	t.Parallel()
	if _, err := New(make([]byte, 16)); !errors.Is(err, ErrKeySize) {
		t.Fatalf("New(16 bytes): err = %v, want ErrKeySize", err)
	}
}

func TestDecryptShortBlob(t *testing.T) {
	t.Parallel()
	enc, err := New(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Decrypt("user:42", []byte{0x00, 0x01}); !errors.Is(err, ErrShortBlob) {
		t.Fatalf("short blob: err = %v, want ErrShortBlob", err)
	}
}

func TestDroppedRecordIDFails(t *testing.T) {
	t.Parallel()
	enc, err := New(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := enc.Encrypt("user:42", []byte("sensitive"))
	if err != nil {
		t.Fatal(err)
	}
	// Decrypting under the empty record ID drops the AAD entirely; the binding
	// must reject a removed record ID just as it rejects a changed one.
	if _, err := enc.Decrypt("", blob); !errors.Is(err, ErrOpen) {
		t.Fatalf("dropped record ID: err = %v, want ErrOpen", err)
	}
}

func Example() {
	// A fixed key keeps this example deterministic; real keys come from a KMS.
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	enc, err := New(key)
	if err != nil {
		panic(err)
	}
	blob, err := enc.Encrypt("user:42", []byte("alice@example.com"))
	if err != nil {
		panic(err)
	}
	got, err := enc.Decrypt("user:42", blob)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", got)
	// Output: alice@example.com
}
```

`TestDroppedRecordIDFails` closes the last gap: decrypting the same blob under
`""` (the empty record ID) must also fail with `ErrOpen`, proving the binding
rejects a dropped AAD, not just a changed one.

## Review

The encryptor is correct when `Open` succeeds only if the key, nonce, tag, and
associated data all agree, and fails otherwise. The tamper test is the strongest
evidence: flipping any single byte of the blob — whether it lands in the nonce,
the ciphertext, or the tag — must produce `ErrOpen`, and the loop checks every
position. The AAD test proves the record binding: a blob that decrypts fine
under `"user:42"` must fail under `"user:43"`, which is what stops
ciphertext-substitution across rows.

The mistakes to avoid are the ones the tests are built to catch. Do not reuse a
nonce: the fresh-per-call test fails if you ever hard-code or cache one. Do not
forget to prepend and re-slice the nonce; if you store it separately and lose
it, decryption is impossible, because the nonce is required (though not secret).
Do not swallow the `Open` error and return partial bytes — a non-nil error means
discard everything. And do not reconstruct the AAD differently on decrypt; here
it is a plain `[]byte(recordID)`, so the only way to break it is to pass a
different string. Run `go test -race` to confirm the encryptor is safe under
concurrent use, since one `Encryptor` is typically shared across request
goroutines.

## Resources

- [crypto/cipher: AEAD, NewGCM, Seal, Open](https://pkg.go.dev/crypto/cipher) — the interface and the append/overlap rules for `Seal`/`Open`.
- [crypto/aes: NewCipher](https://pkg.go.dev/crypto/aes) — key sizes and the AES block cipher.
- [crypto/rand: Read](https://pkg.go.dev/crypto/rand) — the source for nonces and keys; never returns a short read.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-cipher-agility-aead-envelope.md](02-cipher-agility-aead-envelope.md)
