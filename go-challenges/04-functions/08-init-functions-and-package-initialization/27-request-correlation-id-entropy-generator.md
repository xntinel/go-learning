# Exercise 27: Lazy Initialization of Cryptographic Entropy Source for Correlation IDs via sync.OnceValue

**Nivel: Intermedio** — validacion rapida (un test corto, mas una prueba de concurrencia).

A request correlation ID generator needs a cryptographic entropy source --
in Go, `crypto/rand.Reader` wrapped behind a small type that could, in a
larger system, warm a pool or check platform capabilities. Building that
source at import time means every binary that imports the package pays for
it, including tests, CLIs, and code generators that never generate a single
ID. This exercise defers that setup to the first `NewID()` call using
`sync.OnceValue`, so the cost is paid at most once, lazily, and never twice
even under concurrent first requests.

## What you'll build

```text
reqid/                     independent module: example.com/reqid
  go.mod                     module example.com/reqid
  reqid.go                     entropySource, sync.OnceValue singleton, NewID, build counter
  cmd/
    demo/
      main.go                  shows zero builds before use, one build after several IDs
  reqid_test.go                 lazy-before-use test + concurrent-unique-ids test with -race
```

Files: `reqid.go`, `cmd/demo/main.go`, `reqid_test.go`.
Implement: `newEntropySource() *entropySource` wrapping `crypto/rand.Reader` and incrementing an atomic build counter; `var source = sync.OnceValue(newEntropySource)`; `NewID() (string, error)` reading `idBytes` of randomness through `source()` and hex-encoding it.
Test: the build counter is 0 before any `NewID()` call; many concurrent `NewID()` calls build the source exactly once and every returned ID is distinct.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reqid/cmd/demo
cd ~/go-exercises/reqid
go mod init example.com/reqid
go mod edit -go=1.24
```

### Why lazy, and why sync.OnceValue specifically

The naive version is a package-level `var source = newEntropySource()` or an
`init()` that builds it — both run unconditionally at import, in every
binary, including ones that only need the package's types. `sync.OnceValue`
fixes this the same way it did for the shared HTTP client earlier in this
chapter: `sync.OnceValue(f)` returns a function that runs `f` at most once,
caches its return value, and is safe under concurrent access — if a hundred
goroutines call `source()` for the first time simultaneously, exactly one
runs `newEntropySource`, and the rest block until it is ready, then all
receive the same cached `*entropySource`. That is precisely the guarantee a
shared entropy wrapper needs: constructing it twice would be wasteful, not
unsafe by itself, but the double-checked-locking dance to avoid it by hand
is easy to get subtly wrong, and `sync.OnceValue` encodes the correct
version once.

The atomic build counter is not something production code would keep; it
exists so the test can turn "the factory runs at most once" from a claim
into an assertion. `TestLazyBeforeUse` proves the source is not built merely
by importing the package — a build count of 0 before any call would already
be 1 with an `init()`-built global. `TestConcurrentNewIDBuildsSourceOnce`
proves the concurrent guarantee itself: a hundred goroutines call `NewID()`
at once, and the counter must land on exactly 1 no matter how the scheduler
interleaves them.

Create `reqid.go`:

```go
// reqid.go
// Package reqid generates request correlation IDs backed by a cryptographic
// entropy source that is built lazily, on first use, via sync.OnceValue --
// so a binary that never generates an ID never pays the (small but real)
// cost of setting up the entropy source, and concurrent first callers never
// race to construct it twice.
package reqid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// builds counts how many times the entropy source factory ran. Production
// code would not keep this; tests use it to prove the factory runs exactly
// once no matter how many goroutines race into the first call.
var builds atomic.Int64

// Builds reports how many times the entropy source has been constructed.
func Builds() int64 { return builds.Load() }

// entropySource wraps the reader NewID draws bytes from.
type entropySource struct {
	r io.Reader
}

func newEntropySource() *entropySource {
	builds.Add(1)
	return &entropySource{r: rand.Reader}
}

