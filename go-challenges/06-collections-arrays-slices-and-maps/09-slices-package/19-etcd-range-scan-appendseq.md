# Exercise 19: Reuse A Range-Scan Buffer With slices.AppendSeq

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An etcd MVCC keyspace, a BoltDB bucket, or any B-tree-backed store answers a
prefix query the same way: locate the first key that could match with a
binary search, then walk forward yielding entries until the prefix stops
matching. A poller built on top of that -- a lease-expiry sweep, a routing
table refresh -- runs the identical scan over and over, often many times a
second, and each call's result is meant to *replace* the previous one, not
extend it. `slices.Collect` turns any `iter.Seq[T]` into a slice, but it always
allocates a brand-new backing array; a hot poller calling it every tick pays
for a fresh allocation it does not need, since it is about to throw the
previous result away anyway. `slices.AppendSeq(buf, seq)` appends a sequence
onto an existing slice, reusing `buf`'s capacity when there is enough of it --
the tool for exactly this shape of repeated call.

The trap is not reaching for `append` to reuse the buffer -- that part is
right. It is forgetting the one line that makes reuse mean "replace" instead
of "extend": `buf[:0]` before the append, which resets the length to zero
while keeping the capacity (and therefore the backing array) intact. Skip it,
and `append(buf, newResults...)` writes the new results *after* whatever was
already in `buf`, because `append` only ever knows about the length it is
given, not about the caller's intent. The bug does not show up on the first
call, because there is nothing to accumulate onto yet. It shows up on the
second call, and the third, and by the hundredth tick of a poller that has
been "reusing" its buffer all along, the buffer holds the union of every scan
it has ever run, not the current state of anything.

This module builds `Store`, an immutable snapshot of a sorted keyspace, and
`ScanInto`, the method that gets the reuse-without-accumulation contract right
by construction: it truncates before it appends, so the caller cannot forget
the one line that matters.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
rangescan/                module example.com/rangescan
  go.mod                   go 1.24
  rangescan.go             KV, Store; NewStore, Range, ScanInto; ErrUnsorted
  rangescan_test.go        prefix table, buffer-reuse-replaces-not-accumulates,
                           the naive-accumulation contrast, concurrency,
                           ExampleStore_ScanInto
```

- Files: `rangescan.go`, `rangescan_test.go`.
- Implement: `NewStore(kvs []KV) (*Store, error)` validating `kvs` is sorted ascending by `Key` and returning `ErrUnsorted` otherwise; `(*Store).Range(prefix string) iter.Seq[KV]` yielding every entry whose key starts with `prefix`, located by binary search rather than a full scan; `(*Store).ScanInto(buf []KV, prefix string) []KV` returning `slices.AppendSeq(buf[:0], s.Range(prefix))`.
- Test: keyspace construction (sorted, empty, unsorted rejected); prefix matching across a run in the middle, the tail, an empty prefix, no match, and a single exact key; a `ScanInto` reuse test proving the second call's result excludes the first call's entries; a `scanNaive` contrast proving the same two calls through a truncate-forgetting helper accumulate instead of replace; concurrent callers each keeping their own buffer; and `ExampleStore_ScanInto` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Reuse means truncate-then-append, and the truncate is easy to lose

`slices.AppendSeq` has a simple contract: it is `append`, specialized to take
its new elements from an `iter.Seq[E]` instead of a variadic argument, and it
returns the grown slice exactly as `append` would. Nothing about `AppendSeq`
resets anything -- it appends onto whatever length `buf` already has, the
same as `append(buf, x, y, z)` would. That is correct and expected; it is
also exactly why `buf[:0]` has to be the caller's job, not the function's.
`s[:0]` is a re-slice, not an allocation: it produces a new slice header over
the same backing array, with length zero and the original capacity intact.
`append` (or `AppendSeq`) onto that header writes starting at index zero,
reusing every byte of capacity that was already there, and only grows the
backing array if the new content does not fit.

`ScanInto` folds that re-slice into the method itself:

```go
func (s *Store) ScanInto(buf []KV, prefix string) []KV {
    return slices.AppendSeq(buf[:0], s.Range(prefix))
}
```

so the caller's obligation shrinks to "pass back whatever I got last time" --
there is no separate step to remember. Compare that to the version obtained by
skipping the truncation, which is what a first attempt at "reuse this buffer"
usually looks like:

```go
func scanNaive(buf []KV, s *Store, prefix string) []KV {
    for kv := range s.Range(prefix) {
        buf = append(buf, kv)   // appends after whatever buf already held
    }
    return buf
}
```

The first call behaves identically to the correct version, because `buf`
starts empty either way. The second call is where they diverge: `scanNaive`
appends the new prefix's matches onto the tail of the previous call's matches,
so a poller that keeps calling `buf = scanNaive(buf, store, prefix)` once per
tick ends every tick with strictly more entries than the last, all of them
"current" as far as the type system can tell. Nothing panics, nothing errors,
the slice header comes back looking exactly like a valid result -- it is just
the wrong one, and it gets more wrong with every call.

Create `rangescan.go`:

```go
// Package rangescan models the prefix-range scan at the heart of an
// etcd-style MVCC keyspace or a BoltDB bucket: a sorted key-value snapshot,
// and repeated queries for every key under a given prefix. A poller that
// re-runs the same scan many times a second -- expiring stale leases,
// refreshing a routing table -- wants to reuse one scratch buffer across
// calls instead of allocating a fresh result slice every tick.
package rangescan

