# Exercise 8: Nested Locking and Deadlock

A deadlock is two or more goroutines each holding a lock the other needs, a
circular wait in which no one can move and nothing crashes -- the program just
freezes, and Go's runtime only notices when literally every goroutine is
blocked. The canonical trigger is a bank transfer: two accounts, each with its
own mutex, and two concurrent transfers that lock them in opposite order --
transfer(A,B) takes A then B while transfer(B,A) takes B then A. This exercise
reproduces that deadlock, dissects the circular wait, and fixes it with the one
discipline that provably prevents it: a consistent global lock order.

## What you'll build

```text
08-nested-locking-deadlock/
  main.go        the opposite-order transfer that freezes, the circular-wait
                 analysis, the lower-ID-first fix, and a multi-account stress test
```

- Build: a bank-transfer engine that deadlocks under inconsistent lock order, then the fix that never does.
- Implement: `transferUnsafe` (parameter-order locking, deadlocks), `transferSafe`/`lockInOrder` locking by ascending account ID, and a multi-account stress test that conserves money.
- Verify: `go run -race main.go` on the safe versions -- no deadlock, no data race, money conserved.

### Why nested-lock deadlocks are so dangerous

Deadlocks from nested locking are particularly dangerous because:
- They may not manifest during testing if the timing is just right
- They require specific interleaving of goroutine execution to trigger
- They are invisible at compile time
- Once triggered in production, the affected goroutines hang forever while the rest of the server continues, creating a partial freeze that is extremely hard to diagnose

The four conditions for deadlock (Coffman conditions):
1. **Mutual exclusion**: at least one resource is non-shareable (mutex)
2. **Hold and wait**: a goroutine holds one lock while waiting for another
3. **No preemption**: locks can only be released voluntarily
4. **Circular wait**: goroutine A waits for B's lock, B waits for A's lock

Breaking any one condition prevents deadlock. The most practical approach in Go is to break the **circular wait** by establishing a **consistent lock ordering**: always acquire locks in the same order, regardless of which goroutine is executing.

## Step 1 -- The Deadlock: Bank Transfer Gone Wrong

Two concurrent transfers lock accounts in opposite order, creating a classic deadlock:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	initialBalance   = 1000
	lockAcquireDelay = 50 * time.Millisecond
)

type Account struct {
	ID      int
	Name    string
	mu      sync.Mutex
	balance int
}

func NewAccount(id int, name string, balance int) *Account {
	return &Account{ID: id, Name: name, balance: balance}
}

func transferUnsafe(from, to *Account, amount int) {
	from.mu.Lock()
	fmt.Printf("  Transfer %s->%s: locked %s\n", from.Name, to.Name, from.Name)
	time.Sleep(lockAcquireDelay) // give the other transfer time to lock
	fmt.Printf("  Transfer %s->%s: waiting for %s...\n", from.Name, to.Name, to.Name)
	to.mu.Lock() // BLOCKED if the other transfer holds this lock

	if from.balance >= amount {
		from.balance -= amount
		to.balance += amount
	}

	to.mu.Unlock()
	from.mu.Unlock()
}

func demonstrateDeadlock() {
	alice := NewAccount(1, "Alice", initialBalance)
	bob := NewAccount(2, "Bob", initialBalance)

	fmt.Println("=== DEADLOCK DEMO (will freeze) ===")
	fmt.Println("Two transfers locking in opposite order:")
	fmt.Println("  Transfer 1: Alice -> Bob (locks Alice first)")
	fmt.Println("  Transfer 2: Bob -> Alice (locks Bob first)")
	fmt.Println()

	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		transferUnsafe(alice, bob, 100) // locks Alice, then Bob
	}()
	go func() {
		defer wg.Done()
		transferUnsafe(bob, alice, 50) // locks Bob, then Alice -- OPPOSITE ORDER
	}()

	wg.Wait() // never returns -- both goroutines are blocked
	fmt.Println("This line is never reached")
}

func main() {
	demonstrateDeadlock()
}
```

Expected output (then freeze):
```
=== DEADLOCK DEMO (will freeze) ===
Two transfers locking in opposite order:
  Transfer 1: Alice -> Bob (locks Alice first)
  Transfer 2: Bob -> Alice (locks Bob first)

  Transfer Alice->Bob: locked Alice
  Transfer Bob->Alice: locked Bob
  Transfer Alice->Bob: waiting for Bob...
  Transfer Bob->Alice: waiting for Alice...
fatal error: all goroutines are asleep - deadlock!
```

**WARNING:** This program will hang. Press Ctrl+C to kill it. Go's runtime detects the deadlock only because ALL goroutines are blocked.

### Verification
Run it and observe the freeze. The two transfers each hold one lock and wait for the other.

## Step 2 -- Analyze the Circular Wait

```
Timeline:
  T0: Transfer 1 locks Alice,  Transfer 2 locks Bob        (both succeed)
  T1: Transfer 1 wants Bob     (held by Transfer 2)       -- BLOCKED
      Transfer 2 wants Alice   (held by Transfer 1)       -- BLOCKED

  Transfer 1 --> waits for Bob   --> held by Transfer 2
  Transfer 2 --> waits for Alice --> held by Transfer 1

  Circular dependency. Neither can proceed.
