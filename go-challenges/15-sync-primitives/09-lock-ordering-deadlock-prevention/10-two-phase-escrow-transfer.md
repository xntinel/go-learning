# Exercise 10: Escrow Transfers — Eliminating Multi-Lock Sections Entirely

Every technique so far manages the danger of holding two locks. The strongest
move is to stop needing two: debit the source into an escrow ledger under one
lock, credit the destination under the other lock, compensate on failure. This
is how real payment systems transfer between accounts that live in different
services — and built in-process, its invariants can be tested exactly.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
escrowbank/                independent module: example.com/escrowbank
  go.mod
  escrow.go                type Account (mutex, cents, frozen flag); type Bank
                           (atomic.Int64 escrow ledger); Transfer (two-phase,
                           never holds two account locks), ConsistentTotal;
                           ErrInsufficientFunds, ErrAccountFrozen, ...
  cmd/
    demo/
      main.go              runnable demo: transfer, frozen-destination
                           compensation, escrow always drained
  escrow_test.go           error paths (no escrow leaks), injected phase-2
                           failure with exactly-once compensation, 50-goroutine
                           storm with an invariant-sampling goroutine, watchdog
```

- Files: `escrow.go`, `cmd/demo/main.go`, `escrow_test.go`.
- Implement: `Bank.Transfer` — phase 1 debits `from` and adds to the escrow counter under only `from`'s lock; phase 2 credits `to` and drains escrow under only `to`'s lock; a phase-2 failure (frozen destination) triggers a compensation credit back to `from`; `ConsistentTotal` locks all accounts in ID order and reads balances plus escrow as one exact snapshot.
- Test: happy path; insufficient funds and frozen source with zero escrow leak; frozen destination asserting compensation restored `from` exactly once; a 50-goroutine transfer storm with a concurrent sampler asserting accounts+escrow is conserved at every instant, under `-race` and a watchdog.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/10-two-phase-escrow-transfer/cmd/demo
cd go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/10-two-phase-escrow-transfer
```

### Trading a lock for an invariant window

The exercise-1 transfer holds both account locks so that "debit and credit
happen atomically" is literally true. The escrow design gives that up
deliberately. Phase 1: under `from`'s lock only, check funds, debit, and add
the amount to an escrow counter — the ledger entry meaning "this money is in
flight". Phase 2: under `to`'s lock only, credit and drain the escrow. Between
the phases, no lock is held at all.

What this buys: no goroutine ever holds two account locks, so the multi-hold
edges that every previous exercise managed simply do not exist. There is
nothing to order, no rank to assert, no TryLock to retry. And the shape
survives distribution — when `from` and `to` become rows in two different
services' databases, "lock both in ID order" stops being expressible, but
"debit + ledger entry, then credit, then compensate on failure" is exactly a
saga with the escrow counter playing the coordinator's journal. The in-process
version you build here is the honest miniature of that architecture.

What it costs: an invariant window. After phase 1 and before phase 2, the sum
of account balances alone is short by the in-flight amount. The *real*
invariant is `sum(balances) + escrow == constant`, and this module makes that
precise and testable through one careful rule: **every escrow mutation happens
while holding some account's lock** — the debit-side `escrow.Add(+amount)`
under `from`'s lock, the credit-side `Add(-amount)` under `to`'s lock, the
compensation under `from`'s lock. Consequently a reader that holds *all*
account locks (`ConsistentTotal`, which sorts by ID — exercise 2's technique
closing the loop) can observe no mutation mid-flight, so the conserved total
it reads is exact at every instant, even mid-storm. The concurrency test
exploits this with a sampler goroutine hammering `ConsistentTotal` while 50
workers transfer: any escrow leak, double-credit, or lost compensation
surfaces as a broken conserved sum, deterministically.