import (
	"cmp"
	"errors"
	"iter"
	"slices"
	"strings"
)

// KV is one key-value pair in the keyspace.
type KV struct {
	Key   string
	Value string
}

// ErrUnsorted is returned by NewStore when kvs is not sorted ascending by
// Key: Range's binary search requires that invariant and never checks it
// itself.
var ErrUnsorted = errors.New("rangescan: keyspace must be sorted by key")

// Store is an immutable, key-sorted snapshot of a keyspace.
//
// Store is safe for concurrent use by multiple goroutines: Range only
// reads the snapshot taken at construction. The buf argument to ScanInto is
// not shared automatically -- each goroutine that wants to reuse a scratch
// buffer across calls must keep its own.
type Store struct {
	kvs []KV
}

// NewStore returns a Store snapshotting kvs, which must already be sorted
// ascending by Key. It returns ErrUnsorted otherwise. The Store clones kvs,
// so later mutation of the caller's slice does not affect it.
func NewStore(kvs []KV) (*Store, error) {
	if !slices.IsSortedFunc(kvs, func(a, b KV) int { return cmp.Compare(a.Key, b.Key) }) {
		return nil, ErrUnsorted
	}
	return &Store{kvs: slices.Clone(kvs)}, nil
}

// Range returns an iterator over every KV whose Key starts with prefix, in
// ascending key order. It locates the start with a binary search and stops
// as soon as the prefix no longer matches, costing O(log n + k) for k
// matches rather than a full scan.
//
// Each KV yielded is a copy; the sequence does not alias the Store's
// internal slice, and a value pulled from it may be retained past the
// iterator's lifetime.
func (s *Store) Range(prefix string) iter.Seq[KV] {
	return func(yield func(KV) bool) {
		idx, _ := slices.BinarySearchFunc(s.kvs, prefix, func(kv KV, p string) int {
			return strings.Compare(kv.Key, p)
		})
		for _, kv := range s.kvs[idx:] {
			if !strings.HasPrefix(kv.Key, prefix) {
				return
			}
			if !yield(kv) {
				return
			}
		}
	}
}

// ScanInto scans prefix into buf, reusing buf's backing array across
// repeated calls: pass back whatever a previous call returned (or nil the
// first time). Once buf's capacity covers the largest result seen so far,
// later calls with the same or a smaller result size allocate nothing.
//
// ScanInto always truncates buf to length zero before appending, so each
// call's result reflects only that call's scan -- never leftover entries
// from a previous one. The returned slice aliases buf's backing array; it
// does not alias the Store.
func (s *Store) ScanInto(buf []KV, prefix string) []KV {
	return slices.AppendSeq(buf[:0], s.Range(prefix))
}
```

### Using it

A caller that polls the same kind of prefix repeatedly keeps one `[]KV`
variable across calls and always writes the result back to it:
`buf = store.ScanInto(buf, prefix)`. The first call allocates (there is
nothing to reuse yet); once `buf`'s capacity has grown to cover the largest
result the poller ever sees, later calls with an equal or smaller result stop
allocating entirely, because `buf[:0]` inside `ScanInto` hands `AppendSeq` a
zero-length view over capacity that is already big enough. Nothing about that
behavior is the caller's responsibility to get right -- it falls out of
calling `ScanInto` the same way every time.

Two contracts are worth stating precisely because a caller could otherwise
guess wrong. First, the entries `Range` yields are independent copies: mutating
one after pulling it out of the sequence never touches the `Store`'s own data,
because `KV` is a plain value type with no pointer or slice fields to alias.
Second, `ScanInto`'s returned slice *does* alias the `buf` the caller passed
in -- that is the entire point of the method -- so a caller that wants to keep
one call's result around while starting the next scan must copy it out (with
`slices.Clone`, say) before calling `ScanInto` again with the same variable.

`ExampleStore_ScanInto` is the runnable demonstration of this module: `go
test` executes it and compares its stdout against the `// Output:` comment
below.

