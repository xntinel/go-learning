# Exercise 11: DNS RRset Equal-Range Lookup

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An authoritative DNS resolver like CoreDNS keeps a parsed zone file in memory
as a sorted table of resource records, queried thousands of times per second
and mutated only when the zone reloads. Most owner names carry exactly one
record, but round-robin load balancing and SRV service discovery both put
several records under the same owner name on purpose: three `A` records for
`api.example.com.` so DNS round robin spreads client connections across three
backends, or two `SRV` records for `_sip._tcp.example.com.` with different
priorities. A lookup that returns only one of them is not a crash — it is a
resolver that quietly disables load distribution for every client that asks.

That is precisely the trap a single binary search sets. `slices.BinarySearch`
and its `Func` variant answer "where would this go, and is it here" with one
`(pos, found)` pair, and `pos` is always the *first* match in a run of equal
elements — the lower bound. Trusting `found` and returning `records[pos]`
alone is correct exactly when the owner name has one record and wrong every
other time. The fix this module builds is the equal-range: a lower-bound
search for the first record at or after the name, and a second, differently
biased search for the first record strictly after it. Everything between
those two indices belongs to the query.

This module builds `dnszone`, a package that holds a zone's records sorted by
owner name and answers `Lookup(name)` with every record under that name, in
the order the zone was built with. The single-index shortcut is not part of
that API; it lives only in the tests, where it belongs, as the thing the
tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
dnszone/                 module example.com/dnszone
  go.mod                 go 1.24
  zone.go                Record, Zone; NewZone, Lookup, Len; two sentinel errors
  zone_test.go           lookup table, empty zone, sortedness/name guards,
                         aliasing, the single-index contrast, concurrency,
                         ExampleZone_Lookup
```

- Files: `zone.go`, `zone_test.go`.
- Implement: `NewZone(records []Record) (*Zone, error)` cloning its input and rejecting `ErrEmptyName` or `ErrNotSorted`; `(*Zone).Lookup(name string) []Record` returning the whole equal-range for `name` via two `slices.BinarySearchFunc` calls (a lower bound and a strictly-greater upper bound); `(*Zone).Len() int`.
- Test: the round-robin `A` set, the `SRV` set, a single-record owner, a name below/above/in a gap of all owners, an empty zone, `NewZone` rejecting unsorted input and an empty name, the returned slice never aliasing the zone's storage, a `lookupBuggy` contrast proving the single-index shortcut drops records a round-robin set depends on, concurrent `Lookup` calls, and `ExampleZone_Lookup` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dnszone
cd ~/go-exercises/dnszone
go mod init example.com/dnszone
go mod edit -go=1.24
```

### One index answers "is it here"; two indices answer "give me all of them"

`slices.BinarySearchFunc(records, name, cmp)` returns the lower bound of
`name` in the sorted slice — the first index whose record's `Name` is `>=
name` — together with whether that record's `Name` is exactly equal. That
index is a single element. Duplicates present in the slice sit at higher
indices the call never reports, because `BinarySearchFunc` was never asked
about them:

```go
i, found := slices.BinarySearchFunc(records, name, cmpByName)
if !found {
    return nil
}
return records[i : i+1]   // the bug: only the first of N matching records
```

For `api.example.com.` with three `A` records, this returns one. The
resolver logs a healthy lookup, the test that checks "the response contains
an A record" passes, and every client that resolves the name gets routed to
the same single backend forever.

The equal-range fix runs the same primitive twice with two different
comparators. The first call is the ordinary lower bound. The second call
reuses the exact same `BinarySearchFunc`, but its comparator treats an equal
`Name` as *less than* the target — returns `-1` instead of `0` — which folds
the whole run of matching records into the search's "before" partition and
leaves the boundary sitting on the first record that is genuinely greater.
The result is `[lo, hi)`: every record with that owner name, in the order the
zone holds them, and nothing else. `NewZone` protects the invariant both
searches depend on: it clones its input and rejects it with `ErrNotSorted`
unless the records already come sorted by `Name`, the same validate-at-the-
boundary discipline the rest of this lesson relies on.

Create `zone.go`:

```go
// Package dnszone implements an in-memory authoritative DNS zone, modeled on
// the way CoreDNS and similar resolvers hold a parsed zone file: records
// sorted once by owner name, then queried many times.
//
// The one detail this package exists to get right is duplicate owner names.
// A round-robin A record set or an SRV record set puts several Records under
// the same Name, and a query for that name must return every one of them, not
// just the first match a single binary search happens to land on.
package dnszone

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
)