```

The root cause: Transfer 1 acquires locks in order `Alice, Bob` while Transfer 2 acquires them in order `Bob, Alice`. This inconsistency creates the possibility of circular wait.

In production, this deadlock is far worse than it looks. If your server has a listening goroutine (which it always does), Go's runtime deadlock detector does NOT trigger. The transfers hang silently while the server appears to be running. Users see timeouts. No error is logged. The only symptom is that transfer-related requests stop completing.

## Step 3 -- Fix with Consistent Lock Ordering

Always lock the account with the lower ID first, regardless of which is "from" and which is "to":

```go
package main

import (
	"fmt"
	"sync"
)

const (
	initialBalance       = 1000
	transfersPerDir      = 100
	transferAmount       = 10
)

type Account struct {
	ID      int
	Name    string
	mu      sync.Mutex
	balance int
}

func NewAccount(id int, name string, balance int) *Account {
	return &Account{ID: id, Name: name, balance: balance}
}

// lockInOrder acquires locks by ascending account ID to prevent deadlocks.
func lockInOrder(a, b *Account) (*Account, *Account) {
	if a.ID > b.ID {
		return b, a
	}
	return a, b
}

func transferSafe(from, to *Account, amount int) bool {
	first, second := lockInOrder(from, to)

	first.mu.Lock()
	defer first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()

	if from.balance < amount {
		return false
	}
	from.balance -= amount
	to.balance += amount
	return true
}

func runBidirectionalTransfers(alice, bob *Account, rounds, amount int) {
	var wg sync.WaitGroup

	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			transferSafe(alice, bob, amount)
		}()
		go func() {
			defer wg.Done()
			transferSafe(bob, alice, amount)
		}()
	}

	wg.Wait()
}

func main() {
	alice := NewAccount(1, "Alice", initialBalance)
	bob := NewAccount(2, "Bob", initialBalance)

	fmt.Println("=== SAFE TRANSFERS (consistent lock ordering) ===")
	fmt.Printf("Before: Alice=%d, Bob=%d\n\n", alice.balance, bob.balance)

	runBidirectionalTransfers(alice, bob, transfersPerDir, transferAmount)

	totalMoney := alice.balance + bob.balance
	fmt.Printf("After:  Alice=%d, Bob=%d\n", alice.balance, bob.balance)
	fmt.Printf("Total:  %d (must be %d -- money is conserved)\n", totalMoney, initialBalance*2)
	fmt.Println("\nNo deadlock. Consistent lock ordering breaks the circular wait.")
}
```

Expected output:
```
=== SAFE TRANSFERS (consistent lock ordering) ===
Before: Alice=1000, Bob=1000

After:  Alice=1000, Bob=1000
Total:  2000 (must be 2000 -- money is conserved)

No deadlock. Consistent lock ordering breaks the circular wait.
```

### Verification
```bash
go run -race main.go
```
No deadlocks, no data races. Total money is conserved. Run it multiple times -- it never hangs.

## Step 4 -- Multi-Account Transfers at Scale

A realistic banking system has many accounts. The same principle applies -- always lock by ascending ID:

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
)

const (
	numAccounts      = 5
	initialBalance   = 10000
	numTransfers     = 10000
	maxTransferAmount = 100
)

type Account struct {
	ID      int
	mu      sync.Mutex
	balance int
}

func NewAccount(id, balance int) *Account {
	return &Account{ID: id, balance: balance}
}

func transfer(from, to *Account, amount int) bool {
	first, second := from, to
	if from.ID > to.ID {
		first, second = to, from
	}

	first.mu.Lock()
	defer first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()

	if from.balance < amount {
		return false
	}
	from.balance -= amount
	to.balance += amount
	return true
}

func createAccounts(count, balance int) []*Account {
	accounts := make([]*Account, count)
	for i := range accounts {
		accounts[i] = NewAccount(i+1, balance)
	}
	return accounts
}

func pickRandomPair(count int) (int, int) {
	fromIdx := rand.IntN(count)
	toIdx := rand.IntN(count)
	for toIdx == fromIdx {
		toIdx = rand.IntN(count)
	}
	return fromIdx, toIdx
}

func runStressTest(accounts []*Account, transferCount int) int64 {
	var wg sync.WaitGroup
	var successCount atomic.Int64

	for i := 0; i < transferCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fromIdx, toIdx := pickRandomPair(len(accounts))
			amount := rand.IntN(maxTransferAmount) + 1
			if transfer(accounts[fromIdx], accounts[toIdx], amount) {
				successCount.Add(1)
			}
		}()
	}

	wg.Wait()
	return successCount.Load()
}

func totalBalance(accounts []*Account) int {
	total := 0
	for _, acct := range accounts {
		total += acct.balance
	}
	return total
}

func printStressTestResults(accounts []*Account, totalBefore int, succeeded int64) {
	fmt.Printf("\nTransfers attempted: %d\n", numTransfers)
	fmt.Printf("Transfers succeeded: %d\n", succeeded)
	fmt.Println("\nFinal balances:")
	for _, acct := range accounts {
		fmt.Printf("  Account %d: $%d\n", acct.ID, acct.balance)
	}
	totalAfter := totalBalance(accounts)
	fmt.Printf("\nTotal before: $%d\n", totalBefore)
	fmt.Printf("Total after:  $%d\n", totalAfter)
	fmt.Printf("Money conserved: %v\n", totalBefore == totalAfter)
}

func main() {
	accounts := createAccounts(numAccounts, initialBalance)
	totalBefore := totalBalance(accounts)

	fmt.Println("=== Multi-Account Transfer Stress Test ===")
	fmt.Printf("Accounts: %d, each starting with $%d\n", numAccounts, initialBalance)

	succeeded := runStressTest(accounts, numTransfers)
	printStressTestResults(accounts, totalBefore, succeeded)
}
```

