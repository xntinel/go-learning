# Exercise 27: Encryption Key Version Rotation with On-The-Fly Re-Encryption

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A long-lived encrypted record outlives the key it was encrypted under —
keys get rotated on a schedule or after an incident, but old ciphertext
does not spontaneously re-encrypt itself. `NewKeyStore` closes over every
key version it has ever known plus the current one; its `decrypt` closure
detects when a blob was encrypted under an old version and transparently
re-encrypts the recovered plaintext under the current key before handing it
back, so the caller can persist the migrated blob and the record gradually
moves off the retired key as it is read — without a batch re-encryption job.

The cipher used here is a toy XOR-with-key transform, deliberately not real
cryptography. The exercise is the closure design around key versioning and
transparent migration, not building a cipher — swap in AES-GCM (or any
`cipher.AEAD`) in production and the `NewKeyStore` shape is unchanged.

## What you'll build

```text
key-rotation/                independent module: example.com/key-rotation
  go.mod                      go 1.24
  keyver.go                   Encrypted, NewKeyStore returns encrypt/decrypt/rotate
  cmd/
    demo/
      main.go                  encrypt under v1, rotate to v2, decrypt migrates
  keyver_test.go               table test: round-trip, migration, unknown version, concurrency
```

- Files: `keyver.go`, `cmd/demo/main.go`, `keyver_test.go`.
- Implement: `NewKeyStore(version int, key []byte) (encrypt func([]byte) Encrypted, decrypt func(Encrypted) ([]byte, Encrypted, error), rotate func(int, []byte))`, closing over a map of key versions and the current version.
- Test: encrypt/decrypt round-trips on the current version; decrypting a blob from a retired version returns correct plaintext and a re-encrypted blob stamped with the current version; encrypting after a rotation uses the new version; an unknown version errors; a concurrency test drives encrypt/decrypt/rotate from many goroutines behind a caller-supplied lock under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/key-rotation/cmd/demo
cd ~/go-exercises/key-rotation
go mod init example.com/key-rotation
go mod edit -go=1.24
```

### Re-encryption on read, not a migration job

`NewKeyStore` captures `keys`, a `map[int][]byte` from version number to key
material, and `current`, the version new encryptions use. `encrypt` always
uses `keys[current]` and stamps the result with `current`. `decrypt` looks
up the key for the *blob's own* version — so ciphertext written years ago
under version 1 still decrypts correctly after version 7 becomes current, as
long as version 1's key is still in the map. The re-encryption trick is the
last step: if the blob's version is not `current`, `decrypt` re-encrypts the
plaintext it just recovered under the current key and returns that as a
second value, `reencrypted Encrypted`. The caller is expected to write that
value back to storage — the record migrates off the old key on its next
read, with no separate batch job walking every row.

`rotate` never deletes an old key from the map; deleting it would break
`decrypt` for any blob not yet migrated. Real systems retire (and eventually
securely erase) an old key only once they can prove nothing still
references it — out of scope here, but worth knowing the map growing
unbounded is the trade-off this simple version makes.

The closures `NewKeyStore` returns are deliberately *not* internally
synchronized — they model a single-writer key store. A caller that shares
one store across goroutines wraps each call in its own lock, exactly like
using any non-thread-safe library type concurrently; the concurrency test
below does exactly that.

Create `keyver.go`:

```go
// Package keyver demonstrates key-version-aware re-encryption with a toy XOR
// cipher. The cipher is deliberately not real cryptography (XOR-with-key is
// trivially breakable) -- the exercise is about the closure design for
// transparent re-encryption on read, not about building a real cipher.
package keyver

import "fmt"

// xor XORs data with key, repeating key as needed. Its own inverse.
func xor(data, key []byte) []byte {
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = b ^ key[i%len(key)]
	}
	return out
}

// Encrypted bundles ciphertext with the key version it was encrypted under,
// so a store can decrypt data written under any key version it still knows.
type Encrypted struct {
	Version int
	Data    []byte
}

