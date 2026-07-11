# Exercise 18: Multi-Producer Single Consumer

Five microservices -- auth, billing, inventory, shipping, notifications -- each
write log entries, and one aggregator collects them all through a single shared
channel. Go makes the sending trivially safe: many goroutines can send on the
same channel and the runtime serializes them. The hard part is closing. Only
one goroutine may close the channel, and only after every producer has
finished -- close too early and the still-sending producers panic, close never
and the consumer's range blocks forever. This exercise builds up to the
WaitGroup-close pattern that gets the timing exactly right.

## What you'll build

```text
18-multi-producer-single-consumer/
  main.go        log aggregator fed by five concurrent producers over one
                 shared channel (each step is its own runnable program)
```

- Build: a log aggregator fed by five concurrent service producers over one shared channel.
- Implement: `ServiceLogger.WriteLogs` producers, a `LogAggregator` that ranges the channel, and a coordinator goroutine that runs `wg.Wait()` then `close`.
- Verify: `go run main.go`, and `go run -race main.go` where the lesson checks the WaitGroup-close pattern.

### Why closing is the hard part, not sending

This pattern is safe in Go because multiple goroutines can send to the same channel concurrently -- the runtime handles synchronization. The hard part is closing the channel. Only one goroutine should close it, and only after all producers have finished. If a producer closes the channel while others are still sending, they panic. If nobody closes it, the consumer's `range` loop blocks forever.

The WaitGroup-close pattern solves this: each producer increments a WaitGroup before starting and decrements it when done. A separate coordinator goroutine waits for the WaitGroup to reach zero, then closes the channel. The coordinator does not produce or consume -- it only manages the lifecycle.

## Step 1 -- One Producer, One Consumer

Start with the simplest case: one service writes log entries, one aggregator reads them. The producer closes the channel when done.

```go
package main

import (
	"fmt"
	"time"
)

// LogEntry represents a structured log line from a microservice.
type LogEntry struct {
	Service   string
	Level     string
	Message   string
	Timestamp time.Time
}

// ServiceLogger produces log entries for a single service.
type ServiceLogger struct {
	name string
}

// NewServiceLogger creates a logger for the named service.
func NewServiceLogger(name string) *ServiceLogger {
	return &ServiceLogger{name: name}
}

// WriteLogs sends a batch of log entries into the shared channel.
func (sl *ServiceLogger) WriteLogs(out chan<- LogEntry, entries []string) {
	for _, msg := range entries {
		out <- LogEntry{
			Service:   sl.name,
			Level:     "INFO",
			Message:   msg,
			Timestamp: time.Now(),
		}
	}
}

// LogAggregator reads log entries from a channel and collects statistics.
type LogAggregator struct {
	totalEntries   int
	entriesByLevel map[string]int
}

// NewLogAggregator creates an aggregator ready to consume entries.
func NewLogAggregator() *LogAggregator {
	return &LogAggregator{
		entriesByLevel: make(map[string]int),
	}
}

// Start reads entries until the channel closes.
func (la *LogAggregator) Start(in <-chan LogEntry) {
	for entry := range in {
		la.totalEntries++
		la.entriesByLevel[entry.Level]++
		fmt.Printf("  [%s] %s: %s\n", entry.Level, entry.Service, entry.Message)
	}
}

// Report prints the aggregated statistics.
func (la *LogAggregator) Report() {
	fmt.Printf("\nAggregator Report: %d total entries\n", la.totalEntries)
	for level, count := range la.entriesByLevel {
		fmt.Printf("  %s: %d\n", level, count)
	}
}

func main() {
	logCh := make(chan LogEntry, 10)

	authLogger := NewServiceLogger("auth")
	aggregator := NewLogAggregator()

	fmt.Println("=== One Producer, One Consumer ===")

	// Single producer: safe to close from here.
	go func() {
		authLogger.WriteLogs(logCh, []string{
			"user login: alice@example.com",
			"token refreshed: alice@example.com",
			"user login: bob@example.com",
		})
		close(logCh) // single producer owns the close
	}()

	aggregator.Start(logCh)
	aggregator.Report()
}
```

