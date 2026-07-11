# Exercise 19: Order-Book Price-Level Insert-or-Aggregate

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A matching engine's order book is two sorted lists of price levels: bids
descending from the highest price a buyer will pay, asks ascending from the
lowest price a seller will accept. Every incoming limit order does one of two
things at its price: if a level already exists there, the order's quantity
joins that level's aggregate; if not, a new level opens at the correct sorted
position. Get this wrong and the book does not crash -- it quietly
fragments, `BestPrice` starts reading a level that no longer represents the
market's actual best price, and `DepthAt` reports a fraction of what is
really resting there. Every venue that publishes a depth-of-book feed --
exchanges, ECNs, the L2 order book behind a trading UI -- depends on this
insert-or-aggregate step being exact.

`slices.BinarySearchFunc` already computes everything this decision needs in
one call: `pos, found := slices.BinarySearchFunc(levels, price, cmp)` returns
`found=true` when a level already exists at `price`, with `pos` pointing at
it, and `found=false` with `pos` as the correct insertion index otherwise.
The subtlety this lesson's concepts flag directly -- `pos` is meaningful even
when `found` is false -- is exactly the branch this module is built around:
`found` selects *which* operation to run, and `pos` tells that operation
*where*, for both branches, not just one of them.

This module builds `Book`, one side of a matching engine's order book, using
that single search call to decide between adding to an existing level and
inserting a new one, for either side's ordering with one shared comparator.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
orderbook/                module example.com/orderbook
  go.mod                  go 1.24
  orderbook.go            Level, Side, Book; NewBook, AddOrder, BestPrice,
                          DepthAt, Levels; sentinel errors
  orderbook_test.go        ask/bid ordering tables, empty book, missing
                          price, invalid input, aliasing, the
                          ignored-found contrast, ExampleBook
```

- Files: `orderbook.go`, `orderbook_test.go`.
- Implement: `type Level struct { Price, Qty int64 }`; `type Side int` with `Bid` and `Ask`; `type Book struct { ... }`; `func NewBook(side Side) (*Book, error)`, rejecting an unrecognized `Side` with `ErrInvalidSide`; `(*Book).AddOrder(price, qty int64) error`, rejecting a non-positive price or quantity, otherwise joining an existing level or inserting a new one at its sorted position; `(*Book).BestPrice() (int64, bool)`; `(*Book).DepthAt(price int64) int64`; `(*Book).Levels() []Level`.
- Test: ask-side ordering with an aggregated middle level, bid-side ordering (descending), an empty book's `BestPrice`, `DepthAt` for a price with no level, `NewBook` rejecting an invalid `Side`, `AddOrder` rejecting a non-positive price or quantity, `Levels` never aliasing the book's storage, the ignored-`found` contrast, and `ExampleBook` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/orderbook
cd ~/go-exercises/orderbook
go mod init example.com/orderbook
go mod edit -go=1.24
```

### found chooses the operation; pos is where either operation happens

It is tempting to read `slices.BinarySearchFunc`'s two return values as "did
I find it" and, only if so, "where" -- and to treat `pos` as garbage
otherwise. That throws away exactly the information `AddOrder` needs for its
other branch. `pos` is always the correct insertion point, present or not;
`found` is what decides whether that point already holds a level to add
into or is where a new one belongs:

```go
pos, found := slices.BinarySearchFunc(b.levels, price, b.compare)
if found {
    b.levels[pos].Qty += qty                             // join the existing level
    return nil
}
b.levels = slices.Insert(b.levels, pos, Level{Price: price, Qty: qty}) // open a new one, here
```

Both branches use `pos`. Skip the `if found` check and always take the
insert branch and every order becomes its own level, even a second order at
a price the book already lists -- the book fragments into duplicate price
levels, `DepthAt` reads whichever one `BinarySearchFunc` happens to land on
and reports a fraction of the true resting quantity, and every consumer of
`Levels()` sees more price points than the market actually has. The fix is
not a smarter search; the search already told you both facts in one call.
The fix is using both of them.

