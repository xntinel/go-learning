# Exercise 7: Versioned config store with atomic compare-and-swap updates

Configuration and feature-flag stores need lost-update safety: two controllers
pushing updates concurrently must not have one silently overwrite the other's newer
value. The fix is optimistic concurrency — a writer supplies the version it read
and the update only lands if that version is still current. `sync.Map`'s
`CompareAndSwap` and `CompareAndDelete` give you exactly this per key, with no
external mutex. This module builds the versioned store and proves the CAS
semantics under racing writers.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
configstore/                  independent module: example.com/configstore
  go.mod                      go 1.26
  store.go                    type Config, type Store; Set, Get, Update, Remove
  cmd/
    demo/
      main.go                 runnable demo: CAS success, stale CAS fails, conditional delete
  store_test.go               stale-CAS-fails, racing-updaters-one-wins-per-gen, Example
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Store` with `Set`, `Get`, `Update(key, old, new Config) bool` (CompareAndSwap), `Remove(key, expected Config) bool` (CompareAndDelete).
- Test: a stale `Update` fails and leaves the map unchanged; racing updaters have exactly one winner per generation with no lost updates; `Remove` only deletes on a match.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir go-solutions/15-sync-primitives/04-sync-map/07-versioned-config-cas && cd go-solutions/15-sync-primitives/04-sync-map/07-versioned-config-cas
```

### Why the value type must be comparable

`CompareAndSwap(key, old, new)` swaps only if the current value equals `old` by
interface equality, and `CompareAndDelete(key, old)` deletes only if the current
value equals `old`. That equality check is why the stored type **must be
comparable**: a slice, map, or func value panics at runtime when compared. So the
`Config` value here is a struct of comparable fields (`Version int`, `Value
string`) — the whole struct participates in the CAS comparison. If your config
needs a non-comparable field (a slice of allowed IPs, say), you store a comparable
*pointer* to an immutable config and CAS on the pointer identity, never on the
struct contents.

`Update` returns `false` when the caller's `old` no longer matches — a stale writer
that read version 3, took time to compute, and tried to swap while another writer
already advanced to version 4. Instead of clobbering version 4, the stale `Update`
fails cleanly and the caller re-reads and retries. That retry loop is the
optimistic-concurrency pattern: read the current value, compute the next, CAS from
current to next, and loop if the CAS lost. Under contention exactly one writer wins
each round and the losers retry against the new value, so no update is ever lost and
no lock is ever held.

`Remove` is the conditional-delete counterpart: it removes the key only if it still
holds the exact value the caller expected, so a delete race cannot drop a value that
was updated after the caller decided to remove it.

Create `store.go`:

```go
package configstore

import "sync"

// Config is a versioned configuration value. It is a comparable struct so it can
// participate in sync.Map's CompareAndSwap / CompareAndDelete equality checks.
type Config struct {
	Version int
	Value   string
}

// Store holds one versioned Config per key with lost-update-safe updates.
type Store struct {
	configs sync.Map // map[string]Config
}

// NewStore returns an empty store ready for concurrent use.
func NewStore() *Store {
	return &Store{}
}

// Set unconditionally stores cfg under key. Use it to seed a key; use Update for
// lost-update-safe changes.
func (s *Store) Set(key string, cfg Config) {
	s.configs.Store(key, cfg)
}

// Get returns the current Config for key and whether it was present.
func (s *Store) Get(key string) (Config, bool) {
	v, ok := s.configs.Load(key)
	if !ok {
		return Config{}, false
	}
	return v.(Config), true
}

// Update atomically replaces old with new for key, but only if the current value
// still equals old. It returns false (a no-op) if a concurrent writer already
// changed the value, so a stale writer cannot clobber a newer one.
func (s *Store) Update(key string, old, new Config) bool {
	return s.configs.CompareAndSwap(key, old, new)
}

// Remove deletes key only if it still holds expected. It returns false if the
// value changed or the key is absent.
func (s *Store) Remove(key string, expected Config) bool {
	return s.configs.CompareAndDelete(key, expected)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configstore"
)

func main() {
	s := configstore.NewStore()
	v0 := configstore.Config{Version: 0, Value: "debug=false"}
	s.Set("app", v0)

	v1 := configstore.Config{Version: 1, Value: "debug=true"}
	fmt.Println("update v0->v1:", s.Update("app", v0, v1))

	// A stale writer still holding v0 fails: the current value is now v1.
	stale := configstore.Config{Version: 99, Value: "hacked"}
	fmt.Println("stale update v0->x:", s.Update("app", v0, stale))

	cur, _ := s.Get("app")
	fmt.Printf("current: v%d %q\n", cur.Version, cur.Value)

	// Conditional delete: only removes if the value still matches.
	fmt.Println("remove wrong expected:", s.Remove("app", v0))
	fmt.Println("remove right expected:", s.Remove("app", v1))
	_, ok := s.Get("app")
	fmt.Println("present after remove:", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
update v0->v1: true
stale update v0->x: false
current: v1 "debug=true"
remove wrong expected: false
remove right expected: true
present after remove: false
```