// source is the lazily built singleton. sync.OnceValue guarantees
// newEntropySource runs at most once, on the first call to source(), even
// under concurrent callers.
var source = sync.OnceValue(newEntropySource)

// idBytes is the number of random bytes per correlation ID (128 bits).
const idBytes = 16

// NewID returns a new correlation ID: idBytes of cryptographic randomness,
// hex-encoded. The entropy source is built on first call.
func NewID() (string, error) {
	buf := make([]byte, idBytes)
	if _, err := io.ReadFull(source().r, buf); err != nil {
		return "", fmt.Errorf("reqid: reading entropy: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/reqid"
)

func main() {
	fmt.Println("entropy source builds before first use:", reqid.Builds())

	for i := 0; i < 3; i++ {
		id, err := reqid.NewID()
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Printf("request %d correlation id: %s\n", i+1, id)
	}

	fmt.Println("entropy source builds after 3 ids:", reqid.Builds())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the three hex IDs are genuinely random and differ on every
run; only the shape — 32 hex characters, build counts of 0 then 1 — is
fixed):

```
entropy source builds before first use: 0
request 1 correlation id: 83b71345a80bcab24e9e0d3a0e36272f
request 2 correlation id: 75ea6786b2a898404d35bcae3c5b8a72
request 3 correlation id: 7709b227b1d4eb361b46234c08b9257e
entropy source builds after 3 ids: 1
```

### Tests

Create `reqid_test.go`:

```go
// reqid_test.go
package reqid

import (
	"sync"
	"testing"
)

// TestLazyBeforeUse asserts the entropy source is not built merely by
// importing the package. It runs first (source order, no t.Parallel) so no
// other test has called NewID yet.
func TestLazyBeforeUse(t *testing.T) {
	if got := Builds(); got != 0 {
		t.Fatalf("Builds() = %d before any NewID() call; want 0 (was it built at import?)", got)
	}
}

// TestConcurrentNewIDBuildsSourceOnce drives many concurrent callers and
// proves the entropy source factory ran exactly once and every ID is
// distinct.
func TestConcurrentNewIDBuildsSourceOnce(t *testing.T) {
	const n = 100
	ids := make([]string, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = NewID()
		}()
	}
	wg.Wait()

	seen := make(map[string]struct{}, n)
	for i, id := range ids {
		if errs[i] != nil {
			t.Fatalf("NewID() error at %d: %v", i, errs[i])
		}
		if len(id) != idBytes*2 {
			t.Fatalf("NewID() = %q, want %d hex chars", id, idBytes*2)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated: %s", id)
		}
		seen[id] = struct{}{}
	}

	if got := Builds(); got != 1 {
		t.Fatalf("Builds() = %d after %d concurrent calls; want 1", got, n)
	}
}
```

## Review

The design is correct when `Builds()` is 0 until the first `NewID()` call,
exactly 1 no matter how many goroutines race into that first call, and
every generated ID is distinct — `TestConcurrentNewIDBuildsSourceOnce`
checks all three at once by driving a hundred concurrent callers and
collecting both the build count and the full set of IDs. Run with `-race`
to confirm there is no data race around the shared `source` value; the
guarantee comes from `sync.OnceValue`'s internal locking, not from anything
this package does itself.

The mistake to avoid is building the entropy source eagerly — a package
`var source = newEntropySource()` or an `init()` — which pays the cost in
every importing binary regardless of whether it ever generates an ID, and
which a test cannot observe as "not yet built." The other mistake is
hand-rolling the lazy-singleton check (a `bool` flag plus a mutex plus a
value) instead of reaching for `sync.OnceValue`, which already gets the
memory visibility and the "exactly once under concurrency" guarantee right.

## Resources

- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue) — run a factory at most once and cache its value (Go 1.21+), the mechanism behind `source`.
- [crypto/rand](https://pkg.go.dev/crypto/rand) — the cryptographically secure entropy source `NewID` reads from.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-graphql-schema-definition-validator.md](26-graphql-schema-definition-validator.md) | Next: [28-api-base-url-parser-and-validator.md](28-api-base-url-parser-and-validator.md)