Expected output:
```
=== Multi-Account Transfer Stress Test ===
Accounts: 5, each starting with $10,000

Transfers attempted: 10000
Transfers succeeded: 8547

Final balances:
  Account 1: $11234
  Account 2: $8765
  Account 3: $12098
  Account 4: $7654
  Account 5: $10249

Total before: $50000
Total after:  $50000
Money conserved: true
```

### Verification
```bash
go run -race main.go
```
No deadlocks, no data races. Total money across all accounts is always conserved at $50,000.

## Common Mistakes

### Locking Based on Parameter Order

```go
func transfer(from, to *Account, amount int) {
    from.mu.Lock()   // depends on which account is "from"
    to.mu.Lock()     // different callers may reverse this
    // ...
}
```

**What happens:** `transfer(A, B, 100)` and `transfer(B, A, 50)` running concurrently create a deadlock.

**Fix:** Lock based on a stable ordering (ID, address, name), not the parameter position.

### Assuming the Runtime Always Detects Deadlocks
Go's deadlock detector only triggers when ALL goroutines are blocked. In a real server with a listening goroutine, HTTP handler goroutine, or ticker, deadlocks between other goroutines go completely undetected. The program hangs partially: some requests complete, others never do. This is harder to diagnose than a full crash.

### Lock Escalation
Acquiring more locks while already holding one is inherently risky. Minimize nested locking. If you must hold two locks, document the lock ordering invariant clearly:

```go
// lockOrdering documents the required lock acquisition order.
// Always lock the lower-ID account first to prevent deadlocks.
// Invariant: for any pair (A, B) where A.ID < B.ID, lock A before B.
```

### Trying to Detect Deadlocks with Timeouts

```go
// Tempting but fragile
select {
case <-time.After(5 * time.Second):
    log.Fatal("possible deadlock")
}
```

This hides the real problem. Fix the lock ordering instead. Timeouts for deadlock detection are a band-aid that makes debugging harder.

## Review

A deadlock forms when goroutines close a cycle in the wait-for graph: each holds
one lock and waits for another that a peer is holding. The textbook cause is
exactly the two-account transfer -- per-resource mutexes acquired in whatever
order the arguments arrived, so `transfer(A,B)` and `transfer(B,A)` can each grab
one lock and block on the other. The fix breaks the circular-wait condition by
making acquisition order a property of the resource rather than the caller:
always lock the lower-ID account first. Now every goroutine that needs both locks
takes them in the same order, every wait-for edge points from a lower ID to a
higher one, the graph is acyclic, and no cycle -- no deadlock -- can form. The
ordering key has to be stable and total (an ID or address), never the parameter
position the caller controls.

What makes these bugs vicious is their silence. Go's runtime deadlock detector
fires only when every goroutine is asleep, so in a real server -- where a
listener or a ticker is always awake -- a deadlock between two request goroutines
hangs those requests forever while health checks stay green and nothing is
logged. That is why the discipline, not a timeout band-aid, is the fix, and why
you document the ordering invariant for any code holding two locks. Prove you
have it by building the dining philosophers: five philosophers, five fork
mutexes, first the version where everyone grabs their left fork then their right
and the table deadlocks, then the version where everyone takes the lower-numbered
fork first -- run it a thousand times and watch it never hang.

## Resources
- [Go Runtime Deadlock Detection](https://pkg.go.dev/runtime#hdr-Detecting_Deadlocks) -- what the runtime detector catches, and the "all goroutines asleep" limit that lets partial deadlocks through.
- [Coffman Conditions (Wikipedia)](https://en.wikipedia.org/wiki/Deadlock#Necessary_conditions) -- the four conditions for deadlock and why breaking any one prevents it.
- [Go FAQ: Goroutines and Threads](https://go.dev/doc/faq#goroutines) -- how goroutines map onto threads, background for why a live listener defeats the detector.

---

Back to [Concurrency](../../concurrency.md) | Next: [09-sync-map-concurrent-access](../09-sync-map-concurrent-access/09-sync-map-concurrent-access.md)