Phase 2 can fail — here, a frozen destination account (compliance hold), in a
distributed system a downed service. The response is *compensation*: credit
the escrowed amount back to `from`. Two properties matter. It must run
exactly once per failed transfer — compensating twice mints money, zero times
burns it — which in-process is guaranteed by control flow (one failure return
path) and asserted by the tests. And it must be allowed to land even if
`from` has meanwhile been frozen: a freeze blocks *new* activity, but
returning in-flight money is the completion of an old transaction, not new
activity — refusing it would strand funds in escrow forever. This asymmetry
(freezes reject new debits and new credits, but never compensations) is
standard ledger practice and is written into `creditCompensation`.

Money is `int64` cents throughout. The float64 balances of exercises 1 and 2
kept the focus on locking; a ledger whose invariant tests use exact equality
needs integer arithmetic — `0.1 + 0.2 != 0.3` is not a rounding style you
want in a conservation assertion.

Create `escrow.go`:

```go
// Package escrowbank transfers money without ever holding two account
// locks: phase 1 debits the source into an escrow ledger under the
// source's lock, phase 2 credits the destination under the destination's
// lock, and a phase-2 failure compensates the escrow back to the source.
//
// Invariant: sum(balances) + escrow is constant. Every escrow mutation
// happens under some account's lock, so ConsistentTotal — which holds
// all account locks — reads the invariant exactly, even mid-transfer.
package escrowbank

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

var (
	// ErrSameAccount rejects transfers from an account to itself.
	ErrSameAccount = errors.New("escrowbank: cannot transfer to same account")

	// ErrNonPositiveAmount rejects zero and negative amounts.
	ErrNonPositiveAmount = errors.New("escrowbank: non-positive amount")

	// ErrInsufficientFunds rejects debits larger than the source balance.
	ErrInsufficientFunds = errors.New("escrowbank: insufficient funds")

	// ErrAccountFrozen rejects new debits and credits on a frozen
	// account. Compensations are exempt: returning in-flight money
	// completes an old transaction and must always land.
	ErrAccountFrozen = errors.New("escrowbank: account frozen")
)

// Account is a ledger account. Balances are int64 cents: conservation
// tests use exact equality, which float64 cannot honestly provide.
type Account struct {
	ID      int
	mu      sync.Mutex
	balance int64
	frozen  bool
}

// NewAccount returns an account with the given id and balance in cents.
func NewAccount(id int, balanceCents int64) *Account {
	return &Account{ID: id, balance: balanceCents}
}

// BalanceOf returns the balance in cents, read under the lock.
func (a *Account) BalanceOf() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.balance
}

// Freeze places a compliance hold: new debits and credits fail until
// Unfreeze. In-flight compensations still land.
func (a *Account) Freeze() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.frozen = true
}

// Unfreeze lifts the hold.
func (a *Account) Unfreeze() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.frozen = false
}

// Bank carries the escrow ledger: the total cents currently in flight
// between a completed phase 1 and a completed phase 2.
type Bank struct {
	escrow atomic.Int64
}

// InEscrow returns the cents currently in flight.
func (b *Bank) InEscrow() int64 {
	return b.escrow.Load()
}

// Transfer moves amountCents from one account to another in two phases,
// never holding both account locks. On a phase-2 failure the escrowed
// amount is compensated back to the source exactly once and the returned
// error wraps the cause.
func (b *Bank) Transfer(from, to *Account, amountCents int64) error {
	if from == to {
		return ErrSameAccount
	}
	if amountCents <= 0 {
		return fmt.Errorf("%w: %d", ErrNonPositiveAmount, amountCents)
	}

	// Phase 1: debit into escrow, under from's lock ONLY. The escrow
	// add happens inside the lock so the conserved total is never
	// observably violated (see package doc).
	from.mu.Lock()
	if from.frozen {
		from.mu.Unlock()
		return fmt.Errorf("%w: account %d (source)", ErrAccountFrozen, from.ID)
	}
	if from.balance < amountCents {
		have := from.balance
		from.mu.Unlock()
		return fmt.Errorf("%w: have %d, need %d", ErrInsufficientFunds, have, amountCents)
	}
	from.balance -= amountCents
	b.escrow.Add(amountCents)
	from.mu.Unlock()

	// Between the phases: no locks held. sum(balances) is short by
	// amountCents; sum(balances)+escrow is intact.

	// Phase 2: credit out of escrow, under to's lock ONLY.
	if err := b.credit(to, amountCents); err != nil {
		// Compensation, exactly once: this is the only failure return
		// after phase 1, and it credits escrow back to the source.
		b.creditCompensation(from, amountCents)
		return fmt.Errorf("escrowbank: phase 2 failed, compensated: %w", err)
	}
	return nil
}

// credit lands amountCents on to, draining escrow, under to's lock.
func (b *Bank) credit(to *Account, amountCents int64) error {
	to.mu.Lock()
	defer to.mu.Unlock()
	if to.frozen {
		return fmt.Errorf("%w: account %d (destination)", ErrAccountFrozen, to.ID)
	}
	to.balance += amountCents
	b.escrow.Add(-amountCents)
	return nil
}

// creditCompensation returns escrowed money to the source. It ignores
// the frozen flag: a freeze blocks new activity, not the completion of
// an in-flight transaction — refusing would strand funds in escrow.
func (b *Bank) creditCompensation(from *Account, amountCents int64) {
	from.mu.Lock()
	defer from.mu.Unlock()
	from.balance += amountCents
	b.escrow.Add(-amountCents)
}

// ConsistentTotal returns sum(balances) + escrow as one exact snapshot.
// It acquires every account lock in ascending-ID order (transfers hold
// at most one lock and never block while holding it, so this cannot
// deadlock them) and reads the escrow counter while all locks are held,
// which excludes every escrow mutation.
func ConsistentTotal(b *Bank, accounts []*Account) int64 {
	sorted := slices.Clone(accounts)
	slices.SortFunc(sorted, func(x, y *Account) int {
		return cmp.Compare(x.ID, y.ID)
	})
	for _, a := range sorted {
		a.mu.Lock()
	}
	total := b.escrow.Load()
	for _, a := range sorted {
		total += a.balance
	}
	for _, a := range sorted {
		a.mu.Unlock()
	}
	return total
}
```