With one producer, closing is trivial: the producer closes the channel when it finishes sending. The consumer's `range` exits cleanly.

### Verification
```bash
go run main.go
# Expected:
#   3 log entries from auth service
#   Aggregator Report: 3 total entries, INFO: 3
```

## Step 2 -- Five Producers Sharing One Channel

Five services send logs concurrently to the same channel. Each service runs in its own goroutine. For now, we use a fixed `time.Sleep` to wait for all producers -- Step 3 replaces this with the proper WaitGroup-close pattern.

```go
package main

import (
	"fmt"
	"time"
)

const drainDelay = 500 * time.Millisecond

type LogEntry struct {
	Service   string
	Level     string
	Message   string
	Timestamp time.Time
}

type ServiceLogger struct {
	name string
}

func NewServiceLogger(name string) *ServiceLogger {
	return &ServiceLogger{name: name}
}

func (sl *ServiceLogger) WriteLogs(out chan<- LogEntry, entries []LogEntry) {
	for _, entry := range entries {
		entry.Service = sl.name
		entry.Timestamp = time.Now()
		out <- entry
	}
}

type LogAggregator struct {
	totalEntries    int
	entriesByService map[string]int
}

func NewLogAggregator() *LogAggregator {
	return &LogAggregator{
		entriesByService: make(map[string]int),
	}
}

func (la *LogAggregator) Consume(in <-chan LogEntry) {
	for entry := range in {
		la.totalEntries++
		la.entriesByService[entry.Service]++
		fmt.Printf("  [%-12s] %s: %s\n", entry.Service, entry.Level, entry.Message)
	}
}

func (la *LogAggregator) Report() {
	fmt.Printf("\nAggregator Report: %d total entries\n", la.totalEntries)
	for service, count := range la.entriesByService {
		fmt.Printf("  %-12s: %d entries\n", service, count)
	}
}

func main() {
	logCh := make(chan LogEntry, 50)
	aggregator := NewLogAggregator()

	services := map[string][]LogEntry{
		"auth": {
			{Level: "INFO", Message: "user login: alice"},
			{Level: "WARN", Message: "failed login attempt: eve"},
		},
		"billing": {
			{Level: "INFO", Message: "invoice generated: INV-1001"},
			{Level: "ERROR", Message: "payment declined: card expired"},
		},
		"inventory": {
			{Level: "INFO", Message: "stock updated: Widget x100"},
			{Level: "INFO", Message: "low stock alert: Gadget x5"},
		},
		"shipping": {
			{Level: "INFO", Message: "shipment created: SHP-5001"},
		},
		"notification": {
			{Level: "INFO", Message: "email sent: order confirmation"},
			{Level: "INFO", Message: "sms sent: delivery update"},
			{Level: "WARN", Message: "email bounced: invalid address"},
		},
	}

	fmt.Println("=== Five Producers, One Consumer ===")

	for name, entries := range services {
		logger := NewServiceLogger(name)
		go logger.WriteLogs(logCh, entries)
	}

	// BAD: using sleep to wait for producers. Step 3 fixes this.
	time.Sleep(drainDelay)
	close(logCh)

	aggregator.Consume(logCh)
	aggregator.Report()

	fmt.Println()
	fmt.Println("Problem: time.Sleep is fragile. If producers are slower, we miss entries.")
	fmt.Println("Solution: WaitGroup-close pattern (Step 3).")
}
```

This works, but `time.Sleep` is fragile. If any producer takes longer than expected, the channel closes before it finishes sending -- causing a panic on send to a closed channel. Step 3 fixes this properly.

### Verification
```bash
go run main.go
# Expected:
#   10 log entries from 5 services (order may vary due to concurrency)
#   Aggregator Report: 10 entries across 5 services
#   Warning about time.Sleep fragility
```

