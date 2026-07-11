# Exercise 3: Atomic Compare-And-Swap

A flash sale puts 500 tickets live and 10,000 buyers hit Buy at once. Every
purchase has to check whether tickets remain and decrement the count as one
indivisible step -- otherwise two requests both see "1 left," both decrement,
and you have oversold. `atomic.AddInt64` cannot help, because it modifies
without first checking a condition. Compare-and-swap is the primitive that does
check-then-act atomically, and this exercise uses it to build a lock-free
ticket booth, watch the overselling bug appear without it, and weigh optimistic
CAS against a pessimistic mutex.

## What you'll build

```text
03-atomic-compare-and-swap/
  main.go        lock-free ticket/seat reservation with a CAS retry loop, plus
                 a CAS-vs-Mutex benchmark (each step is its own runnable program)
```

- Build: a lock-free ticket/seat reservation system with a CAS retry loop, plus a CAS-vs-Mutex benchmark.
- Implement: `TryReserve` with a `CompareAndSwapInt64` retry loop, a multi-section `ConcertVenue`, and `benchmarkCAS`/`benchmarkMutex`.
- Verify: `go run -race main.go` (Step 1 shows a DATA RACE; Steps 2-4 are race-free).

### Why check-then-act must be one instruction

`CompareAndSwapInt64(&addr, old, new)` atomically checks if `*addr == old`, and if so, sets `*addr = new` and returns `true`. If `*addr != old`, it does nothing and returns `false`. The entire operation is one indivisible CPU instruction.

The CAS retry loop is the core pattern of optimistic concurrency: load the current value, compute the desired new value, attempt the swap. If another goroutine changed the value between your load and your swap, CAS fails and you retry. No locks, no blocking, no goroutine parking. Under low contention, most CAS attempts succeed on the first try. Under high contention, retries increase but the system never deadlocks.

## Step 1 -- The Overselling Bug Without CAS

Multiple goroutines try to buy tickets using a naive check-then-decrement. The check and the decrement are separate operations, creating a race window:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	initialTickets = 100
	buyerCount     = 500
)

type UnsafeTicketBooth struct {
	tickets int64
	sold    int64
}

func NewUnsafeTicketBooth(stock int64) *UnsafeTicketBooth {
	return &UnsafeTicketBooth{tickets: stock}
}

func (tb *UnsafeTicketBooth) TryBuy() {
	// BUG: check and decrement are separate operations
	if tb.tickets > 0 {
		tb.tickets--
		atomic.AddInt64(&tb.sold, 1)
	}
}

func (tb *UnsafeTicketBooth) Report() {
	var oversold int64
	if tb.tickets < 0 {
		oversold = -tb.tickets
	}
	fmt.Println("=== Ticket Sales (BROKEN - no CAS) ===")
	fmt.Printf("Starting tickets: %d\n", initialTickets)
	fmt.Printf("Sold:             %d\n", tb.sold)
	fmt.Printf("Remaining:        %d\n", tb.tickets)
	fmt.Printf("Oversold:         %d\n", oversold)
}

func simulateBuyers(booth *UnsafeTicketBooth) {
	var wg sync.WaitGroup
	for i := 0; i < buyerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			booth.TryBuy()
		}()
	}
	wg.Wait()
}