### The runnable demo

The demo shows the two behaviors that define the design: a normal transfer
drains escrow to zero, and a transfer into a frozen account compensates — the
money is back where it started, and nothing is stranded in flight.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/escrowbank"
)

func main() {
	var bank escrowbank.Bank
	alice := escrowbank.NewAccount(1, 10000) // cents
	bob := escrowbank.NewAccount(2, 5000)

	if err := bank.Transfer(alice, bob, 2500); err != nil {
		fmt.Println("err:", err)
		return
	}
	fmt.Printf("after transfer: alice=%d bob=%d escrow=%d\n",
		alice.BalanceOf(), bob.BalanceOf(), bank.InEscrow())

	bob.Freeze()
	if err := bank.Transfer(alice, bob, 1000); err != nil {
		fmt.Println("frozen destination:", err)
	}
	fmt.Printf("after compensation: alice=%d bob=%d escrow=%d\n",
		alice.BalanceOf(), bob.BalanceOf(), bank.InEscrow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after transfer: alice=7500 bob=7500 escrow=0
frozen destination: escrowbank: phase 2 failed, compensated: escrowbank: account frozen: account 2 (destination)
after compensation: alice=7500 bob=7500 escrow=0
```

### Tests

Every error-path test asserts the same three-part postcondition: correct
sentinel via `errors.Is`, balances untouched, escrow drained to zero — an
escrow leak is this design's signature bug, and it hides from any test that
only checks balances. The frozen-destination test additionally pins
exactly-once compensation by exact arithmetic. The storm test is where the
design proves itself: 50 goroutines transfer among 4 accounts while a sampler
goroutine calls `ConsistentTotal` continuously, asserting conservation *at
every instant*, not just at quiescence; the whole test runs under the
watchdog pattern from exercise 9 (a minimal copy is bundled — modules in this
lesson are self-contained), because the failure mode of a locking regression
here is a hang, not an assertion.

Create `escrow_test.go`:

```go
package escrowbank

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// runWithTimeout is a minimal copy of exercise 9's watchdog: fail with a
// full goroutine dump instead of hanging CI if the workload wedges.
func runWithTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("wedged after %v; all-goroutine dump:\n%s", d, buf[:n])
	}
}