// NewKeyStore returns three closures sharing a private map of key versions
// to key material and the current version number:
//
//   - encrypt always encrypts under the current key and stamps the result
//     with the current version.
//   - decrypt looks up the key for the blob's own version (so ciphertext
//     written under an old key keeps decrypting after a rotation), and if
//     that version is not the current one, transparently re-encrypts the
//     recovered plaintext under the current key before returning it -- the
//     caller is expected to persist the returned Encrypted, migrating that
//     record off the old key the next time it is read.
//   - rotate installs a new current key version, without discarding old
//     key material, so previously written blobs keep decrypting.
func NewKeyStore(version int, key []byte) (
	encrypt func(plaintext []byte) Encrypted,
	decrypt func(e Encrypted) (plaintext []byte, reencrypted Encrypted, err error),
	rotate func(newVersion int, newKey []byte),
) {
	keys := map[int][]byte{version: append([]byte(nil), key...)}
	current := version

	encrypt = func(plaintext []byte) Encrypted {
		return Encrypted{Version: current, Data: xor(plaintext, keys[current])}
	}

	decrypt = func(e Encrypted) ([]byte, Encrypted, error) {
		k, ok := keys[e.Version]
		if !ok {
			return nil, Encrypted{}, fmt.Errorf("keyver: unknown key version %d", e.Version)
		}
		plaintext := xor(e.Data, k)
		if e.Version == current {
			return plaintext, e, nil
		}
		fresh := Encrypted{Version: current, Data: xor(plaintext, keys[current])}
		return plaintext, fresh, nil
	}

	rotate = func(newVersion int, newKey []byte) {
		keys[newVersion] = append([]byte(nil), newKey...)
		current = newVersion
	}

	return encrypt, decrypt, rotate
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/key-rotation"
)