## Step 3 -- WaitGroup-Close Pattern

Replace the fragile `time.Sleep` with the proper pattern: a WaitGroup tracks all producers, and a coordinator goroutine waits for them to finish before closing the channel. This is the standard safe way to close a multi-producer channel.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type LogEntry struct {
	Service   string
	Level     string
	Message   string
	Timestamp time.Time
}

type ServiceLogger struct {
	name string
}

func NewServiceLogger(name string) *ServiceLogger {
	return &ServiceLogger{name: name}
}

// WriteLogs sends entries and marks itself done on the WaitGroup.
func (sl *ServiceLogger) WriteLogs(out chan<- LogEntry, entries []LogEntry, wg *sync.WaitGroup) {
	defer wg.Done()
	for _, entry := range entries {
		entry.Service = sl.name
		entry.Timestamp = time.Now()
		out <- entry
	}
	fmt.Printf("  [%-12s] finished sending %d entries\n", sl.name, len(entries))
}

type LogAggregator struct {
	totalEntries     int
	entriesByService map[string]int
	entriesByLevel   map[string]int
}

func NewLogAggregator() *LogAggregator {
	return &LogAggregator{
		entriesByService: make(map[string]int),
		entriesByLevel:   make(map[string]int),
	}
}

func (la *LogAggregator) Start(in <-chan LogEntry) {
	for entry := range in {
		la.totalEntries++
		la.entriesByService[entry.Service]++
		la.entriesByLevel[entry.Level]++
		fmt.Printf("  AGG [%-12s] %s: %s\n", entry.Service, entry.Level, entry.Message)
	}
}

func (la *LogAggregator) Report() {
	fmt.Printf("\n=== Aggregator Report ===\n")
	fmt.Printf("Total entries: %d\n", la.totalEntries)

	fmt.Println()
	fmt.Println("By service:")
	for service, count := range la.entriesByService {
		fmt.Printf("  %-12s: %d\n", service, count)
	}

	fmt.Println()
	fmt.Println("By level:")
	for level, count := range la.entriesByLevel {
		fmt.Printf("  %-5s: %d\n", level, count)
	}
}