func main() {
	booth := NewUnsafeTicketBooth(initialTickets)
	simulateBuyers(booth)
	booth.Report()
}
```

### Verification
```bash
go run -race main.go
```
The race detector reports `DATA RACE`. Run without `-race` multiple times: the ticket count may go negative, meaning you sold tickets that do not exist.

## Step 2 -- Fix with CAS: Lock-Free Reservation

Use `CompareAndSwapInt64` to atomically check-and-decrement. Each buyer loads the current stock, checks if positive, and attempts a CAS to decrement. If the CAS fails (another buyer got there first), retry:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	initialTickets = 100
	buyerCount     = 500
)

type TicketBooth struct {
	stock        int64
	successCount atomic.Int64
	failCount    atomic.Int64
}

func NewTicketBooth(stock int64) *TicketBooth {
	return &TicketBooth{stock: stock}
}

func (tb *TicketBooth) TryReserve(quantity int64) bool {
	for {
		current := atomic.LoadInt64(&tb.stock)
		if current < quantity {
			return false
		}
		if atomic.CompareAndSwapInt64(&tb.stock, current, current-quantity) {
			return true
		}
		// CAS failed: another goroutine modified stock. Retry.
	}
}

func (tb *TicketBooth) ProcessBuyer() {
	if tb.TryReserve(1) {
		tb.successCount.Add(1)
	} else {
		tb.failCount.Add(1)
	}
}

func (tb *TicketBooth) RemainingStock() int64 {
	return atomic.LoadInt64(&tb.stock)
}

func (tb *TicketBooth) Report() {
	fmt.Println("=== Ticket Sales (FIXED - CAS) ===")
	fmt.Printf("Starting tickets:     %d\n", initialTickets)
	fmt.Printf("Successful purchases: %d\n", tb.successCount.Load())
	fmt.Printf("Rejected (sold out):  %d\n", tb.failCount.Load())
	fmt.Printf("Remaining stock:      %d\n", tb.RemainingStock())
}

func simulateBuyers(booth *TicketBooth) {
	var wg sync.WaitGroup
	for i := 0; i < buyerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			booth.ProcessBuyer()
		}()
	}
	wg.Wait()
}

func main() {
	booth := NewTicketBooth(initialTickets)
	simulateBuyers(booth)
	booth.Report()
}
```

### Verification
```bash
go run -race main.go
```
Exactly 100 purchases succeed, 400 are rejected, remaining stock is 0. No overselling. No race warnings.

## Step 3 -- Full Seat Reservation System with Multiple Event Sections

Build a realistic event reservation system with multiple sections, each managed by a CAS-based counter. Buyers can request specific sections and quantities:

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
)

const (
	buyerCount     = 2000
	maxSeatsPerBuy = 4
)

type EventSection struct {
	Name     string
	stock    int64
	reserved atomic.Int64
	rejected atomic.Int64
}

func NewEventSection(name string, capacity int64) *EventSection {
	return &EventSection{Name: name, stock: capacity}
}

func (s *EventSection) Reserve(quantity int64) bool {
	for {
		current := atomic.LoadInt64(&s.stock)
		if current < quantity {
			s.rejected.Add(1)
			return false
		}
		if atomic.CompareAndSwapInt64(&s.stock, current, current-quantity) {
			s.reserved.Add(1)
			return true
		}
	}
}

func (s *EventSection) Available() int64 {
	return atomic.LoadInt64(&s.stock)
}

type ConcertVenue struct {
	sections      []*EventSection
	totalReserved atomic.Int64
	totalRejected atomic.Int64
}

func NewConcertVenue() *ConcertVenue {
	return &ConcertVenue{
		sections: []*EventSection{
			NewEventSection("VIP Front Row", 20),
			NewEventSection("Orchestra", 200),
			NewEventSection("Balcony", 500),
		},
	}
}

func (v *ConcertVenue) ProcessBuyer() {
	section := v.sections[rand.IntN(len(v.sections))]
	quantity := int64(1 + rand.IntN(maxSeatsPerBuy))

	if section.Reserve(quantity) {
		v.totalReserved.Add(1)
	} else {
		v.totalRejected.Add(1)
	}
}

func (v *ConcertVenue) Report() {
	fmt.Println("=== Concert Seat Reservation Results ===")
	fmt.Println()
	for _, s := range v.sections {
		fmt.Printf("  %-15s  remaining: %3d  reservations: %3d  rejected: %3d\n",
			s.Name, s.Available(), s.reserved.Load(), s.rejected.Load())
	}
	fmt.Println()
	fmt.Printf("Total reservations: %d\n", v.totalReserved.Load())
	fmt.Printf("Total rejections:   %d\n", v.totalRejected.Load())
}

