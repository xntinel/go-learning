# Exercise 7: An Account Aggregate Enforcing an Invariant Across Entries

Some invariants span a whole collection, not a single object: "the running balance
implied by this ledger never goes negative" is a rule about *all* the entries
together. This module builds an `Account` aggregate root that holds a private
slice of immutable `LedgerEntry` value objects, enforces the cross-entry invariant
in `Post`, and hands out copies from its accessor so callers cannot reach in and
mutate its state.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ledger/                     independent module: example.com/ledger
  go.mod                    go 1.26
  ledger.go                 type LedgerEntry (value object); type Account (aggregate root)
  cmd/
    demo/
      main.go               runnable demo: post credits/debits, reject overdraw
  ledger_test.go            tests: overdraw rejected, balance == sum, Entries() is a copy, append-only
```

- Files: `ledger.go`, `cmd/demo/main.go`, `ledger_test.go`.
- Implement: an immutable `LedgerEntry` with a kind and amount; an `Account` with a private `[]LedgerEntry`, `Post(entry)` enforcing a non-negative running balance and append-only ordering, `Balance()`, and `Entries()` returning a copy.
- Test: posting a debit beyond the balance is rejected and leaves entries unchanged; `Balance()` equals the sum of entries; `Entries()` returns a copy (mutating it does not affect the aggregate); append order is preserved.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ledger/cmd/demo
cd ~/go-exercises/ledger
go mod init example.com/ledger
```

### The root is the only door, and it owns the cross-entry rule

An aggregate root is the single entry point to a cluster of objects. Here the
`Account` owns a private slice of `LedgerEntry` values; nothing outside package
`ledger` can append to it, reorder it, or mutate an entry, because the slice is
unexported and the entries themselves are immutable value objects. The only way to
add an entry is `Post`, and `Post` is where the invariant that spans all entries
lives: it computes what the running balance *would* become if this entry were
applied, and if that is negative it rejects the entry with
`ErrInsufficientFunds` and appends nothing. The ledger is append-only — there is
no `Remove` or in-place edit — so the history is immutable and the balance is
always exactly the sum of what was posted.

`LedgerEntry` is a value object: a `kind` (credit or debit) and a positive
`amount`, both comparable, so two identical entries are equal and the type is safe
to store and copy freely. Its constructor rejects a non-positive amount, so an
entry that exists is always well-formed; the *sign* of its effect on the balance
comes from the kind, not from a signed amount, which keeps "how much" and "which
direction" as separate, individually-valid facts.

The accessor is where aggregates are most often breached. `Entries()` must not
return the internal slice, because a returned slice header shares its backing
array with the aggregate — a caller who appended to it or assigned to an element
would silently corrupt the ledger from outside, past every check `Post` performs.
The fix is `slices.Clone`: `Entries()` returns a fresh copy, so the caller can
read and even mutate the returned slice with no effect on the aggregate's private
state. That is the boundary holding. `Account` uses pointer receivers because
`Post` mutates it in place and because an aggregate is a stateful entity, not a
value to be copied around.

Create `ledger.go`:

```go
package ledger

import (
	"errors"
	"fmt"
	"slices"
)

var (
	ErrEmptyID           = errors.New("ledger: account id is required")
	ErrNonPositive       = errors.New("ledger: entry amount must be positive")
	ErrInsufficientFunds = errors.New("ledger: entry would drive balance negative")
)

// EntryKind is the direction of a ledger entry.
type EntryKind int

const (
	Credit EntryKind = iota // increases the balance
	Debit                   // decreases the balance
)

// LedgerEntry is an immutable value object: a direction plus a positive amount.
type LedgerEntry struct {
	kind   EntryKind
	amount int64
	memo   string
}

// NewLedgerEntry builds an entry, rejecting a non-positive amount.
func NewLedgerEntry(kind EntryKind, amount int64, memo string) (LedgerEntry, error) {
	if amount <= 0 {
		return LedgerEntry{}, fmt.Errorf("%w: got %d", ErrNonPositive, amount)
	}
	return LedgerEntry{kind: kind, amount: amount, memo: memo}, nil
}

// signed returns the entry's effect on the running balance.
func (e LedgerEntry) signed() int64 {
	if e.kind == Debit {
		return -e.amount
	}
	return e.amount
}

// Account is an aggregate root: it owns a private, append-only slice of entries
// and enforces the invariant that the running balance never goes negative.
type Account struct {
	id      string
	entries []LedgerEntry
}

// NewAccount builds an empty account.
func NewAccount(id string) (*Account, error) {
	if id == "" {
		return nil, ErrEmptyID
	}
	return &Account{id: id}, nil
}

// Post appends an entry if it keeps the running balance non-negative. It is the
// only way to modify the ledger, and it enforces the cross-entry invariant.
func (a *Account) Post(e LedgerEntry) error {
	if a.Balance()+e.signed() < 0 {
		return fmt.Errorf("%w: balance=%d entry=%d", ErrInsufficientFunds, a.Balance(), e.signed())
	}
	a.entries = append(a.entries, e)
	return nil
}

// Balance is the sum of all posted entries.
func (a *Account) Balance() int64 {
	var sum int64
	for _, e := range a.entries {
		sum += e.signed()
	}
	return sum
}

// Entries returns a COPY of the ledger, so callers cannot mutate private state.
func (a *Account) Entries() []LedgerEntry {
	return slices.Clone(a.entries)
}

// Len reports the number of posted entries.
func (a *Account) Len() int { return len(a.entries) }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/ledger"
)

func main() {
	acct, _ := ledger.NewAccount("acct-1")

	credit, _ := ledger.NewLedgerEntry(ledger.Credit, 10000, "opening deposit")
	_ = acct.Post(credit)

	debit, _ := ledger.NewLedgerEntry(ledger.Debit, 3000, "rent")
	_ = acct.Post(debit)

	fmt.Printf("balance: %d over %d entries\n", acct.Balance(), acct.Len())

	overdraw, _ := ledger.NewLedgerEntry(ledger.Debit, 100000, "car")
	if err := acct.Post(overdraw); errors.Is(err, ledger.ErrInsufficientFunds) {
		fmt.Println("overdraw rejected; ledger unchanged")
	}
	fmt.Printf("final balance: %d over %d entries\n", acct.Balance(), acct.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
balance: 7000 over 2 entries
overdraw rejected; ledger unchanged
final balance: 7000 over 2 entries
```