### Tests

`TestStaleUpdateFails` seeds a value, applies one CAS, then tries a stale CAS with
the original `old` and asserts it fails and leaves the current value untouched.
`TestRacingUpdatersNoLostUpdate` is the hard contract: `goroutines` writers each
perform one successful increment via a CAS-retry loop against a shared key; after
`wg.Wait` the version must equal the number of writers exactly, proving every
update landed and none was lost. `TestRemoveOnlyOnMatch` pins the conditional
delete.

Create `store_test.go`:

```go
package configstore

import (
	"fmt"
	"sync"
	"testing"
)

func TestStaleUpdateFails(t *testing.T) {
	t.Parallel()

	s := NewStore()
	v0 := Config{Version: 0, Value: "a"}
	v1 := Config{Version: 1, Value: "b"}
	s.Set("k", v0)

	if !s.Update("k", v0, v1) {
		t.Fatal("first Update(v0->v1) = false, want true")
	}
	// Stale writer still holds v0; the current value is v1, so this must fail.
	if s.Update("k", v0, Config{Version: 2, Value: "c"}) {
		t.Fatal("stale Update(v0->v2) = true, want false")
	}
	if got, _ := s.Get("k"); got != v1 {
		t.Fatalf("value = %+v after failed stale update, want %+v", got, v1)
	}
}

func TestRacingUpdatersNoLostUpdate(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Set("k", Config{Version: 0, Value: "v0"})

	const goroutines = 64
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Optimistic-concurrency retry loop: read, compute next, CAS, retry
			// if we lost the race.
			for {
				cur, ok := s.Get("k")
				if !ok {
					t.Errorf("key vanished during update")
					return
				}
				next := Config{Version: cur.Version + 1, Value: fmt.Sprintf("w%d", i)}
				if s.Update("k", cur, next) {
					return
				}
			}
		}()
	}
	wg.Wait()

	got, _ := s.Get("k")
	if got.Version != goroutines {
		t.Fatalf("final version = %d, want %d (a lost update)", got.Version, goroutines)
	}
}

func TestRemoveOnlyOnMatch(t *testing.T) {
	t.Parallel()

	s := NewStore()
	v := Config{Version: 5, Value: "x"}
	s.Set("k", v)

	if s.Remove("k", Config{Version: 4, Value: "x"}) {
		t.Fatal("Remove with wrong expected = true, want false")
	}
	if _, ok := s.Get("k"); !ok {
		t.Fatal("key removed despite non-matching expected")
	}
	if !s.Remove("k", v) {
		t.Fatal("Remove with matching expected = false, want true")
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("key present after matching Remove")
	}
}

func ExampleStore() {
	s := NewStore()
	v0 := Config{Version: 0, Value: "off"}
	s.Set("flag", v0)
	v1 := Config{Version: 1, Value: "on"}
	fmt.Println(s.Update("flag", v0, v1))
	fmt.Println(s.Update("flag", v0, v1)) // stale: fails
	cur, _ := s.Get("flag")
	fmt.Println(cur.Version, cur.Value)
	// Output:
	// true
	// false
	// 1 on
}
```

## Review

The store is correct when a stale `Update` fails and leaves the current value
untouched, and when racing writers converge on the exact number of successful
updates with none lost. The mechanism is `CompareAndSwap`: by swapping only if the
current value still equals the caller's `old`, it turns "read-modify-write" into a
lost-update-safe operation, and the retry loop in the racing test is the standard
optimistic-concurrency pattern built on it. `TestRacingUpdatersNoLostUpdate`
asserting the final version equals the writer count is the proof; a lower number
would mean a CAS wrongly overwrote a newer value. The traps to avoid are storing a
non-comparable value (CAS panics on slice/map/func — wrap in a pointer if you need
one) and forgetting that `Update` returning `false` is not an error but a signal to
re-read and retry. Run `go test -race` to confirm the concurrent CAS path is clean.

## Resources

- [sync.Map.CompareAndSwap](https://pkg.go.dev/sync#Map.CompareAndSwap) — atomic CAS, added in Go 1.20; value must be comparable.
- [sync.Map.CompareAndDelete](https://pkg.go.dev/sync#Map.CompareAndDelete) — conditional delete on a matching value.
- [The Go Memory Model](https://go.dev/ref/mem) — `CompareAndSwap` is a write only when it swaps.

---

Back to [06-session-store-expiry.md](06-session-store-expiry.md) | Next: [08-credential-hot-swap.md](08-credential-hot-swap.md)