func (v *ConcertVenue) VerifyIntegrity() {
	for _, s := range v.sections {
		if s.Available() < 0 {
			fmt.Printf("BUG: %s has negative stock: %d\n", s.Name, s.Available())
		}
	}
}

func simulateSale(venue *ConcertVenue) {
	var wg sync.WaitGroup
	for i := 0; i < buyerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			venue.ProcessBuyer()
		}()
	}
	wg.Wait()
}

func main() {
	venue := NewConcertVenue()
	simulateSale(venue)
	venue.Report()
	venue.VerifyIntegrity()
}
```

### Verification
```bash
go run -race main.go
```
No section ever goes negative. Total reservations + rejections = 2000. No race warnings.

## Step 4 -- Compare CAS vs Mutex: Pessimistic Locking

The same reservation system using `sync.Mutex`. Compare the approaches structurally:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	totalBuyers  = 5000
	totalTickets = 500
)

type CASReservationSystem struct {
	stock   int64
	success atomic.Int64
}

func NewCASReservationSystem(stock int64) *CASReservationSystem {
	return &CASReservationSystem{stock: stock}
}

func (s *CASReservationSystem) TryReserve(quantity int64) bool {
	for {
		current := atomic.LoadInt64(&s.stock)
		if current < quantity {
			return false
		}
		if atomic.CompareAndSwapInt64(&s.stock, current, current-quantity) {
			s.success.Add(1)
			return true
		}
	}
}

func (s *CASReservationSystem) Remaining() int64 { return atomic.LoadInt64(&s.stock) }
func (s *CASReservationSystem) Sold() int64      { return s.success.Load() }

type MutexReservationSystem struct {
	mu      sync.Mutex
	stock   int64
	success atomic.Int64
}

func NewMutexReservationSystem(stock int64) *MutexReservationSystem {
	return &MutexReservationSystem{stock: stock}
}

func (s *MutexReservationSystem) TryReserve(quantity int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stock < quantity {
		return false
	}
	s.stock -= quantity
	s.success.Add(1)
	return true
}

func (s *MutexReservationSystem) Remaining() int64 { return s.stock }
func (s *MutexReservationSystem) Sold() int64      { return s.success.Load() }

type ReservationBenchmark struct {
	Duration time.Duration
	Sold     int64
	Remaining int64
}

func benchmarkCAS(buyers int, tickets int64) ReservationBenchmark {
	system := NewCASReservationSystem(tickets)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			system.TryReserve(1)
		}()
	}
	wg.Wait()

	return ReservationBenchmark{
		Duration:  time.Since(start),
		Sold:      system.Sold(),
		Remaining: system.Remaining(),
	}
}

func benchmarkMutex(buyers int, tickets int64) ReservationBenchmark {
	system := NewMutexReservationSystem(tickets)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			system.TryReserve(1)
		}()
	}
	wg.Wait()

	return ReservationBenchmark{
		Duration:  time.Since(start),
		Sold:      system.Sold(),
		Remaining: system.Remaining(),
	}
}

func main() {
	casResult := benchmarkCAS(totalBuyers, totalTickets)
	mutexResult := benchmarkMutex(totalBuyers, totalTickets)

	fmt.Println("=== CAS vs Mutex Reservation ===")
	fmt.Printf("CAS:   sold=%d remaining=%d time=%v\n",
		casResult.Sold, casResult.Remaining, casResult.Duration)
	fmt.Printf("Mutex: sold=%d remaining=%d time=%v\n",
		mutexResult.Sold, mutexResult.Remaining, mutexResult.Duration)
	fmt.Printf("Ratio (Mutex/CAS): %.2fx\n",
		float64(mutexResult.Duration)/float64(casResult.Duration))
}
```

### Verification
```bash
go run main.go
```
Both approaches sell exactly 500 tickets with 0 remaining. CAS is typically faster for this simple check-and-decrement. Under higher contention with longer critical sections, mutex may win because it parks goroutines instead of spinning.

## Verification