The other piece worth noticing is that a bid book and an ask book are the
same data structure sorted in opposite directions, and this module gives
them one shared comparator instead of two. Mapping price through a `key`
function -- the price itself for asks, its negation for bids -- turns "sort
descending" into "sort ascending by a different number," so both sides can
reuse the identical `slices.BinarySearchFunc` call.

Create `orderbook.go`:

```go
// Package orderbook models one side of a limit-order matching engine's
// order book: price levels sorted by matching priority, where a new order
// either joins an existing level's aggregate quantity or opens a new level
// at its sorted position.
package orderbook

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
)

// Sentinel errors returned by NewBook and AddOrder. Callers should test for
// them with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidSide means NewBook was called with a Side other than Bid or Ask.
	ErrInvalidSide = errors.New("orderbook: invalid side")
	// ErrInvalidPrice means AddOrder was called with a non-positive price.
	ErrInvalidPrice = errors.New("orderbook: price must be positive")
	// ErrInvalidQty means AddOrder was called with a non-positive quantity.
	ErrInvalidQty = errors.New("orderbook: quantity must be positive")
)

// Side selects which way a Book's price levels are ordered by priority.
type Side int

const (
	// Bid orders a Book with the highest price first, the priority order
	// for the buy side of a matching engine.
	Bid Side = iota
	// Ask orders a Book with the lowest price first, the priority order
	// for the sell side of a matching engine.
	Ask
)

// Level is one price level: a price and the aggregate quantity of every
// resting order at that price.
type Level struct {
	Price int64
	Qty   int64
}

// Book is one side of a limit-order book: a sequence of price levels kept
// sorted by matching priority for its Side, ascending for Ask (lowest price
// first) or descending for Bid (highest price first).
//
// Book is not safe for concurrent use. Real matching engines process every
// order for a given book on a single goroutine or event loop, both for
// throughput and because price-time priority is only well defined when
// orders are applied one at a time in arrival order; the caller must
// synchronize access if that assumption does not hold.
type Book struct {
	side   Side
	levels []Level
}

// NewBook returns an empty Book ordered by side. It returns ErrInvalidSide
// if side is neither Bid nor Ask.
func NewBook(side Side) (*Book, error) {
	if side != Bid && side != Ask {
		return nil, fmt.Errorf("%w: %d", ErrInvalidSide, side)
	}
	return &Book{side: side}, nil
}

// key maps a price to the value this Book's levels are actually sorted by
// ascending, so both sides can share one binary-search comparator: for Ask
// the key is the price itself; for Bid it is the negated price, so the
// highest price sorts first while the underlying comparison stays ascending.
func (b *Book) key(price int64) int64 {
	if b.side == Bid {
		return -price
	}
	return price
}

func (b *Book) compare(level Level, price int64) int {
	return cmp.Compare(b.key(level.Price), b.key(price))
}

// AddOrder adds qty at price to the book. If a level already exists at
// price, qty is added to that level's aggregate quantity. Otherwise a new
// level is inserted at its sorted position. AddOrder returns ErrInvalidPrice
// or ErrInvalidQty if either argument is not positive.
func (b *Book) AddOrder(price, qty int64) error {
	if price <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidPrice, price)
	}
	if qty <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidQty, qty)
	}

	pos, found := slices.BinarySearchFunc(b.levels, price, b.compare)
	if found {
		b.levels[pos].Qty += qty
		return nil
	}
	// Reassign: Insert may grow and reallocate the backing array.
	b.levels = slices.Insert(b.levels, pos, Level{Price: price, Qty: qty})
	return nil
}

// BestPrice returns the price of the level with the best matching priority
// -- the lowest ask or the highest bid -- and false if the book is empty.
func (b *Book) BestPrice() (int64, bool) {
	if len(b.levels) == 0 {
		return 0, false
	}
	return b.levels[0].Price, true
}

// DepthAt returns the aggregate quantity resting at price, or 0 if no level
// exists at that price.
func (b *Book) DepthAt(price int64) int64 {
	pos, found := slices.BinarySearchFunc(b.levels, price, b.compare)
	if !found {
		return 0
	}
	return b.levels[pos].Qty
}

// Levels returns every price level in priority order, as a freshly
// allocated copy that does not alias the Book's internal storage: the
// caller may retain or mutate it without affecting the book.
func (b *Book) Levels() []Level {
	return slices.Clone(b.levels)
}
```

