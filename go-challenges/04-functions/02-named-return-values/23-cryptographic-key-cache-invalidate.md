# Exercise 23: Cryptographic Key Rotation Cache Invalidation

Once a cryptographic key rotates, anything derived from the old key — a
verified signature, a decrypted blob, a cached MAC — is stale, and serving it
from cache is a correctness bug with security consequences. This exercise
builds a key cache whose `Rotate` flips a `Valid` flag through a deferred
closure keyed on the named `rotated bool` result, so invalidation happens on
every successful rotation without being duplicated at each call site.

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

## What you'll build

```text
keyrotate/                    independent module: example.com/keyrotate
  go.mod
  keyrotate.go                 KeyCache; Derive; Rotate (named rotated, deferred invalidation)
  cmd/demo/
    main.go                    runnable demo: rotate twice, then reject an empty key
  keyrotate_test.go             valid rotation invalidates, empty key rejected, concurrent safety
```

- Files: `keyrotate.go`, `cmd/demo/main.go`, `keyrotate_test.go`.
- Implement: `(*KeyCache) Rotate(newKeyID string, newKey []byte) (rotated bool, err error)` that installs the new key and, via a deferred closure keyed on `rotated`, invalidates the derived-value cache only when the rotation actually succeeded.
- Test: a table covering a successful rotation (cache invalidated) and an empty-key rejection (cache untouched), plus a concurrent `Rotate`/`Derive` test under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/23-cryptographic-key-cache-invalidate/cmd/demo
cd go-solutions/04-functions/02-named-return-values/23-cryptographic-key-cache-invalidate
go mod edit -go=1.24
```

### Invalidate only when the rotation actually happened

```go
defer func() {
    if rotated {
        c.Valid = false
    }
}()

if len(newKey) == 0 {
    err = errors.New("keyrotate: empty key")
    return
}
c.currentID = newKeyID
c.key = newKey
rotated = true
return
```

`rotated` starts false and is only set to true on the one success path, right
before the final `return`. The deferred closure reads that named result after
the body has run and invalidates the cache exactly when it should — a
rejected rotation (empty key) leaves `rotated` false, so the defer does
nothing, and any values already cached under the still-current key remain
valid. If `Rotate` grows a second success path later — say, a key derived
from a KMS call instead of one passed directly — it inherits the same
invalidation guarantee automatically, because the guarantee is tied to
`rotated`, not to a specific line of code.

Create `keyrotate.go`:

```go
package keyrotate

import (
	"errors"
	"sync"
)

// KeyCache holds the current signing/encryption key plus a cache of values
// derived from it (for example, verified-signature results). Valid tracks
// whether that derived cache still corresponds to the current key.
type KeyCache struct {
	mu        sync.Mutex
	currentID string
	key       []byte
	derived   map[string]string
	Valid     bool
}

// NewKeyCache returns a cache with no key installed yet.
func NewKeyCache() *KeyCache {
	return &KeyCache{derived: make(map[string]string)}
}

// Derive returns a cached derived value for id, computing and caching it if
// absent or if the cache was invalidated by a rotation.
func (c *KeyCache) Derive(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.Valid {
		c.derived = make(map[string]string)
		c.Valid = true
	}
	if v, ok := c.derived[id]; ok {
		return v
	}
	v := id + ":" + c.currentID
	c.derived[id] = v
	return v
}

// Rotate installs newKey under newKeyID. rotated reports whether the
// rotation actually took effect (it is rejected if newKey is empty).
//
// rotated is a named result read by a deferred closure: whenever the
// rotation succeeds, the defer flips Valid to false so every entry derived
// under the old key is treated as stale and recomputed on next access. Using
// a defer keyed on the named result means the invalidation logic lives in
// one place and cannot be forgotten on whichever return path the rotation
// takes — today there is only one success path, but the guarantee holds even
// if a second one is added later.
func (c *KeyCache) Rotate(newKeyID string, newKey []byte) (rotated bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	defer func() {
		if rotated {
			c.Valid = false
		}
	}()

	if len(newKey) == 0 {
		err = errors.New("keyrotate: empty key")
		return
	}
	c.currentID = newKeyID
	c.key = newKey
	rotated = true
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/keyrotate"
)

func main() {
	cache := keyrotate.NewKeyCache()

	ok, err := cache.Rotate("key-v1", []byte("secret-v1"))
	fmt.Printf("rotate to v1: ok=%v err=%v\n", ok, err)

	v := cache.Derive("payload-1")
	fmt.Printf("derived under v1: %s valid=%v\n", v, cache.Valid)

	ok, err = cache.Rotate("key-v2", []byte("secret-v2"))
	fmt.Printf("rotate to v2: ok=%v err=%v valid=%v\n", ok, err, cache.Valid)

	v = cache.Derive("payload-1")
	fmt.Printf("derived under v2: %s valid=%v\n", v, cache.Valid)

	ok, err = cache.Rotate("key-bad", nil)
	fmt.Printf("rotate with empty key: ok=%v err=%v valid=%v\n", ok, err, cache.Valid)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rotate to v1: ok=true err=<nil>
derived under v1: payload-1:key-v1 valid=true
rotate to v2: ok=true err=<nil> valid=false
derived under v2: payload-1:key-v2 valid=true
rotate with empty key: ok=false err=keyrotate: empty key valid=true
```

### Tests

Create `keyrotate_test.go`:

```go
package keyrotate

import (
	"sync"
	"testing"
)

func TestRotate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		keyID     string
		key       []byte
		wantOK    bool
		wantErr   bool
		wantValid bool
	}{
		{name: "valid key rotates and invalidates cache", keyID: "v1", key: []byte("secret"), wantOK: true, wantErr: false, wantValid: false},
		{name: "empty key is rejected", keyID: "v-bad", key: nil, wantOK: false, wantErr: true, wantValid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := NewKeyCache()
			c.Derive("warm") // populates the cache and sets Valid = true
			if !c.Valid {
				t.Fatal("setup: expected Valid = true before rotation")
			}

			ok, err := c.Rotate(tt.keyID, tt.key)
			if ok != tt.wantOK {
				t.Fatalf("rotated = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Valid != tt.wantValid {
				t.Fatalf("Valid = %v, want %v", c.Valid, tt.wantValid)
			}
		})
	}
}

func TestRotateConcurrentSafe(t *testing.T) {
	t.Parallel()

	c := NewKeyCache()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Rotate("key", []byte("secret"))
			c.Derive("payload")
		}(i)
	}
	wg.Wait()

	// No assertion beyond "the race detector found nothing" — this test
	// exists to exercise Rotate and Derive under concurrent access.
	if c.Derive("payload") == "" {
		t.Fatal("Derive returned empty string after concurrent rotations")
	}
}
```

## Review

`Rotate` is correct when a successful rotation always invalidates the derived
cache and a rejected rotation never touches it, under both sequential and
concurrent use. The named result `rotated` is what lets a single deferred
closure be the one and only place invalidation logic lives, instead of a
`c.Valid = false` line duplicated after every successful branch. The mistake
to avoid is invalidating unconditionally in the defer (without checking
`rotated`) — that would wipe a perfectly valid cache every time a rotation
was rejected for an empty key, which is a correctness bug of its own, just in
the opposite direction.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex)
- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [NIST SP 800-57: Recommendation for Key Management (key rotation)](https://csrc.nist.gov/pubs/sp/800/57/pt1/r5/final)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-http-response-trailer-header-injection.md](22-http-response-trailer-header-injection.md) | Next: [24-rate-limiter-token-return-on-abort.md](24-rate-limiter-token-return-on-abort.md)
