# Exercise 5: IP-to-Metadata Lookup Over Sorted Non-Overlapping Ranges

Geo-IP, ASN, and allowlist layers all reduce to the same structure: a sorted set
of non-overlapping `[start, end]` IP ranges, each carrying a label, and a lookup
that answers "which range contains this address". That is a *floor* binary search
— the largest range whose start is at or below the address — followed by a
containment check against that range's end. The containment check is what
distinguishes a real hit from an address that falls in a gap between ranges.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
iprange/                     independent module: example.com/iprange
  go.mod
  table.go                   type Range, Table; New (validates), Lookup
  cmd/
    demo/
      main.go                classify several addresses
  table_test.go              below/above/inside/gap/boundaries, overlap rejection
```

Files: `table.go`, `cmd/demo/main.go`, `table_test.go`.
Implement: `Table` over sorted non-overlapping `[start,end]` `uint32` ranges with `New([]Range) (*Table, error)` and `Lookup(netip.Addr) (string, bool)`.
Test: address below all ranges, above all ranges, inside a range, in a gap, at exact start and exact end, and a constructor that rejects overlapping input.
Verify: `go test -count=1 -race ./...`

### Floor search, then the containment check that catches the gaps

An IPv4 address is a 32-bit number, so a range is a pair of `uint32` bounds and
the whole table is sorted by `Start`. `Lookup` converts the address to its
`uint32` form (`Addr.As4()` gives the four bytes, `binary.BigEndian.Uint32`
assembles them) and then does the floor search:

```go
i := sort.Search(len(ranges), func(i int) bool { return ranges[i].Start > ip })
```

That predicate — `Start > ip` — is monotone: false while starts are `<= ip`, true
after. `sort.Search` returns the first index whose start *exceeds* `ip`, so the
*floor* — the last range whose start is `<= ip` — is `i - 1`. If `i == 0` no range
starts at or below the address and the lookup misses.

The floor alone is not the answer. The floor range is only the *candidate*: the
address is inside it only if it is also `<= End`. Skipping this check is the
classic bug. Consider ranges `10.0.0.0-10.255.255.255` and
`192.168.0.0-192.168.255.255` and a query for `172.16.0.1`. The floor search
picks the `10.x` range (its start is the largest start `<= 172.16.0.1`), but
`172.16.0.1` is far past `10.255.255.255`. Without the `ip <= floor.End` check
you would wrongly label a gap address as belonging to the `10.x` range. The
containment check is what turns "nearest range below" into "the range that
actually contains the address".

`New` enforces the invariant the search depends on. It sorts the ranges by start,
rejects any range with `Start > End`, and rejects overlap: after sorting, each
range's start must be strictly greater than the previous range's end. An
overlapping table has no well-defined owner for the overlapping addresses, so the
constructor refuses it with a wrapped sentinel error rather than letting `Lookup`
return whichever range the floor search happens to land on.

Create `table.go`:

```go
package iprange

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
)

// ErrBadRange is returned when a range has Start > End.
var ErrBadRange = errors.New("iprange: range start after end")

// ErrOverlap is returned when two ranges overlap.
var ErrOverlap = errors.New("iprange: overlapping ranges")

// Range is a closed [Start, End] interval of IPv4 addresses in uint32 form,
// labelled with metadata.
type Range struct {
	Start uint32
	End   uint32
	Label string
}

// Table is a sorted, non-overlapping set of labelled IP ranges.
type Table struct {
	ranges []Range
}

// New validates and builds a table. It sorts the ranges by Start, then rejects
// any inverted range (Start > End) and any overlap between adjacent ranges.
func New(ranges []Range) (*Table, error) {
	rs := slices.Clone(ranges)
	slices.SortFunc(rs, func(a, b Range) int {
		if a.Start < b.Start {
			return -1
		}
		if a.Start > b.Start {
			return 1
		}
		return 0
	})
	for i, r := range rs {
		if r.Start > r.End {
			return nil, fmt.Errorf("%w: [%d,%d]", ErrBadRange, r.Start, r.End)
		}
		if i > 0 && r.Start <= rs[i-1].End {
			return nil, fmt.Errorf("%w: [%d,%d] overlaps [%d,%d]",
				ErrOverlap, rs[i-1].Start, rs[i-1].End, r.Start, r.End)
		}
	}
	return &Table{ranges: rs}, nil
}

// asU32 converts an IPv4 (or IPv4-in-IPv6) address to its uint32 value. A real
// IPv6 address has no uint32 form and reports ok=false.
func asU32(addr netip.Addr) (uint32, bool) {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if !addr.Is4() {
		return 0, false
	}
	b := addr.As4()
	return binary.BigEndian.Uint32(b[:]), true
}

// Lookup returns the label of the range containing addr, or ("", false) if the
// address is below all ranges, above all ranges, or in a gap between ranges.
func (t *Table) Lookup(addr netip.Addr) (string, bool) {
	ip, ok := asU32(addr)
	if !ok {
		return "", false
	}
	i := sort.Search(len(t.ranges), func(i int) bool { return t.ranges[i].Start > ip })
	if i == 0 {
		return "", false // below every range's start
	}
	floor := t.ranges[i-1]
	if ip <= floor.End {
		return floor.Label, true
	}
	return "", false // in a gap: past floor.End, before the next start
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/netip"

	"example.com/iprange"
)

