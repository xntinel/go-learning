# Exercise 7: Sorting a Listing Endpoint in Locale Order

`ORDER BY name` looks trivial until a Swedish user opens the list and finds `Åberg`
filed under `A` instead of after `Z`, or a mixed-case list where every capitalized
name sorts before every lowercase one. Byte order is not human order. This module
builds the collator a listing endpoint uses to sort names the way the target
locale expects, with options for numeric and case-insensitive ordering.

This module is fully self-contained: its own `go mod init`, its own demo and tests.
It uses `golang.org/x/text/collate` and `golang.org/x/text/language`.

## What you'll build

```text
collatelist/                independent module: example.com/collatelist
  go.mod                    requires golang.org/x/text
  sorter.go                 Sorter wrapping a *collate.Collator; Sort, Compare
  cmd/demo/main.go          Swedish order vs byte order; numeric order
  sorter_test.go            locale != byte order; numeric; case-insensitive; golden slice
```

Files: `sorter.go`, `cmd/demo/main.go`, `sorter_test.go`.
Implement: a `Sorter` built from `collate.New(tag, opts...)` exposing `Sort([]string)` (via `SortStrings`) and `Compare(a, b) int` (via `CompareString`).
Test: a Swedish sort orders a name set differently from `sort.Strings`; `collate.Numeric` sorts `item2 < item12`; `collate.IgnoreCase` makes `apple`/`Apple` compare equal; a deterministic golden slice.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/07-locale-aware-collation-for-listings/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/07-locale-aware-collation-for-listings
go get golang.org/x/text/collate
```

### Collation is locale order, and it is multi-level

Collation answers "which string comes first?", and unlike normalization it is
inherently locale-dependent. `collate.New(language.Make("sv"))` builds a Swedish
collator that places `å`, `ä`, and `ö` *after* `z`; the same names under
`sort.Strings` (raw byte order) scatter — uppercase Latin sorts before lowercase,
and the accented letters land wherever their code points happen to fall. The
collator implements the Unicode Collation Algorithm, which is *multi-level*: the
primary level compares base letters, the secondary level breaks ties on accents,
and the tertiary level breaks ties on case. Options select which levels participate:

- `collate.Numeric` makes a run of digits sort by numeric value, so `item2`
  precedes `item12` instead of following it (byte order compares `1` < `2`).
- `collate.IgnoreCase` drops the tertiary (case) level, so `apple` and `Apple`
  compare equal.
- `collate.IgnoreDiacritics` drops the secondary (accent) level.
- `collate.Loose` is a convenience that ignores case, width, and diacritics at
  once — useful for a forgiving search-style comparison.

The wrapper is a `Sorter` holding one `*collate.Collator`. The important operational
fact: a `*collate.Collator` is *stateful* and **not safe for concurrent use** — its
`SortStrings` and `CompareString` mutate an internal buffer. A listing endpoint
therefore builds a `Sorter` per request (or per goroutine), or guards a shared one
with a mutex; it never shares one collator across concurrent handlers.

Create `sorter.go`:

```go
package collatelist

