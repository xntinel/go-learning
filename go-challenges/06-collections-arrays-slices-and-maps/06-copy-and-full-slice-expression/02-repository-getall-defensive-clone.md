# Exercise 2: Repository GetAll Returns a Defensive Copy

An in-memory feature-flag store is the canonical source of truth for whether
features are on. If its `GetAll` hands callers the internal slice, any caller that
sorts, filters, or appends the result silently rewrites the store's state. This
exercise builds the store so `GetAll` returns `slices.Clone`, and pins the
boundary with a test that mutates the returned slice and proves the store is
untouched — plus a negative test that documents exactly how the aliasing bug
corrupts canonical state.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
flagstore/                 independent module: example.com/flagstore
  go.mod                   go 1.26
  store.go                 type Store; New, Set, GetAll (clone), getAllAliased (buggy)
  cmd/
    demo/
      main.go              read, sort the copy, read again unchanged
  store_test.go            defensive-copy test, aliasing-bug negative test, concurrent readers
```

Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: a `Store` of `Record` guarded by `sync.RWMutex`; `Set` upserts keeping records sorted by key; `GetAll` returns `slices.Clone(s.records)`.
Test: mutate/sort/append the `GetAll` result, then read again and assert unchanged order; a negative sub-test using the raw internal slice shows corruption; concurrent readers under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/flagstore/cmd/demo
cd ~/go-exercises/flagstore
go mod init example.com/flagstore
```

### Why GetAll must clone

The store keeps `records` sorted by key so lookups and listings are stable. When
`GetAll` returns `s.records` directly, the caller receives a slice header pointing
at the store's live backing array. A caller that does the most natural thing in
the world — `slices.SortFunc(list, byEnabledFirst)` to show enabled flags at the
top, or `append(list, extra)` to add a computed row — reorders or overwrites the
store's own array. The next reader sees a corrupted ordering, or a reader running
concurrently sees a half-sorted array. Nothing in the type system stops this; the
only defense is to decide, at the `GetAll` boundary, that the store owns its array
and the caller gets an independent copy. `slices.Clone(s.records)` is that
decision made explicit.

The `RWMutex` and the clone solve two different problems and you need both. The
lock makes concurrent `Set`/`GetAll` free of data races on the slice header and
length. The clone makes the *returned data* independent so a reader mutating it
after `GetAll` returns — outside the lock — cannot reach back into store state.
Locking without cloning still lets a caller corrupt the array the instant the lock
is released; cloning without locking still races on the header during `Set`.

Create `store.go`:

```go
package flagstore

import (
	"slices"
	"sync"
)

// Record is one feature flag.
type Record struct {
	Key     string
	Enabled bool
}

// Store is a concurrency-safe feature-flag repository. It keeps records sorted
// by Key and owns its backing array: GetAll returns an independent copy.
type Store struct {
	mu      sync.RWMutex
	records []Record
}

// New returns an empty store.
func New() *Store { return &Store{} }

// Set inserts or updates the flag for key, keeping records sorted by Key.
func (s *Store) Set(key string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, found := slices.BinarySearchFunc(s.records, key, func(r Record, k string) int {
		return cmpString(r.Key, k)
	})
	if found {
		s.records[i].Enabled = enabled
		return
	}
	s.records = slices.Insert(s.records, i, Record{Key: key, Enabled: enabled})
}

// GetAll returns an independent snapshot of every record, sorted by Key. The
// caller may sort, filter, append, or truncate it without affecting the store.
func (s *Store) GetAll() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.records)
}

// getAllAliased is the buggy variant used only to demonstrate the aliasing
// failure mode in tests: it hands out the live internal slice.
func (s *Store) getAllAliased() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.records
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
```

### The runnable demo

The demo reads all flags, sorts the returned copy so enabled ones come first, and
then reads again to show the store's own key-sorted order is intact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/flagstore"
)