func main() {
	table, err := iprange.New([]iprange.Range{
		{Start: 0x0A000000, End: 0x0AFFFFFF, Label: "internal-10"},
		{Start: 0xC0A80000, End: 0xC0A8FFFF, Label: "lan-192.168"},
		{Start: 0xCB007100, End: 0xCB0071FF, Label: "doc-203.0.113"},
	})
	if err != nil {
		panic(err)
	}

	for _, s := range []string{"10.1.2.3", "192.168.5.5", "172.16.0.1", "8.8.8.8", "203.0.113.7"} {
		addr := netip.MustParseAddr(s)
		if label, ok := table.Lookup(addr); ok {
			fmt.Printf("%-13s -> %s\n", s, label)
		} else {
			fmt.Printf("%-13s -> (unknown)\n", s)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
10.1.2.3      -> internal-10
192.168.5.5   -> lan-192.168
172.16.0.1    -> (unknown)
8.8.8.8       -> (unknown)
203.0.113.7   -> doc-203.0.113
```

### Tests

The suite covers every position relative to the ranges: below all, above all,
inside, in a gap, and at the exact start and exact end of a range (both must be
hits, since the interval is closed). A separate test proves `New` rejects
overlapping input with `errors.Is(err, ErrOverlap)`, and another proves an
inverted range is rejected with `ErrBadRange`.

Create `table_test.go`:

```go
package iprange

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"testing"
)

func u32(s string) uint32 {
	b := netip.MustParseAddr(s).As4()
	return binary.BigEndian.Uint32(b[:])
}

func testTable(t *testing.T) *Table {
	t.Helper()
	tbl, err := New([]Range{
		{Start: u32("10.0.0.0"), End: u32("10.255.255.255"), Label: "internal-10"},
		{Start: u32("192.168.0.0"), End: u32("192.168.255.255"), Label: "lan"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tbl
}

func TestLookup(t *testing.T) {
	t.Parallel()

	tbl := testTable(t)
	cases := []struct {
		ip        string
		wantLabel string
		wantOK    bool
	}{
		{"0.0.0.1", "", false},                  // below all
		{"255.255.255.255", "", false},          // above all
		{"10.1.2.3", "internal-10", true},       // inside first
		{"172.16.0.1", "", false},               // gap between the two ranges
		{"192.168.99.1", "lan", true},           // inside second
		{"10.0.0.0", "internal-10", true},       // exact start boundary
		{"10.255.255.255", "internal-10", true}, // exact end boundary
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			t.Parallel()
			got, ok := tbl.Lookup(netip.MustParseAddr(tc.ip))
			if got != tc.wantLabel || ok != tc.wantOK {
				t.Fatalf("Lookup(%s) = %q,%v; want %q,%v", tc.ip, got, ok, tc.wantLabel, tc.wantOK)
			}
		})
	}
}

func TestNewRejectsOverlap(t *testing.T) {
	t.Parallel()

	_, err := New([]Range{
		{Start: 100, End: 200, Label: "a"},
		{Start: 150, End: 300, Label: "b"}, // overlaps a
	})
	if !errors.Is(err, ErrOverlap) {
		t.Fatalf("New with overlap: err = %v, want ErrOverlap", err)
	}
}

func TestNewRejectsInvertedRange(t *testing.T) {
	t.Parallel()

	_, err := New([]Range{{Start: 300, End: 100, Label: "bad"}})
	if !errors.Is(err, ErrBadRange) {
		t.Fatalf("New with inverted range: err = %v, want ErrBadRange", err)
	}
}

func TestNewSortsInput(t *testing.T) {
	t.Parallel()

	// Deliberately unsorted input: New must sort it so Lookup works.
	tbl, err := New([]Range{
		{Start: u32("192.168.0.0"), End: u32("192.168.255.255"), Label: "lan"},
		{Start: u32("10.0.0.0"), End: u32("10.255.255.255"), Label: "internal-10"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, ok := tbl.Lookup(netip.MustParseAddr("10.1.1.1")); !ok || got != "internal-10" {
		t.Fatalf("Lookup after unsorted New = %q,%v; want internal-10,true", got, ok)
	}
}

func Example() {
	tbl, _ := New([]Range{
		{Start: u32("10.0.0.0"), End: u32("10.255.255.255"), Label: "internal-10"},
	})
	label, ok := tbl.Lookup(netip.MustParseAddr("10.9.9.9"))
	fmt.Println(label, ok)
	// Output: internal-10 true
}
```

## Review

The lookup is correct when the floor search finds the candidate range and the
containment check accepts it only when the address is truly inside. The gap case
(`172.16.0.1`) is the load-bearing test: remove the `ip <= floor.End` check and it
starts returning `internal-10`, silently mislabeling every gap address as the
nearest range below it. The closed-interval boundary tests pin that both `Start`
and `End` are inclusive. And `New` rejecting overlap is what lets `Lookup` assume
a unique owner — without it, the answer would depend on where the floor search
happened to land. Run `go test -race`.

## Resources

- [`net/netip`](https://pkg.go.dev/net/netip) — `Addr`, `ParseAddr`, `Addr.As4`.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `BigEndian.Uint32` to turn four address bytes into a comparable integer.
- [`sort.Search`](https://pkg.go.dev/sort#Search) — the floor search via the `Start > ip` predicate.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-weighted-router.md](06-weighted-router.md)
