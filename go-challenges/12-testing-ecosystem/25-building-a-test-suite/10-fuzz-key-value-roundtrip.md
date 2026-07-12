# Exercise 10: Fuzz The Key/Value Round-Trip For Encoding Bugs

A fuzz target asserts an *invariant*, not a fixed output. For a cache the natural
invariant is the Set-then-Get round-trip: whatever bytes you store come back
byte-for-byte, and `Len`/`Delete` stay consistent for any key. This module fuzzes
that property, which surfaces copy-on-store and key-handling bugs a hand-picked
example would miss, and builds a regression corpus under `testdata/fuzz`.

## What you'll build

```text
fuzzcache/                  independent module: example.com/fuzzcache
  go.mod
  cache.go                  Cache with defensive copy-on-store and copy-on-read
  cmd/
    demo/
      main.go               demo showing caller mutation cannot corrupt the store
  cache_test.go             FuzzRoundTrip + a copy-on-store unit test
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: `Set` copies the value before storing; `Get` returns a copy; plus `Len`/`Delete`.
Test: `FuzzRoundTrip` seeding edge cases with `f.Add`, then asserting round-trip equality and store/Delete consistency over `(string, []byte)`.
Verify: `go test -count=1 -race ./...` and `go test -fuzz=FuzzRoundTrip -fuzztime=30s`

### Asserting an invariant, and the copy bug fuzzing catches

The mental shift fuzzing demands is that the target checks a *property* that must
hold for every input, not a value you computed by hand. `f.Add` seeds the corpus
with representative edge cases — an empty key, a unicode key, an empty value, a
large value, a value with NUL bytes — and `f.Fuzz(func(t *testing.T, key string,
value []byte){ ... })` explores mutations of those seeds. The function under test
never knows the concrete input, so it cannot assert `got == "expected"`; it asserts
that storing then reading returns the same bytes, that `Len` is `1` afterward, and
that `Delete` returns the cache to empty. Those invariants hold for any key and any
value, which is exactly what makes them fuzzable.

The specific defect this surfaces is aliasing. If `Set` stores the caller's slice
directly rather than copying it, the cache and the caller share a backing array,
and a later mutation by the caller silently corrupts the stored value — a bug that
never appears with string-literal test values but detonates under a fuzzer that
reuses and mutates byte slices, or in production when a caller reuses a buffer.
This cache defends both ends: `Set` clones the value before storing, and `Get`
clones before returning, so neither side can reach through the API to mutate the
other's memory. The fuzz target proves it by capturing the original bytes, storing,
mutating the caller's slice, and asserting the stored value is unchanged. Remove
the clone in `Set` and the fuzzer (or even the seed corpus) fails immediately.

Under a plain `go test`, the fuzz target runs its seed corpus as ordinary
sub-tests, so it guards against regressions even when nobody is actively fuzzing.
`go test -fuzz=FuzzRoundTrip -fuzztime=30s` runs the mutation engine; any input
that breaks an invariant is written to `testdata/fuzz/FuzzRoundTrip/` and becomes a
permanent regression seed replayed on every future `go test`.

Create `cache.go`:

```go
package fuzzcache

import (
	"bytes"
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("cache: key not found")
	ErrExpired  = errors.New("cache: key expired")
)

type entry struct {
	value     []byte
	expiresAt time.Time
}

type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time
}

func New() *Cache {
	return &Cache{data: make(map[string]entry), now: time.Now}
}

// Set stores a defensive copy of value, so a later mutation of the caller's
// slice cannot corrupt the stored bytes.
func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}
	c.data[key] = entry{value: bytes.Clone(value), expiresAt: expiresAt}
}

// Get returns a copy of the stored value, so the caller cannot mutate the
// cache's memory through the returned slice.
func (c *Cache) Get(key string) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	if !e.expiresAt.IsZero() && c.now().After(e.expiresAt) {
		return nil, ErrExpired
	}
	return bytes.Clone(e.value), nil
}