Run the race detector on Steps 2-4:
```bash
go run -race main.go
```
All should pass with zero warnings. Step 1 should show `DATA RACE`.

## Common Mistakes

### Forgetting to Reload in the CAS Loop

**Wrong:**
```go
package main

import "sync/atomic"

// badReserve loads the stock value only once outside the loop.
// Under contention, every CAS after the first uses a stale value
// and fails indefinitely.
func badReserve(stock *int64) bool {
	current := atomic.LoadInt64(stock) // loaded once -- never refreshed
	for {
		if current <= 0 {
			return false
		}
		if atomic.CompareAndSwapInt64(stock, current, current-1) {
			return true
		}
		// BUG: current is stale. Every subsequent CAS uses the wrong old value.
	}
}

func main() {
	var s int64 = 10
	badReserve(&s) // works once, infinite-loops under contention
}
```

**Fix:** Reload `current` at the start of each loop iteration:
```go
for {
    current := atomic.LoadInt64(stock)
    if current <= 0 { return false }
    if atomic.CompareAndSwapInt64(stock, current, current-1) { return true }
}
```

### Using CAS Where AddInt64 Suffices

**Wrong (not broken, but wasteful):**
```go
func increment(addr *int64) {
    for {
        old := atomic.LoadInt64(addr)
        if atomic.CompareAndSwapInt64(addr, old, old+1) { return }
    }
}
```

This is `atomic.AddInt64` with extra steps: more code, more retries under contention, same result. Reserve CAS for operations that need a condition check before the update.

### Ignoring the ABA Problem

The ABA problem: a value changes from A to B and back to A. A CAS checking for A succeeds even though the value was modified in between. For simple counters and inventory this is harmless (stock going from 5 to 4 to 5 is fine -- the final state is valid). For pointer-based lock-free data structures, ABA can cause corruption. Go does not provide tagged pointers or double-word CAS. If you encounter ABA concerns, use a mutex.

## Review

Compare-and-swap collapses check-then-act into one indivisible instruction:
`CompareAndSwapInt64` verifies the address still holds the value you loaded
and, only then, writes the new one, returning false without touching anything
if some other goroutine got there first. Wrapped in a retry loop -- load, check
the condition, compute the new value, attempt the swap, loop on failure -- that
primitive gives you optimistic concurrency: no lock, no blocking, just a
re-read and another try under contention. The one non-negotiable is reloading
the current value at the top of every iteration; carry a stale value forward
and every CAS after the first compares against the wrong old value and fails
forever. It is the right tool precisely when a condition gates the update --
inventory that must not go negative, rate limits, seat booking -- and the wrong
tool for a plain increment, where `atomic.AddInt64` does the same thing with
fewer retries.

The comparison with a mutex is the judgment call. `atomic.AddInt64` cannot
solve the reservation because it decrements unconditionally -- it has no way to
refuse when stock hits zero, which is the entire requirement. When a CAS fails
the goroutine simply loops and retries against the fresh value; it never
blocks. That makes CAS win at low-to-medium contention, where most attempts
succeed on the first try and there is no lock to acquire. A mutex wins when
contention is high or the critical section is long, because a spinning CAS loop
burns CPU while a mutex parks the loser until it can make progress. Knowing
which regime you are in is the whole decision.

## Resources
- [atomic.CompareAndSwapInt64](https://pkg.go.dev/sync/atomic#CompareAndSwapInt64) -- the exact signature and semantics of the primitive the retry loop is built on.
- [Optimistic Concurrency Control (Wikipedia)](https://en.wikipedia.org/wiki/Optimistic_concurrency_control) -- the general load-check-retry model CAS is a hardware instance of.
- [ABA Problem (Wikipedia)](https://en.wikipedia.org/wiki/ABA_problem) -- the A-to-B-to-A hazard that makes bare CAS unsafe for pointer-based structures.

---

Back to [Concurrency](../../concurrency.md) | Next: [04-atomic-value-dynamic-config](../04-atomic-value-dynamic-config/04-atomic-value-dynamic-config.md)