func main() {
	logCh := make(chan LogEntry, 50)
	aggregator := NewLogAggregator()
	var wg sync.WaitGroup

	services := map[string][]LogEntry{
		"auth": {
			{Level: "INFO", Message: "user login: alice"},
			{Level: "WARN", Message: "failed login attempt: eve"},
			{Level: "INFO", Message: "token refreshed: alice"},
		},
		"billing": {
			{Level: "INFO", Message: "invoice generated: INV-1001"},
			{Level: "ERROR", Message: "payment declined: card expired"},
		},
		"inventory": {
			{Level: "INFO", Message: "stock updated: Widget x100"},
			{Level: "INFO", Message: "low stock alert: Gadget x5"},
		},
		"shipping": {
			{Level: "INFO", Message: "shipment created: SHP-5001"},
			{Level: "INFO", Message: "tracking updated: SHP-5001"},
		},
		"notification": {
			{Level: "INFO", Message: "email sent: order confirmation"},
			{Level: "WARN", Message: "email bounced: invalid address"},
		},
	}

	fmt.Println("=== WaitGroup-Close Pattern ===")
	fmt.Printf("Launching %d service loggers...\n\n", len(services))

	// Launch all producers with WaitGroup tracking.
	for name, entries := range services {
		wg.Add(1)
		logger := NewServiceLogger(name)
		go logger.WriteLogs(logCh, entries, &wg)
	}

	// Coordinator goroutine: waits for all producers, then closes the channel.
	// This is NOT a producer -- it only manages the channel lifecycle.
	go func() {
		wg.Wait()
		fmt.Println()
		fmt.Println("  [coordinator] all producers finished, closing channel")
		close(logCh)
	}()

	// Consumer blocks until all entries are read and channel closes.
	aggregator.Start(logCh)
	aggregator.Report()

	fmt.Println()
	fmt.Println("Pattern: WaitGroup tracks producers, coordinator closes channel.")
	fmt.Println("No producer closes the channel. No time.Sleep. No race.")
}
```

The WaitGroup-close pattern has three actors:
1. **Producers** (5 service loggers): each calls `wg.Done()` when finished
2. **Coordinator** (anonymous goroutine): calls `wg.Wait()` then `close(logCh)`
3. **Consumer** (aggregator): ranges over the channel until it closes

No producer closes the channel. The coordinator does not produce or consume. The responsibilities are cleanly separated.

### Verification
```bash
go run -race main.go
# Expected:
#   All 5 services send their entries
#   Coordinator prints "all producers finished, closing channel"
#   Aggregator report: 11 entries across 5 services
#   No race warnings
```

## Step 4 -- Wrong Close vs Correct Close

Demonstrate the difference between the wrong approach (a producer closes the channel, causing panic) and the correct WaitGroup-close pattern. This makes the failure mode visible.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type LogEntry struct {
	Service string
	Message string
}

// demonstrateWrongClose shows what happens when a producer closes the shared channel.
func demonstrateWrongClose() {
	fmt.Println("=== WRONG: Producer Closes Channel ===")
	fmt.Println("(Wrapped in recover to catch the panic)")
	fmt.Println()

	logCh := make(chan LogEntry, 10)
	var panicMessage any

	var wg sync.WaitGroup
	wg.Add(2)

	// Producer A: sends and then WRONGLY closes the channel.
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicMessage = r
			}
		}()
		for i := 0; i < 3; i++ {
			logCh <- LogEntry{Service: "service-A", Message: fmt.Sprintf("msg-%d", i)}
		}
		fmt.Println("  [service-A] done sending, CLOSING channel (WRONG!)")
		close(logCh)
	}()

	// Producer B: still sending when A closes the channel.
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicMessage = r
			}
		}()
		time.Sleep(10 * time.Millisecond) // ensure A closes first
		fmt.Println("  [service-B] trying to send on closed channel...")
		logCh <- LogEntry{Service: "service-B", Message: "this will panic"}
	}()

	wg.Wait()

	// Drain remaining entries.
	drainDone := make(chan struct{})
	go func() {
		for entry := range logCh {
			fmt.Printf("  consumed: [%s] %s\n", entry.Service, entry.Message)
		}
		close(drainDone)
	}()

	select {
	case <-drainDone:
	case <-time.After(100 * time.Millisecond):
	}

	if panicMessage != nil {
		fmt.Printf("\n  PANIC CAUGHT: %v\n", panicMessage)
		fmt.Println("  This is why producers must NEVER close a shared channel.")
	}
}

// demonstrateCorrectClose shows the WaitGroup-close pattern.
func demonstrateCorrectClose() {
	fmt.Println()
	fmt.Println("=== CORRECT: Coordinator Closes Channel ===")
	fmt.Println()

	logCh := make(chan LogEntry, 10)
	var wg sync.WaitGroup

	producerFunc := func(name string, count int) {
		defer wg.Done()
		for i := 0; i < count; i++ {
			logCh <- LogEntry{
				Service: name,
				Message: fmt.Sprintf("msg-%d", i),
			}
		}
		fmt.Printf("  [%-10s] done sending %d entries\n", name, count)
	}

	wg.Add(3)
	go producerFunc("service-A", 3)
	go producerFunc("service-B", 2)
	go producerFunc("service-C", 4)

	// Coordinator: waits for all, then closes.
	go func() {
		wg.Wait()
		close(logCh)
	}()

	consumed := 0
	for entry := range logCh {
		consumed++
		fmt.Printf("  consumed: [%-10s] %s\n", entry.Service, entry.Message)
	}

	fmt.Printf("\n  Total consumed: %d entries, no panics, no races\n", consumed)
}

func main() {
	demonstrateWrongClose()
	demonstrateCorrectClose()

	fmt.Println()
	fmt.Println("=== Key Takeaway ===")
	fmt.Println("  WRONG:   Any producer calls close() -> other producers panic on send")
	fmt.Println("  CORRECT: WaitGroup + coordinator goroutine closes after all producers finish")
	fmt.Println("  Rule:    Only the goroutine that KNOWS all sends are done should close the channel")
}
```