// Sentinel errors returned by NewZone. Callers should test for them with
// errors.Is rather than by comparing error strings.
var (
	// ErrEmptyName means a Record had an empty owner name.
	ErrEmptyName = errors.New("dnszone: record has empty name")
	// ErrNotSorted means the input records were not sorted by Name.
	ErrNotSorted = errors.New("dnszone: records not sorted by name")
)

// Record is one resource record: an owner Name ("api.example.com."), its
// Type ("A", "AAAA", "SRV", ...), and its Value (the record's RDATA,
// formatted as a string).
type Record struct {
	Name  string
	Type  string
	Value string
}

// Zone is a set of Records held sorted by Name, ready for equal-range
// lookup.
//
// A Zone is immutable after construction: NewZone clones its input, and no
// method mutates the stored records. It is therefore safe for concurrent use
// by multiple goroutines without external synchronization.
type Zone struct {
	records []Record
}

// NewZone builds a Zone from records, which must already be sorted by Name
// (ties broken by input order, which Lookup preserves so a round-robin set
// keeps its configured order). NewZone clones records, so the caller's slice
// is never retained.
//
// It returns ErrEmptyName if any record has an empty Name, or ErrNotSorted
// if records is not sorted by Name. A nil or empty records is valid and
// produces an empty Zone.
func NewZone(records []Record) (*Zone, error) {
	rs := slices.Clone(records)
	for i, r := range rs {
		if r.Name == "" {
			return nil, fmt.Errorf("%w: at index %d", ErrEmptyName, i)
		}
	}
	if !slices.IsSortedFunc(rs, func(a, b Record) int { return cmp.Compare(a.Name, b.Name) }) {
		return nil, ErrNotSorted
	}
	return &Zone{records: rs}, nil
}

// lowerBoundByName returns the index of the first record whose Name is >=
// name -- the same insertion point slices.BinarySearchFunc always computes,
// used here even when found is false, since it is still the correct left
// edge of the equal-range.
func lowerBoundByName(records []Record, name string) int {
	i, _ := slices.BinarySearchFunc(records, name, func(r Record, n string) int {
		return cmp.Compare(r.Name, n)
	})
	return i
}

// upperBoundByName returns the index of the first record whose Name is
// strictly greater than name. It reuses slices.BinarySearchFunc with a
// comparator that treats an equal Name as "less than" the target (returns
// -1 instead of 0), which pushes the whole equal-name run into the search's
// "before" partition and leaves the boundary sitting on the first record
// that is genuinely greater.
func upperBoundByName(records []Record, name string) int {
	i, _ := slices.BinarySearchFunc(records, name, func(r Record, n string) int {
		if r.Name == n {
			return -1
		}
		return cmp.Compare(r.Name, n)
	})
	return i
}

// Lookup returns every Record whose Name equals name, in the order they were
// given to NewZone. It carves out the equal-range [lo, hi) with two
// BinarySearchFunc calls -- a lower bound and a strictly-greater upper bound
// -- rather than trusting a single found index, so a round-robin set of
// three A records comes back as three records, not one.
//
// The returned slice is a clone: it never aliases Zone's internal storage,
// so the caller may retain, sort, or mutate it freely. Lookup returns an
// empty slice when name is not present.
func (z *Zone) Lookup(name string) []Record {
	lo := lowerBoundByName(z.records, name)
	hi := upperBoundByName(z.records, name)
	return slices.Clone(z.records[lo:hi])
}