func main() {
	encrypt, decrypt, rotate := keyver.NewKeyStore(1, []byte("v1-secret"))

	blob := encrypt([]byte("account-balance:500"))
	fmt.Printf("encrypted under version %d\n", blob.Version)

	rotate(2, []byte("v2-secret-longer"))

	plaintext, fresh, err := decrypt(blob)
	if err != nil {
		fmt.Println("decrypt error:", err)
		return
	}
	fmt.Printf("decrypted old blob: %s\n", plaintext)
	fmt.Printf("re-encrypted under version %d\n", fresh.Version)

	plaintext2, fresh2, _ := decrypt(fresh)
	fmt.Printf("decrypted migrated blob: %s (version %d, unchanged)\n", plaintext2, fresh2.Version)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
encrypted under version 1
decrypted old blob: account-balance:500
re-encrypted under version 2
decrypted migrated blob: account-balance:500 (version 2, unchanged)
```

### Tests

Create `keyver_test.go`:

```go
package keyver

import (
	"sync"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	encrypt, decrypt, _ := NewKeyStore(1, []byte("key-one"))

	blob := encrypt([]byte("hello"))
	if blob.Version != 1 {
		t.Fatalf("blob.Version = %d, want 1", blob.Version)
	}

	plaintext, fresh, err := decrypt(blob)
	if err != nil {
		t.Fatalf("decrypt() error = %v", err)
	}
	if string(plaintext) != "hello" {
		t.Fatalf("plaintext = %q, want %q", plaintext, "hello")
	}
	if fresh.Version != 1 {
		t.Fatalf("fresh.Version = %d, want 1 (no rotation happened yet)", fresh.Version)
	}
}

func TestDecryptOldBlobReencryptsUnderCurrentVersion(t *testing.T) {
	encrypt, decrypt, rotate := NewKeyStore(1, []byte("key-one"))

	blob := encrypt([]byte("secret-data"))
	rotate(2, []byte("key-two-longer"))

	plaintext, fresh, err := decrypt(blob)
	if err != nil {
		t.Fatalf("decrypt() error = %v", err)
	}
	if string(plaintext) != "secret-data" {
		t.Fatalf("plaintext = %q, want %q", plaintext, "secret-data")
	}
	if fresh.Version != 2 {
		t.Fatalf("fresh.Version = %d, want 2 (re-encrypted under current key)", fresh.Version)
	}
	if string(fresh.Data) == string(blob.Data) {
		t.Fatal("fresh.Data equals the old ciphertext, want re-encrypted bytes")
	}

	// The migrated blob must keep decrypting correctly, and now decrypting
	// it a second time (already current) must not change its version again.
	plaintext2, fresh2, err := decrypt(fresh)
	if err != nil {
		t.Fatalf("decrypt(fresh) error = %v", err)
	}
	if string(plaintext2) != "secret-data" {
		t.Fatalf("plaintext2 = %q, want %q", plaintext2, "secret-data")
	}
	if fresh2.Version != 2 {
		t.Fatalf("fresh2.Version = %d, want 2 (already current, unchanged)", fresh2.Version)
	}
}

func TestEncryptAfterRotateUsesNewVersion(t *testing.T) {
	encrypt, _, rotate := NewKeyStore(1, []byte("key-one"))
	rotate(2, []byte("key-two-longer"))

	blob := encrypt([]byte("fresh-data"))
	if blob.Version != 2 {
		t.Fatalf("blob.Version = %d, want 2", blob.Version)
	}
}

func TestDecryptUnknownVersionErrors(t *testing.T) {
	_, decrypt, _ := NewKeyStore(1, []byte("key-one"))

	_, _, err := decrypt(Encrypted{Version: 99, Data: []byte("garbage")})
	if err == nil {
		t.Fatal("decrypt() error = nil, want error for unknown key version")
	}
}

func TestKeyStoreConcurrentEncryptDecryptRotate(t *testing.T) {
	var mu sync.Mutex
	encryptUnsafe, decryptUnsafe, rotateUnsafe := NewKeyStore(1, []byte("key-one"))

	// The closures returned by NewKeyStore are not internally synchronized
	// (they model a single-writer key store); callers that share one store
	// across goroutines add their own lock around each call, exactly like
	// callers of a non-thread-safe library type would.
	encrypt := func(p []byte) Encrypted {
		mu.Lock()
		defer mu.Unlock()
		return encryptUnsafe(p)
	}
	decrypt := func(e Encrypted) ([]byte, Encrypted, error) {
		mu.Lock()
		defer mu.Unlock()
		return decryptUnsafe(e)
	}
	rotate := func(v int, k []byte) {
		mu.Lock()
		defer mu.Unlock()
		rotateUnsafe(v, k)
	}

	blob := encrypt([]byte("payload"))

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%5 == 0 {
				rotate(10+i, []byte("rotated-key-material"))
				return
			}
			plaintext, _, err := decrypt(blob)
			if err != nil {
				t.Errorf("decrypt() error = %v", err)
				return
			}
			if string(plaintext) != "payload" {
				t.Errorf("plaintext = %q, want %q", plaintext, "payload")
			}
		}(i)
	}
	wg.Wait()
}
```

Verify: `go test -count=1 -race ./...`

## Review

The round-trip and migration tests are the exercise's core contract: a blob
encrypted under a retired version still decrypts correctly, and doing so
hands back a re-encrypted blob stamped with the current version — the
transparent-migration behavior the whole module exists to demonstrate. The
unknown-version test guards the failure mode of deleting a key while blobs
still reference it. The concurrency test proves the caller-supplied lock
pattern is sufficient: many goroutines interleaving `decrypt` calls with a
`rotate` never corrupt the map or return wrong plaintext under `-race`.

## Resources

- [pkg.go.dev: crypto/cipher AEAD](https://pkg.go.dev/crypto/cipher#AEAD) — the real interface a production `encrypt`/`decrypt` pair would wrap instead of XOR.
- [NIST SP 800-57: Key Management](https://csrc.nist.gov/pubs/sp/800/57/pt1/r5/final) — guidance on key rotation and retiring old key versions.
- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — the caller-side lock the concurrency test wraps each closure call in.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-compensating-transaction-unwinding-stack.md](26-compensating-transaction-unwinding-stack.md) | Next: [28-gradual-rollout-feature-variant-router.md](28-gradual-rollout-feature-variant-router.md)