func TestTransferHappyPath(t *testing.T) {
	t.Parallel()

	var bank Bank
	from := NewAccount(1, 10000)
	to := NewAccount(2, 0)

	if err := bank.Transfer(from, to, 2500); err != nil {
		t.Fatal(err)
	}
	if got := from.BalanceOf(); got != 7500 {
		t.Errorf("from = %d, want 7500", got)
	}
	if got := to.BalanceOf(); got != 2500 {
		t.Errorf("to = %d, want 2500", got)
	}
	if got := bank.InEscrow(); got != 0 {
		t.Errorf("escrow = %d after completed transfer, want 0", got)
	}
}

func TestTransferErrorsLeaveNoEscrow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		fromBalance  int64
		amount       int64
		same         bool
		freezeSource bool
		wantErr      error
	}{
		{name: "insufficient funds", fromBalance: 100, amount: 200, wantErr: ErrInsufficientFunds},
		{name: "zero amount", fromBalance: 100, amount: 0, wantErr: ErrNonPositiveAmount},
		{name: "negative amount", fromBalance: 100, amount: -5, wantErr: ErrNonPositiveAmount},
		{name: "same account", fromBalance: 100, amount: 10, same: true, wantErr: ErrSameAccount},
		{name: "frozen source", fromBalance: 100, amount: 10, freezeSource: true, wantErr: ErrAccountFrozen},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var bank Bank
			from := NewAccount(1, tc.fromBalance)
			to := NewAccount(2, 0)
			if tc.same {
				to = from
			}
			if tc.freezeSource {
				from.Freeze()
			}

			err := bank.Transfer(from, to, tc.amount)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := from.BalanceOf(); got != tc.fromBalance {
				t.Errorf("from = %d after failure, want %d", got, tc.fromBalance)
			}
			if got := bank.InEscrow(); got != 0 {
				t.Errorf("escrow leak: %d cents stranded", got)
			}
		})
	}
}

func TestPhase2FailureCompensatesExactlyOnce(t *testing.T) {
	t.Parallel()

	var bank Bank
	from := NewAccount(1, 10000)
	to := NewAccount(2, 500)
	to.Freeze() // phase 2 will fail

	err := bank.Transfer(from, to, 3000)
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("err = %v, want ErrAccountFrozen", err)
	}

	// Exactly-once compensation: from is restored to the cent — a lost
	// compensation leaves 7000, a doubled one leaves 13000.
	if got := from.BalanceOf(); got != 10000 {
		t.Fatalf("from = %d after compensation, want 10000", got)
	}
	if got := to.BalanceOf(); got != 500 {
		t.Errorf("frozen destination changed: %d, want 500", got)
	}
	if got := bank.InEscrow(); got != 0 {
		t.Errorf("escrow = %d after compensation, want 0", got)
	}
}

func TestCompensationLandsOnFrozenSource(t *testing.T) {
	t.Parallel()

	// The source freezes AFTER phase 1 debits it (a compliance hold
	// racing an in-flight transfer). Compensation must still land, or
	// the money is stranded in escrow forever.
	var bank Bank
	from := NewAccount(1, 1000)
	to := NewAccount(2, 0)
	to.Freeze()
	from.Freeze() // freeze both: phase 1 must fail cleanly...

	if err := bank.Transfer(from, to, 100); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("err = %v, want ErrAccountFrozen", err)
	}

	from.Unfreeze() // ...and with only the destination frozen,
	err := bank.Transfer(from, to, 100)
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("err = %v, want ErrAccountFrozen", err)
	}
	if got := from.BalanceOf(); got != 1000 {
		t.Errorf("from = %d, want 1000 (compensated)", got)
	}
	if got := bank.InEscrow(); got != 0 {
		t.Errorf("escrow = %d, want 0", got)
	}
}