// Len reports the number of records held in the zone.
func (z *Zone) Len() int { return len(z.records) }
```

### Using it

`NewZone` is called once, at zone-load time, with the records already sorted
by `Name` — the way a zone file compiler or `AXFR` transfer would hand them
over. From then on every request-path call is `Lookup`, which never mutates
the `Zone` and therefore needs no lock: `TestZoneIsSafeForConcurrentUse`
holds the type to the concurrency contract its doc comment promises.

The aliasing contract matters just as much: `Lookup` clones the equal-range
before returning it, so a caller that sorts or mutates the result — to apply
its own load-balancing weight ordering, say — can never corrupt the zone's
own storage. `TestLookupDoesNotAliasInternalStorage` pins that by mutating a
result and checking a second `Lookup` for the same name comes back
unchanged.

`ExampleZone_Lookup` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment, so the
usage shown here cannot drift away from the code.

### Tests

`TestLookup` is the table: the three-record round-robin set, the two-record
`SRV` set, a single-record owner, and then every boundary a real resolver
hits — a name below all owners, above all owners, and in a gap between two
owners, all three expected to return zero records. `TestLookupEmptyZone` and
`TestNewZoneRejectsUnsorted` / `TestNewZoneRejectsEmptyName` cover the
constructor's edge cases and its two sentinel errors.

`TestBuggySingleIndexDropsRoundRobinRecords` is the heart of the module.
`lookupBuggy` is unexported and unreachable from the package API; it exists
so the test can state the defect numerically — it returns exactly one record
for the three-record owner name — and then show `Lookup` on the same input
returning all three. If a future edit reintroduces the single-index shortcut
into `Lookup`, this test fails here instead of in a client that only ever
resolves to one backend.

Create `zone_test.go`:

```go
package dnszone

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func sampleRecords() []Record {
	return []Record{
		{Name: "_sip._tcp.example.com.", Type: "SRV", Value: "10 60 5060 sipa.example.com."},
		{Name: "_sip._tcp.example.com.", Type: "SRV", Value: "20 60 5060 sipb.example.com."},
		{Name: "api.example.com.", Type: "A", Value: "203.0.113.10"},
		{Name: "api.example.com.", Type: "A", Value: "203.0.113.11"},
		{Name: "api.example.com.", Type: "A", Value: "203.0.113.12"},
		{Name: "www.example.com.", Type: "A", Value: "203.0.113.20"},
	}
}

// lookupBuggy is the mistake this module exists to prevent: it trusts the
// single (pos, found) result of one BinarySearchFunc call and returns only
// that one record. It is never exported and never reachable from the
// package API; it exists so the tests can pin what it silently drops.
func lookupBuggy(records []Record, name string) []Record {
	i, found := slices.BinarySearchFunc(records, name, func(r Record, n string) int {
		return cmp.Compare(r.Name, n)
	})
	if !found {
		return nil
	}
	return records[i : i+1]
}

func TestLookup(t *testing.T) {
	t.Parallel()

	recs := sampleRecords()
	z, err := NewZone(recs)
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}

	tests := []struct {
		name      string
		query     string
		wantCount int
	}{
		{name: "round robin A set, three records", query: "api.example.com.", wantCount: 3},
		{name: "SRV set, two records", query: "_sip._tcp.example.com.", wantCount: 2},
		{name: "single record owner", query: "www.example.com.", wantCount: 1},
		{name: "name below all owners", query: "aaa.example.com.", wantCount: 0},
		{name: "name above all owners", query: "zzz.example.com.", wantCount: 0},
		{name: "name in a gap", query: "gap.example.com.", wantCount: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := z.Lookup(tc.query)
			if len(got) != tc.wantCount {
				t.Fatalf("Lookup(%q) returned %d records, want %d: %+v", tc.query, len(got), tc.wantCount, got)
			}
			for _, r := range got {
				if r.Name != tc.query {
					t.Errorf("Lookup(%q) returned record with Name %q", tc.query, r.Name)
				}
			}
		})
	}
}

func TestLookupEmptyZone(t *testing.T) {
	t.Parallel()

	z, err := NewZone(nil)
	if err != nil {
		t.Fatalf("NewZone(nil): %v", err)
	}
	if got := z.Lookup("anything."); len(got) != 0 {
		t.Fatalf("Lookup on empty zone = %+v, want empty", got)
	}
	if z.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", z.Len())
	}
}

func TestNewZoneRejectsUnsorted(t *testing.T) {
	t.Parallel()

	recs := []Record{
		{Name: "b.example.com.", Type: "A", Value: "1.1.1.1"},
		{Name: "a.example.com.", Type: "A", Value: "2.2.2.2"},
	}
	if _, err := NewZone(recs); !errors.Is(err, ErrNotSorted) {
		t.Fatalf("NewZone error = %v, want ErrNotSorted", err)
	}
}

func TestNewZoneRejectsEmptyName(t *testing.T) {
	t.Parallel()

	recs := []Record{{Name: "", Type: "A", Value: "1.1.1.1"}}
	if _, err := NewZone(recs); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("NewZone error = %v, want ErrEmptyName", err)
	}
}