The wrong version demonstrates a real `send on closed channel` panic. Producer A finishes and closes the channel. Producer B is still running and panics when it tries to send. The correct version uses the coordinator pattern: no producer touches `close`, and the coordinator only closes after all WaitGroups are done.

### Verification
```bash
go run main.go
# Expected:
#   WRONG section: panic caught ("send on closed channel")
#   CORRECT section: all 9 entries consumed, no panics
#   Key takeaway printed
```

## Common Mistakes

### Any Producer Calls close()

**Wrong:**
```go
go func() {
    for _, msg := range messages {
        ch <- msg
    }
    close(ch) // this producer assumes it is the last one!
}()
```

**What happens:** If other producers are still sending, they panic with "send on closed channel". In production, this crashes the entire process.

**Fix:** No producer closes the channel. Use the WaitGroup-close pattern with a dedicated coordinator.

### Forgetting wg.Add Before Launching Goroutines

**Wrong:**
```go
for _, name := range services {
    go func(n string) {
        wg.Add(1) // TOO LATE: coordinator might call wg.Wait() before this runs
        defer wg.Done()
        produceLogs(n, ch)
    }(name)
}
```

**What happens:** The coordinator's `wg.Wait()` might return before all goroutines have called `wg.Add(1)`. The channel closes prematurely.

**Fix:** Always call `wg.Add(1)` in the launching goroutine, before `go`:
```go
for _, name := range services {
    wg.Add(1) // BEFORE launching the goroutine
    go func(n string) {
        defer wg.Done()
        produceLogs(n, ch)
    }(name)
}
```

### Closing the Channel Twice

**Wrong:**
```go
go func() {
    wg.Wait()
    close(ch)
}()

// Later, accidentally:
close(ch) // double close -> panic!
```

**What happens:** Closing an already-closed channel panics. Always have exactly one close path.

**Fix:** Only the coordinator goroutine closes the channel, and it does so exactly once.

## Review

The pattern rests on an asymmetry between sending and closing. Sending on a
shared channel from many goroutines is safe by construction -- the runtime
serializes concurrent sends, so five service loggers can write to one
aggregator channel without a mutex. Closing is the opposite: it must happen
exactly once, and only after the last send, because a send on a closed channel
is an unrecoverable panic and a second close panics too. The WaitGroup-close
pattern resolves the tension by handing the responsibility to a goroutine that
neither produces nor consumes: each producer calls `wg.Done()` when it
finishes, and a lone coordinator calls `wg.Wait()` and then `close`. No
producer ever touches close, the consumer just ranges, and `time.Sleep` --
which only ever guesses at when the producers are done -- disappears entirely.

Three details make it correct, and you should be able to justify each.
Concurrent sends are safe because the channel implementation synchronizes them
internally, so no send tears another's value. `wg.Add(1)` must run in the
launching goroutine before `go`, not inside the new goroutine, because
otherwise the coordinator's `wg.Wait()` can observe a counter of zero and close
the channel before a producer has even registered. What guarantees the
coordinator waits for all producers is exactly that ordering: every `Add`
happens-before the `Wait` that sees it. Take the close away and none of it
saves you -- the aggregator's range never reaches the end of a channel that is
never closed, and the program deadlocks.

## Resources
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the philosophy of coordinating goroutines through channels rather than shared state.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- fan-in and the ownership rules for who closes a shared channel.
- [sync.WaitGroup documentation](https://pkg.go.dev/sync#WaitGroup) -- the Add/Done/Wait contract the coordinator depends on.

---

Back to [Concurrency](../../concurrency.md) | Next: [19-channel-orchestration](../19-channel-orchestration/19-channel-orchestration.md)