func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}
```

### The runnable demo

The demo stores a value, mutates the caller's original slice, and shows the stored
value is untouched — the copy-on-store guarantee the fuzz target generalizes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fuzzcache"
)

func main() {
	c := fuzzcache.New()
	buf := []byte("alice")
	c.Set("user", buf, 0)

	buf[0] = 'A' // caller mutates its own slice after Set

	got, _ := c.Get("user")
	fmt.Printf("caller buf = %s\n", buf)
	fmt.Printf("stored     = %s\n", got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
caller buf = Alice
stored     = alice
```

### The fuzz target

Note the value type `bytes.Clone` returns for an empty input: `bytes.Clone(nil)`
and `bytes.Clone([]byte{})` both return `nil`, and reading back an empty value
yields `nil` too, so the round-trip is compared with `bytes.Equal`, which treats
`nil` and an empty slice as equal.

Create `cache_test.go`:

```go
package fuzzcache

import (
	"bytes"
	"errors"
	"testing"
)

func TestCopyOnStore(t *testing.T) {
	t.Parallel()
	c := New()
	buf := []byte("value")
	c.Set("k", buf, 0)
	buf[0] = 'X'

	got, err := c.Get("k")
	if err != nil {
		t.Fatalf("Get(k) err = %v", err)
	}
	if !bytes.Equal(got, []byte("value")) {
		t.Fatalf("stored value = %q; caller mutation leaked in", got)
	}
}

func FuzzRoundTrip(f *testing.F) {
	f.Add("", []byte(nil))
	f.Add("key", []byte("value"))
	f.Add("empty-value", []byte(""))
	f.Add("unicode-é-世界", []byte{0x00, 0x01, 0x02, 0xff})
	f.Add("large", bytes.Repeat([]byte("x"), 8192))

	f.Fuzz(func(t *testing.T, key string, value []byte) {
		want := bytes.Clone(value)
		c := New()

		c.Set(key, value, 0)
		got, err := c.Get(key)
		if err != nil {
			t.Fatalf("Get(%q) err = %v; want nil", key, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("round-trip mismatch: got %q; want %q", got, want)
		}
		if n := c.Len(); n != 1 {
			t.Fatalf("Len after one Set = %d; want 1", n)
		}

		// copy-on-store: mutating the caller's slice must not change the store.
		if len(value) > 0 {
			value[0] ^= 0xff
			got2, _ := c.Get(key)
			if !bytes.Equal(got2, want) {
				t.Fatalf("caller mutation leaked into stored value: %q; want %q", got2, want)
			}
		}

		c.Delete(key)
		if n := c.Len(); n != 0 {
			t.Fatalf("Len after Delete = %d; want 0", n)
		}
		if _, err := c.Get(key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get after Delete err = %v; want ErrNotFound", err)
		}
	})
}
```

## Review

The fuzz target is correct because it asserts an invariant that holds for every
input — round-trip equality, `Len == 1` after one `Set`, and an empty cache after
`Delete` — rather than a value hand-computed for one case. That is what lets it
generalize over random keys and byte slices. The invariant catches the aliasing bug
directly: without `bytes.Clone` in `Set`, mutating the caller's slice corrupts the
stored bytes and the target fails on the very first seed. Comparing with
`bytes.Equal` handles the `nil`-versus-empty-slice edge that `bytes.Clone` produces
for empty inputs. Run the seed corpus under `go test` as a regression guard, run
`go test -fuzz=FuzzRoundTrip -fuzztime=30s` to actively explore, and commit any
failure the engine finds under `testdata/fuzz` so it is replayed forever.

## Resources

- [Go Fuzzing](https://go.dev/doc/security/fuzz/) — `f.Add`, `f.Fuzz`, the seed corpus, and `testdata/fuzz`.
- [`(*testing.F).Fuzz`](https://pkg.go.dev/testing#F.Fuzz) — the fuzz target signature and supported argument types.
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — the defensive copy behind copy-on-store, including its `nil` behavior.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-benchmark-suite-b-loop.md](09-benchmark-suite-b-loop.md) | Next: [../../13-goroutines-and-channels/01-your-first-goroutine/00-concepts.md](../../13-goroutines-and-channels/01-your-first-goroutine/00-concepts.md)