### Using it

Construct one `Book` per side per traded instrument with `NewBook(Ask)` or
`NewBook(Bid)`, then feed it every incoming limit order through `AddOrder`.
Because `Book` is not safe for concurrent use, the standard shape is one
goroutine per book -- an event loop reading orders off a channel -- rather
than a shared `Book` guarded by a mutex; that keeps price-time priority
well defined without lock contention on the hot path. `BestPrice` and
`DepthAt` are the read side a market-data publisher or a matching routine
would call after every `AddOrder`; `Levels` is the snapshot a depth-of-book
feed would serialize, and it never aliases the book's internal slice, so a
caller can hold onto or mutate the snapshot after the book itself has moved
on.

`ExampleBook` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment below.

```go
func ExampleBook() {
	bids, err := NewBook(Bid)
	if err != nil {
		panic(err)
	}
	for _, o := range []struct{ price, qty int64 }{
		{100, 10}, {101, 5}, {100, 3}, {99, 7},
	} {
		if err := bids.AddOrder(o.price, o.qty); err != nil {
			panic(err)
		}
	}

	best, _ := bids.BestPrice()
	fmt.Println("best bid:", best)
	fmt.Println("depth at 100:", bids.DepthAt(100))
	for _, l := range bids.Levels() {
		fmt.Printf("%d @ %d\n", l.Qty, l.Price)
	}

	// Output:
	// best bid: 101
	// depth at 100: 13
	// 5 @ 101
	// 13 @ 100
	// 7 @ 99
}
```

### Tests

`TestAddOrderAsk` and `TestAddOrderBid` each drive five orders through a
book, including two orders landing on the same price, and check the final
`Levels()` and `BestPrice()` against the ordering each side promises.
`TestBestPriceEmptyBook` and `TestDepthAtMissingPrice` are the two read-side
edge cases: nothing to read, and reading a price the book never opened a
level for. `TestNewBookRejectsInvalidSide` and
`TestAddOrderRejectsInvalidInput` cover construction and the two ways an
order can be malformed. `TestLevelsDoesNotAliasBook` pins the aliasing
contract on the one method that returns a slice.

`TestIgnoringFoundFragmentsTheBook` is the heart of the module.
`addOrderFragmenting` is unexported and unreachable from the package API: it
runs the identical `BinarySearchFunc` call `AddOrder` does but always takes
the insert branch, discarding `found`. The test pins the numeric defect --
two orders at the same price become two levels, not one, and neither level
alone reports the true resting quantity -- against the same two orders
through `Book.AddOrder` producing a single, correctly aggregated level.

Create `orderbook_test.go`:

```go
package orderbook

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestAddOrderAsk(t *testing.T) {
	t.Parallel()

	book, err := NewBook(Ask)
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	orders := []struct{ price, qty int64 }{
		{101, 10}, {99, 5}, {100, 3}, {99, 2}, {102, 1},
	}
	for _, o := range orders {
		if err := book.AddOrder(o.price, o.qty); err != nil {
			t.Fatalf("AddOrder(%d, %d): %v", o.price, o.qty, err)
		}
	}

	want := []Level{{99, 7}, {100, 3}, {101, 10}, {102, 1}}
	if got := book.Levels(); !slices.Equal(got, want) {
		t.Fatalf("Levels() = %+v, want %+v", got, want)
	}
	if best, ok := book.BestPrice(); !ok || best != 99 {
		t.Fatalf("BestPrice() = (%d, %v), want (99, true)", best, ok)
	}
}

func TestAddOrderBid(t *testing.T) {
	t.Parallel()

	book, err := NewBook(Bid)
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	orders := []struct{ price, qty int64 }{
		{99, 10}, {101, 5}, {100, 3}, {101, 2}, {98, 1},
	}
	for _, o := range orders {
		if err := book.AddOrder(o.price, o.qty); err != nil {
			t.Fatalf("AddOrder(%d, %d): %v", o.price, o.qty, err)
		}
	}

	want := []Level{{101, 7}, {100, 3}, {99, 10}, {98, 1}}
	if got := book.Levels(); !slices.Equal(got, want) {
		t.Fatalf("Levels() = %+v, want %+v", got, want)
	}
	if best, ok := book.BestPrice(); !ok || best != 101 {
		t.Fatalf("BestPrice() = (%d, %v), want (101, true)", best, ok)
	}
}

func TestBestPriceEmptyBook(t *testing.T) {
	t.Parallel()

	book, err := NewBook(Ask)
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	if _, ok := book.BestPrice(); ok {
		t.Fatal("BestPrice() on empty book: want ok=false")
	}
}

func TestDepthAtMissingPrice(t *testing.T) {
	t.Parallel()

	book, err := NewBook(Ask)
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	if err := book.AddOrder(100, 5); err != nil {
		t.Fatalf("AddOrder: %v", err)
	}
	if depth := book.DepthAt(999); depth != 0 {
		t.Fatalf("DepthAt(999) = %d, want 0", depth)
	}
}

func TestNewBookRejectsInvalidSide(t *testing.T) {
	t.Parallel()

	if _, err := NewBook(Side(99)); !errors.Is(err, ErrInvalidSide) {
		t.Fatalf("NewBook(99) error = %v, want ErrInvalidSide", err)
	}
}

func TestAddOrderRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	book, err := NewBook(Ask)
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	tests := []struct {
		name       string
		price, qty int64
		want       error
	}{
		{name: "zero price", price: 0, qty: 5, want: ErrInvalidPrice},
		{name: "negative price", price: -1, qty: 5, want: ErrInvalidPrice},
		{name: "zero qty", price: 100, qty: 0, want: ErrInvalidQty},
		{name: "negative qty", price: 100, qty: -5, want: ErrInvalidQty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := book.AddOrder(tc.price, tc.qty); !errors.Is(err, tc.want) {
				t.Fatalf("AddOrder(%d, %d) error = %v, want %v", tc.price, tc.qty, err, tc.want)
			}
		})
	}
}

func TestLevelsDoesNotAliasBook(t *testing.T) {
	t.Parallel()

	book, err := NewBook(Ask)
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	if err := book.AddOrder(100, 5); err != nil {
		t.Fatalf("AddOrder: %v", err)
	}

	levels := book.Levels()
	levels[0].Qty = 999
	if depth := book.DepthAt(100); depth != 5 {
		t.Fatalf("mutating Levels() changed the book: DepthAt(100) = %d, want 5", depth)
	}
}

// addOrderFragmenting is the antipattern this module contrasts, kept
// unexported and unreachable from the package API. It runs the exact same
// BinarySearchFunc call AddOrder does, but it ignores the found bit the
// search already computed and always inserts a new level, even when one
// already exists at price.
func addOrderFragmenting(levels []Level, price, qty int64, key func(int64) int64) []Level {
	pos, _ := slices.BinarySearchFunc(levels, price, func(l Level, p int64) int {
		return cmp.Compare(key(l.Price), key(p))
	})
	return slices.Insert(levels, pos, Level{Price: price, Qty: qty})
}

// TestIgnoringFoundFragmentsTheBook is the heart of the module: it pins the
// exact defect of discarding BinarySearchFunc's found bit -- two orders at
// the same price become two separate levels instead of one aggregated
// level, so a caller reading DepthAt off either individual level
// undercounts the resting quantity -- and shows the same two orders through
// Book.AddOrder producing a single, correctly aggregated level.
func TestIgnoringFoundFragmentsTheBook(t *testing.T) {
	t.Parallel()

	identity := func(p int64) int64 { return p }

	var fragmented []Level
	fragmented = addOrderFragmenting(fragmented, 100, 5, identity)
	fragmented = addOrderFragmenting(fragmented, 100, 3, identity)
	if len(fragmented) != 2 {
		t.Fatalf("len(fragmented) = %d, want 2 (one level per AddOrder call, not aggregated)", len(fragmented))
	}
	if fragmented[0].Qty == 8 {
		t.Fatalf("fragmented[0].Qty = %d, want an individual order's quantity, not the aggregate", fragmented[0].Qty)
	}

	book, err := NewBook(Ask)
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	if err := book.AddOrder(100, 5); err != nil {
		t.Fatalf("AddOrder: %v", err)
	}
	if err := book.AddOrder(100, 3); err != nil {
		t.Fatalf("AddOrder: %v", err)
	}
	if levels := book.Levels(); len(levels) != 1 {
		t.Fatalf("len(book.Levels()) = %d, want 1", len(levels))
	}
	if depth := book.DepthAt(100); depth != 8 {
		t.Fatalf("DepthAt(100) = %d, want 8", depth)
	}
}

// ExampleBook is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleBook() {
	bids, err := NewBook(Bid)
	if err != nil {
		panic(err)
	}
	for _, o := range []struct{ price, qty int64 }{
		{100, 10}, {101, 5}, {100, 3}, {99, 7},
	} {
		if err := bids.AddOrder(o.price, o.qty); err != nil {
			panic(err)
		}
	}

	best, _ := bids.BestPrice()
	fmt.Println("best bid:", best)
	fmt.Println("depth at 100:", bids.DepthAt(100))
	for _, l := range bids.Levels() {
		fmt.Printf("%d @ %d\n", l.Qty, l.Price)
	}

	// Output:
	// best bid: 101
	// depth at 100: 13
	// 5 @ 101
	// 13 @ 100
	// 7 @ 99
}
```