```go
func ExampleStore_ScanInto() {
	s, err := NewStore(keyspace())
	if err != nil {
		panic(err)
	}

	var buf []KV
	buf = s.ScanInto(buf, "/leases/")
	fmt.Println(len(buf), "leases")

	buf = s.ScanInto(buf, "/routes/")
	fmt.Println(len(buf), "routes")

	// Output:
	// 3 leases
	// 2 routes
}
```

The second `ScanInto` call reuses `buf`'s backing array from the first, and
its result is exactly the two `/routes/` entries -- not five, not the three
leases plus two routes. That is the property the naive, truncate-forgetting
version gets wrong.

### Tests

`TestNewStore` checks a sorted keyspace, an empty one, and an unsorted one
rejected with `ErrUnsorted`. `TestScanIntoPrefixes` is the prefix table: a run
of keys in the middle, the tail of the keyspace, an empty prefix that matches
everything, a prefix that matches nothing, and a prefix that happens to equal
one full key exactly. `TestScanIntoReplacesNotAccumulates` is the heart of the
module: it reuses one `buf` across two different-prefix scans and asserts the
second result has exactly the second scan's length and contains none of the
first scan's keys. `TestScanNaiveAccumulatesAcrossCalls` runs the identical
two scans through `scanNaive` and asserts the opposite: the second result's
length is the *sum* of both scans, pinning the exact accumulation defect the
correct version cannot produce. `TestScanIntoConcurrent` drives twenty
goroutines, each keeping its own buffer and scanning repeatedly, holding
`Store` to the concurrent-read safety its doc comment promises.

Create `rangescan_test.go`:

```go
package rangescan

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func keyspace() []KV {
	return []KV{
		{Key: "/leases/1", Value: "a"},
		{Key: "/leases/2", Value: "b"},
		{Key: "/leases/3", Value: "c"},
		{Key: "/routes/x", Value: "d"},
		{Key: "/routes/y", Value: "e"},
	}
}

// scanNaive is the buffer-reuse scan as it is usually written the first
// time: append onto buf without truncating it first. It looks like reuse
// -- the same backing array does get reused once it is big enough -- but
// every call's results pile on top of the previous call's, instead of
// replacing them. Never exported, never reachable from Store.
func scanNaive(buf []KV, s *Store, prefix string) []KV {
	for kv := range s.Range(prefix) {
		buf = append(buf, kv)
	}
	return buf
}

func TestNewStore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kvs     []KV
		wantErr error
	}{
		{name: "sorted", kvs: keyspace()},
		{name: "empty", kvs: nil},
		{name: "unsorted", kvs: []KV{{Key: "/b"}, {Key: "/a"}}, wantErr: ErrUnsorted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewStore(tc.kvs)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewStore error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestScanIntoPrefixes(t *testing.T) {
	t.Parallel()

	s, err := NewStore(keyspace())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	tests := []struct {
		name     string
		prefix   string
		wantKeys []string
	}{
		{name: "matches a run in the middle", prefix: "/leases/", wantKeys: []string{"/leases/1", "/leases/2", "/leases/3"}},
		{name: "matches the tail", prefix: "/routes/", wantKeys: []string{"/routes/x", "/routes/y"}},
		{name: "empty prefix matches everything", prefix: "", wantKeys: []string{"/leases/1", "/leases/2", "/leases/3", "/routes/x", "/routes/y"}},
		{name: "no match", prefix: "/zzz/", wantKeys: nil},
		{name: "exact single key", prefix: "/leases/2", wantKeys: []string{"/leases/2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := s.ScanInto(nil, tc.prefix)
			var gotKeys []string
			for _, kv := range got {
				gotKeys = append(gotKeys, kv.Key)
			}
			if !slices.Equal(gotKeys, tc.wantKeys) {
				t.Fatalf("keys = %v, want %v", gotKeys, tc.wantKeys)
			}
		})
	}
}

// TestScanIntoReplacesNotAccumulates is the heart of the module: reusing
// buf across two different scans must yield only the second scan's
// results, never the first scan's leftovers mixed in.
func TestScanIntoReplacesNotAccumulates(t *testing.T) {
	t.Parallel()

	s, err := NewStore(keyspace())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var buf []KV
	buf = s.ScanInto(buf, "/leases/")
	if len(buf) != 3 {
		t.Fatalf("first scan len = %d, want 3", len(buf))
	}
	buf = s.ScanInto(buf, "/routes/")
	if len(buf) != 2 {
		t.Fatalf("second scan len = %d, want 2 (got leftover /leases/ entries)", len(buf))
	}
	for _, kv := range buf {
		if kv.Key == "/leases/1" {
			t.Fatalf("second scan result %v still contains a first-scan key", buf)
		}
	}
}

// TestScanNaiveAccumulatesAcrossCalls shows the bug scanNaive ships: the
// same two scans through the naive helper leave the first scan's entries
// in place, so a poller reusing buf across ticks accretes stale results
// forever instead of replacing them.
func TestScanNaiveAccumulatesAcrossCalls(t *testing.T) {
	t.Parallel()

	s, err := NewStore(keyspace())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var buf []KV
	buf = scanNaive(buf, s, "/leases/")
	if len(buf) != 3 {
		t.Fatalf("first naive scan len = %d, want 3", len(buf))
	}
	buf = scanNaive(buf, s, "/routes/")
	if len(buf) != 5 {
		t.Fatalf("second naive scan len = %d, want 5 (3 stale + 2 fresh); the bug should have accumulated", len(buf))
	}
}

func TestScanIntoConcurrent(t *testing.T) {
	t.Parallel()

	s, err := NewStore(keyspace())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []KV // each goroutine keeps its own scratch buffer
			for range 10 {
				buf = s.ScanInto(buf, "/leases/")
				if len(buf) != 3 {
					t.Errorf("len(buf) = %d, want 3", len(buf))
				}
			}
		}()
	}
	wg.Wait()
}

// ExampleStore_ScanInto is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment.
func ExampleStore_ScanInto() {
	s, err := NewStore(keyspace())
	if err != nil {
		panic(err)
	}

	var buf []KV
	buf = s.ScanInto(buf, "/leases/")
	fmt.Println(len(buf), "leases")

	buf = s.ScanInto(buf, "/routes/")
	fmt.Println(len(buf), "routes")

	// Output:
	// 3 leases
	// 2 routes
}
```

## Review

`ScanInto` is correct when a call's result reflects exactly that call's scan,
regardless of how many times the same buffer has been reused before it --
`buf[:0]` inside the method guarantees that by construction, so the caller has
nothing to remember. The mistake it avoids is `append(buf, newEntries...)`
without the truncation: syntactically valid reuse, semantically an unbounded
accumulator, and invisible on the very first call because there is nothing
yet to accumulate onto. `NewStore` rejects a keyspace that is not sorted
ascending by key with `ErrUnsorted`, checkable with `errors.Is`, since `Range`
depends on that invariant for its binary search and never checks it itself.
`Range` yields independent copies that never alias the `Store`; `ScanInto`'s
result aliases the caller's own buffer, by design, so a caller who needs to
keep one result while starting the next scan must clone it first. `Store` is
immutable after construction and safe for concurrent `Range` and `ScanInto`
calls, provided each goroutine keeps its own buffer. Run
`go test -count=1 -race ./...` to confirm the prefix table, the
replace-not-accumulate contract, the naive contrast, and the concurrent
scan.

## Resources

- [`slices.AppendSeq`](https://pkg.go.dev/slices#AppendSeq) — appends an `iter.Seq[E]` onto an existing slice, the operation this module is built around.
- [`slices.Collect`](https://pkg.go.dev/slices#Collect) — the always-allocating counterpart `AppendSeq` avoids paying for on every call.
- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — locates the start of a prefix run in O(log n) rather than scanning from the beginning.
- [etcd: Data Model](https://etcd.io/docs/latest/learning/data_model/) — the MVCC keyspace and prefix-range query this module's `Store` is modeled on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-prometheus-bucket-sorted-dedup.md](18-prometheus-bucket-sorted-dedup.md) | Next: [20-reconnect-backoff-bounded-collect.md](20-reconnect-backoff-bounded-collect.md)