func main() {
	s := flagstore.New()
	s.Set("beta-search", true)
	s.Set("audit-log", false)
	s.Set("dark-mode", true)

	list := s.GetAll()
	// Caller-side sort: enabled first, then by key. Mutates only the copy.
	slices.SortFunc(list, func(a, b flagstore.Record) int {
		if a.Enabled != b.Enabled {
			if a.Enabled {
				return -1
			}
			return 1
		}
		return 0
	})
	fmt.Println("caller view (enabled first):")
	for _, r := range list {
		fmt.Printf("  %s=%v\n", r.Key, r.Enabled)
	}

	fmt.Println("store view (still key-sorted):")
	for _, r := range s.GetAll() {
		fmt.Printf("  %s=%v\n", r.Key, r.Enabled)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
caller view (enabled first):
  beta-search=true
  dark-mode=true
  audit-log=false
store view (still key-sorted):
  audit-log=false
  beta-search=true
  dark-mode=true
```

### Tests

`TestGetAllIsDefensiveCopy` sorts, appends to, and truncates the `GetAll` result,
then re-reads and asserts the store is byte-for-byte unchanged and still key
sorted. `TestAliasingBugCorruptsStore` calls the buggy `getAllAliased`, sorts it
in place, and asserts the store's canonical order was corrupted — making the
failure mode explicit and pinning why `GetAll` must clone.
`TestConcurrentReadersAndWriter` runs readers and a writer together under `-race`.

Create `store_test.go`:

```go
package flagstore

import (
	"slices"
	"sync"
	"testing"
)

func seeded() *Store {
	s := New()
	s.Set("audit-log", false)
	s.Set("beta-search", true)
	s.Set("dark-mode", true)
	return s
}

func keys(rs []Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Key
	}
	return out
}

func TestGetAllIsDefensiveCopy(t *testing.T) {
	t.Parallel()

	s := seeded()
	list := s.GetAll()

	// Abuse the returned slice every way a caller might.
	slices.Reverse(list)
	list = append(list, Record{Key: "injected", Enabled: true})
	list[0].Enabled = !list[0].Enabled
	list = list[:1]

	got := keys(s.GetAll())
	want := []string{"audit-log", "beta-search", "dark-mode"}
	if !slices.Equal(got, want) {
		t.Fatalf("store corrupted by caller mutation: got %v, want %v", got, want)
	}
	if v, _ := flagOf(s, "audit-log"); v != false {
		t.Fatalf("audit-log flipped to %v, want false", v)
	}
}

func TestAliasingBugCorruptsStore(t *testing.T) {
	t.Parallel()

	s := seeded()
	aliased := s.getAllAliased()

	// Sorting the aliased slice in place reorders the store's own array.
	slices.Reverse(aliased)

	got := keys(s.getAllAliased())
	want := []string{"dark-mode", "beta-search", "audit-log"}
	if !slices.Equal(got, want) {
		t.Fatalf("expected aliasing to corrupt store order to %v, got %v", want, got)
	}
}

func TestConcurrentReadersAndWriter(t *testing.T) {
	t.Parallel()

	s := seeded()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			list := s.GetAll()
			if len(list) > 0 {
				list[0].Enabled = !list[0].Enabled // mutate the copy only
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 50 {
			s.Set("k"+string(rune('a'+i%26)), i%2 == 0)
		}
	}()
	wg.Wait()
}

func flagOf(s *Store, key string) (bool, bool) {
	for _, r := range s.GetAll() {
		if r.Key == key {
			return r.Enabled, true
		}
	}
	return false, false
}
```

## Review

The store is correct when a caller cannot reach its state through a returned
slice: `TestGetAllIsDefensiveCopy` sorts, appends, mutates, and truncates the
result and the store stays key-sorted with its flags intact. If it regresses,
`GetAll` is leaking `s.records`. `TestAliasingBugCorruptsStore` is the deliberate
counter-example — it exercises `getAllAliased` and asserts the corruption happens,
so the lesson's claim ("returning the raw slice corrupts canonical state") is
proven, not just asserted. The `RWMutex` and the clone are both load-bearing: the
lock guards the header during `Set`, the clone guards the data after `GetAll`
returns; `-race` with concurrent readers and a writer confirms the lock covers
every access.

## Resources

- [slices package (`Clone`, `Insert`, `BinarySearchFunc`)](https://pkg.go.dev/slices)
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex)
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-window-queue-copy-on-push.md](01-window-queue-copy-on-push.md) | Next: [03-three-index-buffer-handoff.md](03-three-index-buffer-handoff.md)
