# Exercise 33: Encryption Keys Lazily Derived Using KDF with sync.OnceValue, Keyed by Key ID or Purpose

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye prueba de concurrencia con -race).

Deriving an encryption key from a master secret with a key derivation
function is *supposed* to be expensive — that cost is what makes brute-force
attacks on the master secret impractical. But a service that supports
several purposes (`at-rest`, `token-signing`, `backup`) should not pay that
cost for every purpose at startup if half of them are never used in a given
process. This exercise derives each purpose's key lazily, the first time
that key ID is requested, using `sync.OnceValue` per ID so the expensive
derivation runs at most once per purpose — never at import, never twice
even under concurrent first requests for the same purpose.

## What you'll build

```text
keyderive/                 independent module: example.com/keyderive
  go.mod                     module example.com/keyderive
  keyderive.go                 deriveKey (stdlib PBKDF2-HMAC-SHA256), Manager with per-id sync.OnceValue
  cmd/
    demo/
      main.go                  derives two keys, shows caching and cross-id independence
  keyderive_test.go            deriveKey determinism/salt table + per-id caching + concurrent same-id test with -race
```

Files: `keyderive.go`, `cmd/demo/main.go`, `keyderive_test.go`.
Implement: `deriveKey(secret, salt []byte, iterations, keyLen int) []byte` implementing PBKDF2-HMAC-SHA256 using only `crypto/hmac` and `crypto/sha256`; `Manager.Key(id KeyID) []byte` lazily deriving and caching each key ID's key via `sync.OnceValue`, keyed by `KeyID` as the KDF salt; `Manager.Derivations() int64` for tests to observe how many distinct IDs have actually been derived.
Test: `deriveKey` is deterministic for the same inputs and differs when the salt (key ID) differs; `Manager.Key` derives a given ID's key exactly once no matter how many times it is requested, and two different IDs derive independently, both proven via the atomic derivation counter; many goroutines requesting the same ID concurrently still derive it exactly once and all receive identical bytes, verified with `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/33-encryption-key-derivation-lazy-init/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/33-encryption-key-derivation-lazy-init
go mod edit -go=1.24
```

### Why PBKDF2 by hand, and why lazy per key ID

Go's standard library does not ship PBKDF2 or Argon2 — those live in
`golang.org/x/crypto`, outside stdlib. Since this chapter's modules are
stdlib-only, `deriveKey` implements PBKDF2-HMAC-SHA256 directly from `RFC
8018`, using nothing beyond `crypto/hmac` and `crypto/sha256`: for each
output block, it computes `U_1 = HMAC(secret, salt || blockIndex)`, then
iterates `U_i = HMAC(secret, U_{i-1})`, XORing every `U_i` into an
accumulator, `iterations` times. Calling `prf.Reset()` between rounds reuses
the same `hash.Hash` keyed with `secret` without re-keying it — `Reset`
clears only the data written since the last reset, not the HMAC key itself.
The result, `T = U_1 XOR U_2 XOR ... XOR U_iterations`, is exactly PBKDF2's
per-block output; concatenating blocks and truncating to `keyLen` gives the
final derived key. `iterations = 10000` is deliberately expensive — that is
the entire point of a KDF — which is precisely why deriving a key nobody
asked for would be wasteful.

`Manager` treats each `KeyID` as an independent lazy singleton, the same
`sync.OnceValue` pattern used for the shared HTTP client and the per-key
rate limiter earlier in this chapter, applied here to per-purpose key
material instead: the first call to `Key("at-rest")` builds a
`sync.OnceValue`-wrapped closure for that ID and stores it in the map under
the manager's mutex; every later call to `Key("at-rest")` — from any
goroutine — reuses the same cached closure and therefore the same already-
derived bytes, without re-running the expensive KDF. A different ID,
`Key("token-signing")`, gets its own independent `sync.OnceValue` closure
and its own independent 10000-round derivation, using the ID itself as the
KDF salt — different purposes must never share a derived key even though
they share the same master secret.

The atomic `derivations` counter is the test-only observable that turns
"derives at most once per ID" into an assertion, exactly like the build
counters used for the earlier lazy-singleton exercises in this chapter.

Create `keyderive.go`:

```go
// keyderive.go
// Package keyderive derives one encryption key per purpose (a KeyID) from a
// shared master secret using PBKDF2-HMAC-SHA256, deferring each key's
// derivation until the first time that key ID is actually requested via
// sync.OnceValue -- so a purpose that is never used never pays the
// deliberately expensive KDF cost, and concurrent first requests for the
// same key ID never derive it twice.
package keyderive

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"sync/atomic"
)