// TestLookupDoesNotAliasInternalStorage pins the aliasing contract: mutating
// the slice Lookup returns must never corrupt the Zone's own records.
func TestLookupDoesNotAliasInternalStorage(t *testing.T) {
	t.Parallel()

	z, err := NewZone(sampleRecords())
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}
	got := z.Lookup("api.example.com.")
	got[0].Value = "mutated"

	again := z.Lookup("api.example.com.")
	if again[0].Value == "mutated" {
		t.Fatalf("mutating a Lookup result corrupted the zone's internal records")
	}
}

// TestBuggySingleIndexDropsRoundRobinRecords is the heart of the module: it
// shows lookupBuggy silently dropping two of three A records in a
// round-robin set, then shows the real Lookup returning all three.
func TestBuggySingleIndexDropsRoundRobinRecords(t *testing.T) {
	t.Parallel()

	recs := sampleRecords()
	z, err := NewZone(recs)
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}

	buggy := lookupBuggy(recs, "api.example.com.")
	if len(buggy) != 1 {
		t.Fatalf("lookupBuggy returned %d records, want exactly 1 (the bug)", len(buggy))
	}

	good := z.Lookup("api.example.com.")
	if len(good) != 3 {
		t.Fatalf("Lookup returned %d records, want 3", len(good))
	}
	if len(good) <= len(buggy) {
		t.Fatalf("Lookup should return strictly more records than lookupBuggy: got %d vs %d", len(good), len(buggy))
	}
}

// TestZoneIsSafeForConcurrentUse exercises the concurrency contract the
// Zone doc comment makes: many goroutines calling Lookup on a shared,
// immutable Zone concurrently.
func TestZoneIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	z, err := NewZone(sampleRecords())
	if err != nil {
		t.Fatalf("NewZone: %v", err)
	}

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if got := z.Lookup("api.example.com."); len(got) != 3 {
				t.Errorf("Lookup returned %d records, want 3", len(got))
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// ExampleZone_Lookup is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleZone_Lookup() {
	z, err := NewZone([]Record{
		{Name: "api.example.com.", Type: "A", Value: "203.0.113.10"},
		{Name: "api.example.com.", Type: "A", Value: "203.0.113.11"},
		{Name: "api.example.com.", Type: "A", Value: "203.0.113.12"},
		{Name: "www.example.com.", Type: "A", Value: "203.0.113.20"},
	})
	if err != nil {
		panic(err)
	}

	for _, r := range z.Lookup("api.example.com.") {
		fmt.Println(r.Type, r.Value)
	}
	fmt.Println("records for gap.example.com.:", len(z.Lookup("gap.example.com.")))

	// Output:
	// A 203.0.113.10
	// A 203.0.113.11
	// A 203.0.113.12
	// records for gap.example.com.: 0
}
```

## Review

`Lookup` is correct when it returns every record sharing an owner name, not
just the one a single `BinarySearchFunc` call happens to find. The mechanism
is two calls to the same primitive with two different comparators: a lower
bound (the ordinary insertion point) and an upper bound built by treating an
equal `Name` as "less than" the target, which pushes the whole matching run
left of the boundary and leaves it sitting on the first genuinely greater
record. `records[lo:hi]` is then exactly the equal-range, cloned before it
leaves the package so the caller can never corrupt the zone's storage.
Around that core, `NewZone` clones its input and refuses it with
`ErrEmptyName` or `ErrNotSorted` rather than let `Lookup` silently
mis-answer a query against unsorted data, and `Zone` is immutable after
construction and therefore safe to share across goroutines. Run
`go test -count=1 -race ./...` to confirm the lookup table, the empty-zone
and validation edge cases, the aliasing guarantee, the single-index
contrast, and the concurrent-use property.

## Resources

- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — the generic search this module calls twice, with two different comparators.
- [`slices.IsSortedFunc`](https://pkg.go.dev/slices#IsSortedFunc) — the invariant check `NewZone` runs before trusting the search.
- [RFC 1035, section 4.1.1](https://www.rfc-editor.org/rfc/rfc1035#section-4.1.1) — the DNS message format that motivates several records sharing one owner name.
- [CoreDNS](https://coredns.io/manual/toc/#configuration) — a real authoritative resolver that keeps zone data sorted in memory for exactly this kind of lookup.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-invalid-search-guardrails.md](10-invalid-search-guardrails.md) | Next: [12-content-addressed-pack-index.md](12-content-addressed-pack-index.md)