func TestStormConservesAccountsPlusEscrow(t *testing.T) {
	t.Parallel()

	var bank Bank
	accounts := []*Account{
		NewAccount(1, 100000),
		NewAccount(2, 100000),
		NewAccount(3, 100000),
		NewAccount(4, 100000),
	}
	const initial = int64(4 * 100000)

	runWithTimeout(t, 60*time.Second, func() {
		stop := make(chan struct{})
		var samplerWg sync.WaitGroup
		samplerWg.Add(1)
		go func() {
			defer samplerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Conserved AT EVERY INSTANT: balances alone dip
				// mid-transfer, balances+escrow never does.
				if got := ConsistentTotal(&bank, accounts); got != initial {
					t.Errorf("ConsistentTotal mid-storm = %d, want %d", got, initial)
					return
				}
			}
		}()

		var wg sync.WaitGroup
		for i := range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range 40 {
					from := accounts[(i+j)%len(accounts)]
					to := accounts[(i+j+1)%len(accounts)]
					err := bank.Transfer(from, to, 7)
					if err != nil && !errors.Is(err, ErrInsufficientFunds) {
						t.Errorf("unexpected transfer error: %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()
		close(stop)
		samplerWg.Wait()
	})

	// At quiescence the stricter invariant holds too: escrow is empty
	// and balances alone are conserved.
	if got := bank.InEscrow(); got != 0 {
		t.Fatalf("escrow = %d at quiescence, want 0", got)
	}
	var sum int64
	for _, a := range accounts {
		sum += a.BalanceOf()
	}
	if sum != initial {
		t.Fatalf("balance sum = %d at quiescence, want %d", sum, initial)
	}
}

func ExampleBank_Transfer() {
	var bank Bank
	from := NewAccount(1, 100)
	to := NewAccount(2, 0)
	if err := bank.Transfer(from, to, 25); err != nil {
		fmt.Println("err:", err)
		return
	}
	fmt.Println(from.BalanceOf(), to.BalanceOf(), bank.InEscrow())
	// Output: 75 25 0
}
```

## Review

Check the lock discipline first, because it is the point: `Transfer` holds
`from.mu` in phase 1, *releases it*, then holds `to.mu` in phase 2 — at no
program point are two account locks held, so no ordering rule is needed and
none is written. Then check the ledger discipline: every `escrow.Add` sits
inside some account's critical section, which is what entitles
`ConsistentTotal` to call its snapshot exact; move an `Add` outside a lock
and the storm's sampler will catch the momentary non-conservation within a
few thousand iterations. Finally the compensation properties: exactly one
failure return exists after phase 1 and it compensates exactly once, and
compensation ignores freezes because stranding escrowed funds is worse than
crediting a frozen account with its own money back.

Know what the miniature does *not* give you for free in the distributed
version: there, phase boundaries are network calls, "exactly once" becomes
idempotency keys plus retries, the escrow counter becomes a persisted outbox
or saga log, and a crash between phases needs recovery that scans the log and
re-drives phase 2 or the compensation. The state machine you built — debit,
in-flight, credited or compensated — is the same one; the durability of each
arrow is what changes. Confirm with `go test -count=1 -race ./...`.

## Resources

- [sync/atomic — Int64](https://pkg.go.dev/sync/atomic#Int64) — the escrow counter's type.
- [Sagas (microservices.io)](https://microservices.io/patterns/data/saga.html) — the distributed pattern this transfer miniaturizes, including compensating transactions.
- [Pattern: Transactional outbox (microservices.io)](https://microservices.io/patterns/data/transactional-outbox.html) — how the in-flight ledger persists across process boundaries.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../10-mutex-vs-channel/00-concepts.md](../10-mutex-vs-channel/00-concepts.md)
