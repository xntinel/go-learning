# Exercise 3: Order-Sensitive, Namespace-Isolated Cache-Key Hash

A raw `users:alice:42` key is fine until a segment contains a colon or grows to
kilobytes; then you want a fixed-width digest. This is the lesson's core
correctness artifact: `Builder.Hash(parts ...string)` returns the hex SHA-256 of
the joined key. A cache key must be deterministic (same inputs, same key),
order-sensitive (a different segment order is a different request), and
tenant-isolated (one namespace never collides with another).

This module is self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
keyhash/                   independent module: example.com/keyhash
  go.mod                   go 1.25
  cachekey.go              Builder with Hash(parts ...string) string
  cmd/
    demo/
      main.go              runnable demo: hash a few keys
  cachekey_test.go         determinism, length, order-sensitivity, namespace isolation
```

- Files: `cachekey.go`, `cmd/demo/main.go`, `cachekey_test.go`.
- Implement: `Hash(parts ...string) string` returning `hex.EncodeToString(sha256.Sum256(join(parts)))`.
- Test: two identical calls are equal and 64 hex chars long; `Hash("alice","42") != Hash("42","alice")`; `New("users").Hash("42") != New("orders").Hash("42")`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/03-deterministic-namespaced-hash/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/03-deterministic-namespaced-hash
go mod edit -go=1.25
```

### Why hash a cache key, and the three properties the test pins

Hashing the joined key gives you three operational wins: a fixed 64-character
width regardless of segment length, a safe alphabet (hex, no separators that could
collide with `:`), and no leakage of the raw identifiers into cache-server logs.
The correctness bar is higher than for the plain string, because a cache is only
correct if the key function is a *deterministic, injective-enough* map from
request shape to key. Three properties encode that, and each has a test:

- **Determinism.** `sha256.Sum256` is a pure function, and `join` is pure, so two
  identical calls must produce the identical digest. If they ever differ, the
  cache never hits — every request looks new. The test asserts equality across two
  calls and that the result is exactly 64 hex characters (SHA-256 is 32 bytes,
  hex-encoded to 64).
- **Order sensitivity.** `Hash("alice","42")` must differ from `Hash("42","alice")`.
  Because `join` emits `users:alice:42` versus `users:42:alice`, the pre-image
  differs and so does the digest. For a cache this is the right default: a
  different argument order is a different request.
- **Namespace isolation.** `New("users").Hash("42")` must differ from
  `New("orders").Hash("42")`. The namespace is part of the pre-image, so the two
  tenants get disjoint keys and one tenant's cached value can never be served to
  another. This is a security property, not just a correctness one.

`Sum256` returns a `[32]byte` array; `sum[:]` slices it so `hex.EncodeToString`
can consume it. Note the array-to-slice conversion is the small idiom that trips
people up: `hex.EncodeToString(sum)` does not compile — it needs the `[:]`.

Create `cachekey.go`:

```go
// cachekey.go
package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Builder produces namespace-prefixed cache keys and their hashes.
type Builder struct {
	namespace string
}

// New returns a Builder that prefixes every key with namespace.
func New(namespace string) *Builder {
	return &Builder{namespace: namespace}
}

// Hash returns the hex-encoded SHA-256 of the namespaced, colon-joined key. It is
// deterministic, order-sensitive, and namespace-isolated.
func (b *Builder) Hash(parts ...string) string {
	sum := sha256.Sum256([]byte(b.join(parts)))
	return hex.EncodeToString(sum[:])
}

func (b *Builder) join(parts []string) string {
	all := make([]string, 0, len(parts)+1)
	all = append(all, b.namespace)
	all = append(all, parts...)
	return strings.Join(all, ":")
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/keyhash"
)

func main() {
	users := cachekey.New("users")
	orders := cachekey.New("orders")

	fmt.Println(users.Hash("alice", "42"))
	fmt.Println(users.Hash("42", "alice"))
	fmt.Println(orders.Hash("42"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (SHA-256 is fixed, so these digests are reproducible):

```
6a300fa0e206c50f05b71455f307208740d2e02e7a4de8783a75d806e36f4b04
6ab6ff5500b7d373e6a7e90224e8815500c6b26c85321951d62fc0d41851c6fe
8a5f217ddb0c9f03d16c0db38f2ba1c3afba56ca9be31a6bbb468b7d2e03746c
```

What the demo demonstrates is structural: the first two lines differ (order
sensitivity, from hashing `users:alice:42` versus `users:42:alice`) and all three
are exactly 64 hex characters.

### Tests

The tests assert the three properties directly rather than hard-coding a digest,
which keeps them robust and readable. `TestHashLength` pins the 64-hex width;
`TestHashOrderSensitive` and `TestHashNamespaceIsolated` prove no cross-request or
cross-tenant collision.

Create `cachekey_test.go`:

```go
// cachekey_test.go
package cachekey

import (
	"encoding/hex"
	"fmt"
	"testing"
)

func TestHashDeterministic(t *testing.T) {
	t.Parallel()

	b := New("users")
	h1 := b.Hash("alice", "42")
	h2 := b.Hash("alice", "42")
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %q vs %q", h1, h2)
	}
}

func TestHashLength(t *testing.T) {
	t.Parallel()

	b := New("users")
	h := b.Hash("alice", "42")
	if len(h) != 64 {
		t.Fatalf("hash length = %d, want 64", len(h))
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Fatalf("hash is not valid hex: %v", err)
	}
}

func TestHashOrderSensitive(t *testing.T) {
	t.Parallel()

	b := New("users")
	if b.Hash("alice", "42") == b.Hash("42", "alice") {
		t.Fatal("hash must differ when segment order differs")
	}
}

func TestHashNamespaceIsolated(t *testing.T) {
	t.Parallel()

	users := New("users")
	orders := New("orders")
	if users.Hash("42") == orders.Hash("42") {
		t.Fatal("hash must differ across namespaces (tenant isolation)")
	}
}

func Example() {
	b := New("users")
	fmt.Println(len(b.Hash("alice", "42")))
	// Output: 64
}
```

## Review

The hash is correct when it is a deterministic, order-sensitive, namespace-scoped
function of the segments — which the four tests pin without hard-coding a digest.
The array-to-slice `sum[:]` is the only easy-to-miss detail; forget it and the
code will not compile. The reason to test *properties* (equal-when-same,
differ-when-reordered, differ-across-tenant) rather than a literal digest is
robustness: the properties are what the cache depends on, and they stay true even
if you later swap the separator or the hash. Run `go test -race` to confirm.

## Resources

- [`crypto/sha256`: `Sum256`](https://pkg.go.dev/crypto/sha256#Sum256)
- [`encoding/hex`: `EncodeToString`](https://pkg.go.dev/encoding/hex#EncodeToString)
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-printf-style-arg-forwarding.md](02-printf-style-arg-forwarding.md) | Next: [04-typed-int-segment-key.md](04-typed-int-segment-key.md)