## Review

`Book` is correct when every order at a price already on the book joins that
level's aggregate quantity instead of opening a duplicate, and when
`BestPrice` and `DepthAt` reflect exactly that aggregate. The mechanism
worth internalizing is that `slices.BinarySearchFunc` already answers both
questions `AddOrder` needs -- whether a level exists (`found`) and where it
is or belongs (`pos`) -- in the single call every order runs through; the
mistake this module isolates is using only the `found` half and always
taking the insert branch, which fragments the book into duplicate levels at
the same price and makes `DepthAt` undercount whichever one of them the
search happens to land on. `NewBook` rejects an unrecognized `Side` with
`ErrInvalidSide`, and `AddOrder` rejects a non-positive price or quantity
with `ErrInvalidPrice` or `ErrInvalidQty`, both checkable with `errors.Is`.
`Book` is deliberately not safe for concurrent use, matching how real
matching engines process one book on one goroutine, and `Levels` never
aliases the book's internal storage. `ExampleBook` is the executable
documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — the `(pos, found)` search this module builds its insert-or-aggregate decision on.
- [`slices.Insert`](https://pkg.go.dev/slices#Insert) — opening a new level at its sorted position, and why the result must be reassigned.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the aliasing-free snapshot `Levels` returns.
- [Investopedia: Order Book](https://www.investopedia.com/terms/o/order-book.asp) — the bid/ask, price-level model this module implements one side of.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-sorted-wordlist-prefix-lookup.md](18-sorted-wordlist-prefix-lookup.md) | Next: [20-columnar-metrics-table-sort-interface.md](20-columnar-metrics-table-sort-interface.md)