### Tests

The tests pin the aggregate boundary. The overdraw test posts a debit past the
balance and asserts both the error and that the entry count did not change. The
sum test asserts `Balance()` equals the arithmetic sum of what was posted. The
copy test is the key one: it takes the slice from `Entries()`, mutates an element
and appends to it, then asserts the aggregate's own balance and length are
untouched — proof the accessor handed back a copy, not a window into private
state. The order test asserts entries come back in post order.

Create `ledger_test.go`:

```go
package ledger

import (
	"errors"
	"testing"
)

func mustEntry(t *testing.T, kind EntryKind, amount int64) LedgerEntry {
	t.Helper()
	e, err := NewLedgerEntry(kind, amount, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPostRejectsOverdraw(t *testing.T) {
	t.Parallel()
	a, _ := NewAccount("acct-1")
	if err := a.Post(mustEntry(t, Credit, 5000)); err != nil {
		t.Fatal(err)
	}
	if err := a.Post(mustEntry(t, Debit, 9000)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("Post err = %v, want ErrInsufficientFunds", err)
	}
	if a.Len() != 1 {
		t.Fatalf("rejected entry was appended: Len = %d, want 1", a.Len())
	}
}

func TestBalanceEqualsSum(t *testing.T) {
	t.Parallel()
	a, _ := NewAccount("acct-1")
	_ = a.Post(mustEntry(t, Credit, 10000))
	_ = a.Post(mustEntry(t, Debit, 2500))
	_ = a.Post(mustEntry(t, Credit, 500))
	if got := a.Balance(); got != 8000 {
		t.Fatalf("Balance = %d, want 8000", got)
	}
}

func TestEntriesReturnsCopy(t *testing.T) {
	t.Parallel()
	a, _ := NewAccount("acct-1")
	_ = a.Post(mustEntry(t, Credit, 10000))

	got := a.Entries()
	got[0] = mustEntry(t, Debit, 999) // mutate the returned slice
	got = append(got, mustEntry(t, Credit, 1))

	if a.Balance() != 10000 {
		t.Fatalf("aggregate mutated through Entries(): balance = %d, want 10000", a.Balance())
	}
	if a.Len() != 1 {
		t.Fatalf("aggregate length changed through Entries(): Len = %d, want 1", a.Len())
	}
}

func TestAppendOnlyOrder(t *testing.T) {
	t.Parallel()
	a, _ := NewAccount("acct-1")
	_ = a.Post(mustEntry(t, Credit, 100))
	_ = a.Post(mustEntry(t, Credit, 200))
	_ = a.Post(mustEntry(t, Credit, 300))
	got := a.Entries()
	want := []int64{100, 200, 300}
	for i, w := range want {
		if got[i].signed() != w {
			t.Fatalf("entry[%d] = %d, want %d (order not preserved)", i, got[i].signed(), w)
		}
	}
}
```

## Review

The aggregate is correct when `Post` is the only mutator, it rejects any entry
that would break the cross-entry invariant, and `Entries()` returns a copy. The
copy test is the one that catches the classic bug — returning the internal slice —
because a shared backing array lets a caller corrupt the ledger past every check.
The mistakes to avoid: returning `a.entries` directly (breach), and putting the
balance rule anywhere but `Post` (which would let some other path append an
overdrawing entry). Pointer receivers are correct here because the aggregate is a
stateful entity that `Post` mutates in place.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — returning a copy from an accessor.
- [Domain-Driven Design (Evans): Aggregates](https://domainlanguage.com/ddd/) — the aggregate-root pattern and invariant ownership.
- [Go Specification: Appending to and copying slices](https://go.dev/ref/spec#Appending_and_copying_slices) — why a returned slice shares its backing array.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-domain-dto-json-boundary.md](06-domain-dto-json-boundary.md) | Next: [08-order-state-machine-transitions.md](08-order-state-machine-transitions.md)