// KeyID names a purpose a derived key is used for (e.g. "at-rest",
// "token-signing"). Each distinct KeyID gets its own derived key, derived
// from the same master secret with the KeyID as salt.
type KeyID string

const (
	iterations = 10000 // PBKDF2 iteration count
	keyLen     = 32    // derived key length in bytes (AES-256)
)

// deriveKey implements PBKDF2-HMAC-SHA256 (RFC 8018) using only stdlib
// crypto/hmac and crypto/sha256 primitives.
func deriveKey(secret, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha256.New, secret)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	dk := make([]byte, 0, numBlocks*hashLen)
	blockIndex := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		binary.BigEndian.PutUint32(blockIndex, uint32(block))

		prf.Reset()
		prf.Write(salt)
		prf.Write(blockIndex)
		u := prf.Sum(nil)

		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iterations; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// Manager lazily derives and caches one key per KeyID from a shared master
// secret.
type Manager struct {
	mu          sync.Mutex
	secret      []byte
	keys        map[KeyID]func() []byte
	derivations atomic.Int64
}

// NewManager returns a Manager deriving keys from secret.
func NewManager(secret []byte) *Manager {
	return &Manager{secret: secret, keys: make(map[KeyID]func() []byte)}
}

// Key returns the derived key for id, deriving and caching it on first
// request via sync.OnceValue. Concurrent first callers for the same id
// never trigger two derivations.
func (m *Manager) Key(id KeyID) []byte {
	m.mu.Lock()
	get, ok := m.keys[id]
	if !ok {
		get = sync.OnceValue(func() []byte {
			m.derivations.Add(1)
			return deriveKey(m.secret, []byte(id), iterations, keyLen)
		})
		m.keys[id] = get
	}
	m.mu.Unlock()
	return get()
}

// Derivations reports how many distinct key IDs have actually had their
// key derived so far.
func (m *Manager) Derivations() int64 { return m.derivations.Load() }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"encoding/hex"
	"fmt"

	"example.com/keyderive"
)

func main() {
	mgr := keyderive.NewManager([]byte("super-secret-master-key"))

	fmt.Println("derivations before first use:", mgr.Derivations())

	atRest := mgr.Key("at-rest")
	fmt.Println("at-rest key:", hex.EncodeToString(atRest))

	atRestAgain := mgr.Key("at-rest")
	fmt.Println("same key returned on second call:", hex.EncodeToString(atRest) == hex.EncodeToString(atRestAgain))

	tokenSigning := mgr.Key("token-signing")
	fmt.Println("token-signing key differs from at-rest:", hex.EncodeToString(atRest) != hex.EncodeToString(tokenSigning))

	fmt.Println("derivations after two distinct key ids:", mgr.Derivations())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the derived key is deterministic for this fixed master
secret and key ID, so it is identical on every run):

```
derivations before first use: 0
at-rest key: bf915d7670111a09f078bd093aa4d6700c3c5120c119b4b49099142715d4f374
same key returned on second call: true
token-signing key differs from at-rest: true
derivations after two distinct key ids: 2
```

### Tests

Create `keyderive_test.go`:

```go
// keyderive_test.go
package keyderive

import (
	"bytes"
	"sync"
	"testing"
)

func TestDeriveKeyDeterministic(t *testing.T) {
	t.Parallel()

	secret := []byte("secret")
	a := deriveKey(secret, []byte("purpose-a"), 1000, 32)
	b := deriveKey(secret, []byte("purpose-a"), 1000, 32)
	if !bytes.Equal(a, b) {
		t.Fatal("deriveKey is not deterministic for the same secret/salt/params")
	}
	if len(a) != 32 {
		t.Fatalf("len(a) = %d, want 32", len(a))
	}
}

func TestDeriveKeyDiffersBySalt(t *testing.T) {
	t.Parallel()

	secret := []byte("secret")
	a := deriveKey(secret, []byte("purpose-a"), 1000, 32)
	b := deriveKey(secret, []byte("purpose-b"), 1000, 32)
	if bytes.Equal(a, b) {
		t.Fatal("deriveKey produced the same key for two different salts")
	}
}

func TestManagerKeyCachedPerID(t *testing.T) {
	t.Parallel()

	mgr := NewManager([]byte("master"))
	if got := mgr.Derivations(); got != 0 {
		t.Fatalf("Derivations() before use = %d, want 0", got)
	}

	k1 := mgr.Key("purpose-a")
	k2 := mgr.Key("purpose-a")
	if !bytes.Equal(k1, k2) {
		t.Fatal("Manager.Key returned different bytes for the same id on repeat calls")
	}
	if got := mgr.Derivations(); got != 1 {
		t.Fatalf("Derivations() after two calls with the same id = %d, want 1", got)
	}

	k3 := mgr.Key("purpose-b")
	if bytes.Equal(k1, k3) {
		t.Fatal("Manager.Key returned the same bytes for two different ids")
	}
	if got := mgr.Derivations(); got != 2 {
		t.Fatalf("Derivations() after a second distinct id = %d, want 2", got)
	}
}

func TestManagerConcurrentKeySameIDDerivesOnce(t *testing.T) {
	mgr := NewManager([]byte("master"))

	const callers = 50
	results := make([][]byte, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = mgr.Key("shared-purpose")
		}()
	}
	wg.Wait()

	if got := mgr.Derivations(); got != 1 {
		t.Fatalf("Derivations() after %d concurrent calls for one id = %d, want 1", callers, got)
	}
	for i, r := range results {
		if !bytes.Equal(r, results[0]) {
			t.Fatalf("caller %d got a different key than caller 0", i)
		}
	}
}
```

## Review

`deriveKey` is correct when it is deterministic for identical inputs and
produces different output for a different salt — the whole point of using
the `KeyID` as salt is that two purposes sharing one master secret must
never end up with the same derived key. `Manager.Key` is correct when
`Derivations()` counts distinct IDs, not distinct calls: two calls with the
same ID count once, a call with a new ID counts again, and — the test that
would catch a broken `sync.OnceValue` wiring — fifty concurrent goroutines
requesting the *same* ID still leave `Derivations()` at exactly 1, with
every goroutine receiving byte-identical key material.

The mistake to avoid is deriving all configured keys eagerly, in `init()`
or in `NewManager`, "to keep things simple" — that pays the deliberately
expensive KDF cost for every purpose a service *might* use, in every
process that constructs a `Manager`, including short-lived CLI invocations
that only ever touch one key ID. The other mistake is guarding the
per-`KeyID` map with only a lock around the map access but not around the
derivation itself: without `sync.OnceValue` wrapping the closure, two
goroutines racing into `Key` for a brand-new ID could both see "not yet
present" and both run the 10000-iteration derivation, wasting the work
`sync.OnceValue` exists to deduplicate.

## Resources

- [RFC 8018 — PKCS #5: Password-Based Cryptography Specification (PBKDF2)](https://www.rfc-editor.org/rfc/rfc8018#section-5.2) — the algorithm `deriveKey` implements.
- [crypto/hmac](https://pkg.go.dev/crypto/hmac) and [crypto/sha256](https://pkg.go.dev/crypto/sha256) — the stdlib primitives PBKDF2-HMAC-SHA256 is built from.
- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue) — the per-`KeyID` lazy-derivation mechanism `Manager.Key` relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-multi-tenant-routing-table-validator.md](32-multi-tenant-routing-table-validator.md) | Next: [34-distributed-trace-sampler-policy-init.md](34-distributed-trace-sampler-policy-init.md)