import (
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Sorter wraps a locale Collator. A *collate.Collator is stateful and NOT safe
// for concurrent use, so build one Sorter per goroutine/request or guard it.
type Sorter struct {
	c *collate.Collator
}

// NewSorter builds a Sorter for tag with the given options (e.g. collate.Numeric,
// collate.IgnoreCase, collate.Loose).
func NewSorter(tag language.Tag, opts ...collate.Option) *Sorter {
	return &Sorter{c: collate.New(tag, opts...)}
}

// Sort orders names in place in locale order.
func (s *Sorter) Sort(names []string) {
	s.c.SortStrings(names)
}

// Compare reports whether a sorts before (-1), equal to (0), or after (+1) b.
func (s *Sorter) Compare(a, b string) int {
	return s.c.CompareString(a, b)
}
```

### The runnable demo

The demo sorts a Swedish name set both ways so the divergence is visible, then
shows numeric ordering placing `item2` before `item12`. The collation options
(`collate.Numeric` and friends) are ordinary values passed straight to `NewSorter`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/collatelist"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

func main() {
	names := []string{"Zorn", "Ström", "Ahlgren", "Öberg", "aker", "Åberg"}

	sv := collatelist.NewSorter(language.Make("sv"))
	svOrder := append([]string(nil), names...)
	sv.Sort(svOrder)
	fmt.Println("swedish:", svOrder)

	byteOrder := append([]string(nil), names...)
	sort.Strings(byteOrder)
	fmt.Println("bytes:  ", byteOrder)

	items := []string{"item12", "item2", "item1"}
	numeric := collatelist.NewSorter(language.Und, collate.Numeric)
	numeric.Sort(items)
	fmt.Println("numeric:", items)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
swedish: [Ahlgren aker Ström Zorn Åberg Öberg]
bytes:   [Ahlgren Ström Zorn aker Åberg Öberg]
numeric: [item1 item2 item12]
```

### Tests

The tests pin the three behaviors a listing depends on and a golden Swedish order.
`TestSwedishDiffersFromByteOrder` proves the collator and `sort.Strings` disagree
and asserts the exact locale order. `TestNumericOrder` and `TestIgnoreCase` pin the
option semantics.

Create `sorter_test.go`:

```go
package collatelist

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

func TestSwedishDiffersFromByteOrder(t *testing.T) {
	t.Parallel()

	names := []string{"Zorn", "Ström", "Ahlgren", "Öberg", "aker", "Åberg"}

	svOrder := append([]string(nil), names...)
	NewSorter(language.Make("sv")).Sort(svOrder)

	byteOrder := append([]string(nil), names...)
	sort.Strings(byteOrder)

	want := []string{"Ahlgren", "aker", "Ström", "Zorn", "Åberg", "Öberg"}
	if !reflect.DeepEqual(svOrder, want) {
		t.Fatalf("swedish order = %v, want %v", svOrder, want)
	}
	if reflect.DeepEqual(svOrder, byteOrder) {
		t.Fatal("swedish collation must differ from byte order for this set")
	}
}

func TestNumericOrder(t *testing.T) {
	t.Parallel()

	items := []string{"item12", "item2", "item1"}
	NewSorter(language.Und, collate.Numeric).Sort(items)
	want := []string{"item1", "item2", "item12"}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("numeric order = %v, want %v", items, want)
	}
}

func TestIgnoreCase(t *testing.T) {
	t.Parallel()

	s := NewSorter(language.Und, collate.IgnoreCase)
	if got := s.Compare("apple", "Apple"); got != 0 {
		t.Fatalf("IgnoreCase Compare(apple, Apple) = %d, want 0", got)
	}
	// Without IgnoreCase, the two are not equal at the tertiary level.
	plain := NewSorter(language.Und)
	if plain.Compare("apple", "Apple") == 0 {
		t.Fatal("case-sensitive collator should not treat apple and Apple as equal")
	}
}

func ExampleSorter_Compare() {
	s := NewSorter(language.Und, collate.Numeric)
	fmt.Println(s.Compare("item2", "item12"))
	// Output: -1
}
```

## Review

The sorter is correct when its order matches the target locale, not the byte
values: `NewSorter(language.Make("sv"))` files `Åberg` and `Öberg` after `Zorn`,
which `TestSwedishDiffersFromByteOrder` pins against both the golden slice and the
byte order it must differ from. `collate.Numeric` and `collate.IgnoreCase` select
which collation levels matter, and the tests pin each. The operational mistake to
avoid is sharing one `*collate.Collator` across concurrent requests — it mutates an
internal buffer and is not concurrency-safe, so each request builds its own
`Sorter` (or guards a shared one). The other mistake is shipping `sort.Strings` to
users at all: it mis-sorts every accented and mixed-case list.

## Resources

- [`golang.org/x/text/collate`](https://pkg.go.dev/golang.org/x/text/collate) — `New`, `Collator.SortStrings`/`CompareString`, and the `Numeric`/`IgnoreCase`/`Loose` options.
- [`golang.org/x/text/language`](https://pkg.go.dev/golang.org/x/text/language) — `Make`, `Und`, `Tag`.
- [UTS #10: Unicode Collation Algorithm](https://www.unicode.org/reports/tr10/) — the multi-level model behind the collator.

---

Back to [06-precis-username-and-password-enforcement.md](06-precis-username-and-password-enforcement.md) | Next: [08-collation-sort-keys-for-a-db-index.md](08-collation-sort-keys-for-a-db-index.md)
