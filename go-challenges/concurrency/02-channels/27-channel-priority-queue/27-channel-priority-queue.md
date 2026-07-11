# Exercise 27: Channel-Based Priority Queue

An alerting system takes critical, warning, and info alerts, and a single
dispatcher must always handle critical before warning and warning before info.
The obvious build -- three channels and a `select` -- quietly fails, because
Go's `select` chooses uniformly at random among ready channels: with one
critical and ten info alerts waiting, the critical alert has only even odds of
going first. That randomness is deliberate (it prevents starvation) but it
destroys priority. This exercise walks from the naive select to the nested drain
pattern that gives real priority, then measures the starvation that strict
priority buys you under sustained load.

## What you'll build

```text
27-channel-priority-queue/
  main.go        FIFO baseline, the broken naive-select dispatcher, a priority
                 drain loop, and a starvation analysis under continuous load
```

- Build: a priority alert dispatcher over three channels, from a broken naive version to a correct priority drain, plus a starvation measurement.
- Implement: a single-channel FIFO baseline, `naiveDispatcher`, `priorityDispatcher` with the nested-drain-plus-blocking-fallback loop, and a `Stats`-backed starvation analyzer.
- Verify: `go run main.go`, and `go run -race main.go` on the starvation step.

### Why select cannot give you priority

The obvious approach is three channels and a `select`. But Go's `select` is
uniformly random when multiple cases are ready: it gives every ready channel an
equal probability of being chosen. With 1 critical and 10 info alerts queued,
the critical alert has only a 1-in-2 chance of being selected first -- the
`select` picks randomly between the two ready channels, regardless of how many
items each holds. This is by design, because it prevents starvation, but it
breaks priority dispatch.

True priority requires explicit checking: drain the critical channel completely,
then check warning, then info. This is the nested drain pattern, and it is how
every priority queue over channels must be built in Go. Understanding it matters
because the naive `select` approach is a common production bug that only
manifests under load.

## Step 1 -- Single Channel Baseline

Start with a single channel carrying all alerts. No priority differentiation -- first in, first out.

```go
package main

import (
	"fmt"
)

const alertCount = 12

// Priority represents alert severity level.
type Priority int

const (
	PriorityCritical Priority = iota
	PriorityWarning
	PriorityInfo
)

// String returns the human-readable priority name.
func (p Priority) String() string {
	switch p {
	case PriorityCritical:
		return "CRITICAL"
	case PriorityWarning:
		return "WARNING"
	case PriorityInfo:
		return "INFO"
	default:
		return "UNKNOWN"
	}
}

// Alert represents a system alert with a priority level.
type Alert struct {
	ID       int
	Priority Priority
	Message  string
}

// dispatcher processes alerts from a single FIFO channel.
func dispatcher(alerts <-chan Alert, done chan<- struct{}) {
	processed := 0
	for alert := range alerts {
		fmt.Printf("  [%d] %-10s %s\n", processed+1, alert.Priority, alert.Message)
		processed++
	}
	done <- struct{}{}
}

func main() {
	alerts := make(chan Alert, alertCount)
	done := make(chan struct{}, 1)

	go dispatcher(alerts, done)

	// Send alerts in mixed order: critical, info, warning interleaved.
	alertDefs := []struct {
		prio Priority
		msg  string
	}{
		{PriorityInfo, "deployment v2.1 started"},
		{PriorityWarning, "disk /data at 82%"},
		{PriorityCritical, "db-primary unreachable"},
		{PriorityInfo, "cache hit rate 94%"},
		{PriorityInfo, "health check passed"},
		{PriorityWarning, "memory usage 78%"},
		{PriorityCritical, "api-gateway 5xx spike"},
		{PriorityInfo, "log rotation complete"},
		{PriorityWarning, "certificate expires in 7d"},
		{PriorityInfo, "backup completed"},
		{PriorityCritical, "redis cluster split-brain"},
		{PriorityInfo, "metrics export done"},
	}

	for i, def := range alertDefs {
		alerts <- Alert{ID: i + 1, Priority: def.prio, Message: def.msg}
	}
	close(alerts)

	<-done

	fmt.Printf("\nSingle channel: alerts processed in arrival order (no priority)\n")
}
```

With a single channel, alerts are processed in FIFO order. Critical alerts wait behind info alerts that arrived earlier. This is the problem we need to solve.

### Verification
```bash
go run main.go
```
Expected output:
```
  [1] INFO       deployment v2.1 started
  [2] WARNING    disk /data at 82%
  [3] CRITICAL   db-primary unreachable
  [4] INFO       cache hit rate 94%
  ...

Single channel: alerts processed in arrival order (no priority)
```

## Step 2 -- Naive Select: Observe the Fairness Problem

Split into three channels (one per priority). Use a standard `select` to read from whichever is ready. Observe that Go's `select` does NOT respect priority.

```go
package main

import (
	"fmt"
	"time"
)

const (
	naiveCriticalCount = 3
	naiveWarningCount  = 3
	naiveInfoCount     = 6
	naiveProcessDelay  = 10 * time.Millisecond
)

type Priority int

const (
	PriorityCritical Priority = iota
	PriorityWarning
	PriorityInfo
)

func (p Priority) String() string {
	switch p {
	case PriorityCritical:
		return "CRITICAL"
	case PriorityWarning:
		return "WARNING"
	case PriorityInfo:
		return "INFO"
	default:
		return "UNKNOWN"
	}
}

type Alert struct {
	ID       int
	Priority Priority
	Message  string
}

// naiveDispatcher uses select -- which is uniformly random, not priority-based.
func naiveDispatcher(
	critical <-chan Alert,
	warning <-chan Alert,
	info <-chan Alert,
	done chan<- []Alert,
) {
	var processed []Alert
	critOpen, warnOpen, infoOpen := true, true, true

	for critOpen || warnOpen || infoOpen {
		select {
		case alert, ok := <-critical:
			if !ok {
				critOpen = false
				continue
			}
			time.Sleep(naiveProcessDelay)
			processed = append(processed, alert)
		case alert, ok := <-warning:
			if !ok {
				warnOpen = false
				continue
			}
			time.Sleep(naiveProcessDelay)
			processed = append(processed, alert)
		case alert, ok := <-info:
			if !ok {
				infoOpen = false
				continue
			}
			time.Sleep(naiveProcessDelay)
			processed = append(processed, alert)
		}
	}

	done <- processed
}

func main() {
	critical := make(chan Alert, 10)
	warning := make(chan Alert, 10)
	info := make(chan Alert, 10)
	done := make(chan []Alert, 1)

	// Pre-fill all channels before starting dispatcher.
	id := 1
	for i := 0; i < naiveCriticalCount; i++ {
		critical <- Alert{ID: id, Priority: PriorityCritical, Message: fmt.Sprintf("critical-%d", i+1)}
		id++
	}
	for i := 0; i < naiveWarningCount; i++ {
		warning <- Alert{ID: id, Priority: PriorityWarning, Message: fmt.Sprintf("warning-%d", i+1)}
		id++
	}
	for i := 0; i < naiveInfoCount; i++ {
		info <- Alert{ID: id, Priority: PriorityInfo, Message: fmt.Sprintf("info-%d", i+1)}
		id++
	}
	close(critical)
	close(warning)
	close(info)

	go naiveDispatcher(critical, warning, info, done)

	processed := <-done

	fmt.Println("NAIVE SELECT PROCESSING ORDER:")
	fmt.Printf("%-5s %-10s %s\n", "POS", "PRIORITY", "MESSAGE")
	fmt.Println("-------------------------------")

	critBeforeInfo := true
	firstInfoPos := -1
	lastCritPos := -1
	for i, alert := range processed {
		fmt.Printf("%-5d %-10s %s\n", i+1, alert.Priority, alert.Message)
		if alert.Priority == PriorityInfo && firstInfoPos == -1 {
			firstInfoPos = i
		}
		if alert.Priority == PriorityCritical {
			lastCritPos = i
		}
	}

	if firstInfoPos >= 0 && lastCritPos > firstInfoPos {
		critBeforeInfo = false
	}

	fmt.Printf("\nAll critical before any info: %v\n", critBeforeInfo)
	fmt.Println("(Run multiple times -- select is random, results will vary)")
}
```

Run this multiple times. Sometimes critical alerts appear first, sometimes info alerts sneak in before critical ones are fully drained. This is the fundamental problem: `select` treats all ready channels equally.

### Verification
```bash
go run main.go && go run main.go && go run main.go
```
Expected output (varies each run):
```
NAIVE SELECT PROCESSING ORDER:
POS   PRIORITY   MESSAGE
-------------------------------
1     INFO       info-2
2     CRITICAL   critical-1
3     WARNING    warning-1
4     INFO       info-4
5     CRITICAL   critical-2
...

All critical before any info: false
(Run multiple times -- select is random, results will vary)
```

## Step 3 -- Priority Drain Loop

Replace the naive `select` with explicit priority checking: always drain critical first, then warning, then info. Only fall through to lower priorities when higher-priority channels are empty.

```go
package main

import (
	"fmt"
	"time"
)

const (
	prioCriticalCount = 3
	prioWarningCount  = 3
	prioInfoCount     = 6
	prioProcessDelay  = 10 * time.Millisecond
)

type Priority int

const (
	PriorityCritical Priority = iota
	PriorityWarning
	PriorityInfo
)

func (p Priority) String() string {
	switch p {
	case PriorityCritical:
		return "CRITICAL"
	case PriorityWarning:
		return "WARNING"
	case PriorityInfo:
		return "INFO"
	default:
		return "UNKNOWN"
	}
}

type Alert struct {
	ID       int
	Priority Priority
	Message  string
}

// priorityDispatcher drains channels in strict priority order.
func priorityDispatcher(
	critical <-chan Alert,
	warning <-chan Alert,
	info <-chan Alert,
	done chan<- []Alert,
) {
	var processed []Alert

	for {
		// Priority 1: drain all critical alerts.
		select {
		case alert, ok := <-critical:
			if ok {
				time.Sleep(prioProcessDelay)
				processed = append(processed, alert)
				continue
			}
			critical = nil // closed, remove from consideration
		default:
			// No critical alerts pending.
		}

		// Priority 2: drain all warning alerts.
		select {
		case alert, ok := <-warning:
			if ok {
				time.Sleep(prioProcessDelay)
				processed = append(processed, alert)
				continue
			}
			warning = nil
		default:
			// No warning alerts pending.
		}

		// Priority 3: drain info alerts.
		select {
		case alert, ok := <-info:
			if ok {
				time.Sleep(prioProcessDelay)
				processed = append(processed, alert)
				continue
			}
			info = nil
		default:
			// No info alerts pending.
		}

		// All channels nil (closed) and drained: exit.
		if critical == nil && warning == nil && info == nil {
			break
		}

		// All channels open but empty: block until something arrives.
		// Re-check in priority order after waking.
		select {
		case alert, ok := <-critical:
			if ok {
				time.Sleep(prioProcessDelay)
				processed = append(processed, alert)
			} else {
				critical = nil
			}
		case alert, ok := <-warning:
			if ok {
				time.Sleep(prioProcessDelay)
				processed = append(processed, alert)
			} else {
				warning = nil
			}
		case alert, ok := <-info:
			if ok {
				time.Sleep(prioProcessDelay)
				processed = append(processed, alert)
			} else {
				info = nil
			}
		}
	}

	done <- processed
}

func main() {
	critical := make(chan Alert, 10)
	warning := make(chan Alert, 10)
	info := make(chan Alert, 10)
	done := make(chan []Alert, 1)

	id := 1
	for i := 0; i < prioCriticalCount; i++ {
		critical <- Alert{ID: id, Priority: PriorityCritical, Message: fmt.Sprintf("critical-%d", i+1)}
		id++
	}
	for i := 0; i < prioWarningCount; i++ {
		warning <- Alert{ID: id, Priority: PriorityWarning, Message: fmt.Sprintf("warning-%d", i+1)}
		id++
	}
	for i := 0; i < prioInfoCount; i++ {
		info <- Alert{ID: id, Priority: PriorityInfo, Message: fmt.Sprintf("info-%d", i+1)}
		id++
	}
	close(critical)
	close(warning)
	close(info)

	go priorityDispatcher(critical, warning, info, done)

	processed := <-done

	fmt.Println("PRIORITY DRAIN PROCESSING ORDER:")
	fmt.Printf("%-5s %-10s %s\n", "POS", "PRIORITY", "MESSAGE")
	fmt.Println("-------------------------------")

	allCritFirst := true
	allWarnBeforeInfo := true
	critDone, warnDone := false, false

	for i, alert := range processed {
		fmt.Printf("%-5d %-10s %s\n", i+1, alert.Priority, alert.Message)
		switch alert.Priority {
		case PriorityCritical:
			if critDone {
				allCritFirst = false
			}
		case PriorityWarning:
			critDone = true
			if warnDone {
				allWarnBeforeInfo = false
			}
		case PriorityInfo:
			critDone = true
			warnDone = true
		}
	}

	fmt.Printf("\nAll CRITICAL before WARNING: %v\n", allCritFirst)
	fmt.Printf("All WARNING before INFO:    %v\n", allWarnBeforeInfo)
	fmt.Println("(Run multiple times -- priority order is now deterministic)")
}
```

The nested drain pattern checks critical first with a non-blocking `select` (using `default`). Only if critical is empty does it check warning, then info. The final blocking `select` at the bottom handles the case where all channels are open but empty -- it waits until something arrives, then loops back to re-check in priority order.

### Verification
```bash
go run main.go && go run main.go && go run main.go
```
Expected output (same every run):
```
PRIORITY DRAIN PROCESSING ORDER:
POS   PRIORITY   MESSAGE
-------------------------------
1     CRITICAL   critical-1
2     CRITICAL   critical-2
3     CRITICAL   critical-3
4     WARNING    warning-1
5     WARNING    warning-2
6     WARNING    warning-3
7     INFO       info-1
8     INFO       info-2
9     INFO       info-3
10    INFO       info-4
11    INFO       info-5
12    INFO       info-6

All CRITICAL before WARNING: true
All WARNING before INFO:    true
(Run multiple times -- priority order is now deterministic)
```

## Step 4 -- Starvation Analysis Under Continuous High-Priority Traffic

Measure what happens when critical alerts arrive continuously. Info alerts may never be processed -- this is starvation. Analyze and quantify the risk.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	starvationDuration = 300 * time.Millisecond
	starvationDelay    = 10 * time.Millisecond
	criticalInterval   = 4 * time.Millisecond
	infoInterval       = 20 * time.Millisecond
)

type Priority int

const (
	PriorityCritical Priority = iota
	PriorityWarning
	PriorityInfo
)

func (p Priority) String() string {
	switch p {
	case PriorityCritical:
		return "CRITICAL"
	case PriorityWarning:
		return "WARNING"
	case PriorityInfo:
		return "INFO"
	default:
		return "UNKNOWN"
	}
}

type Alert struct {
	ID        int
	Priority  Priority
	Message   string
	CreatedAt time.Time
}

// Stats tracks processing counts per priority.
type Stats struct {
	mu             sync.Mutex
	critProcessed  int
	warnProcessed  int
	infoProcessed  int
	critLatencySum time.Duration
	infoLatencySum time.Duration
}

func (s *Stats) record(alert Alert) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latency := time.Since(alert.CreatedAt)
	switch alert.Priority {
	case PriorityCritical:
		s.critProcessed++
		s.critLatencySum += latency
	case PriorityWarning:
		s.warnProcessed++
	case PriorityInfo:
		s.infoProcessed++
		s.infoLatencySum += latency
	}
}

// priorityDispatcher with strict priority drain.
func priorityDispatcher(
	critical <-chan Alert,
	warning <-chan Alert,
	info <-chan Alert,
	stop <-chan struct{},
	stats *Stats,
	done chan<- struct{},
) {
	defer func() { done <- struct{}{} }()

	for {
		// Check for stop signal first.
		select {
		case <-stop:
			return
		default:
		}

		// Priority drain: critical first.
		drained := false
		select {
		case alert, ok := <-critical:
			if ok {
				time.Sleep(starvationDelay)
				stats.record(alert)
				drained = true
			}
		default:
		}
		if drained {
			continue
		}

		// Then warning.
		select {
		case alert, ok := <-warning:
			if ok {
				time.Sleep(starvationDelay)
				stats.record(alert)
				drained = true
			}
		default:
		}
		if drained {
			continue
		}

		// Then info.
		select {
		case alert, ok := <-info:
			if ok {
				time.Sleep(starvationDelay)
				stats.record(alert)
				drained = true
			}
		default:
		}
		if drained {
			continue
		}

		// Nothing ready: block until something arrives or stop.
		select {
		case <-stop:
			return
		case alert := <-critical:
			time.Sleep(starvationDelay)
			stats.record(alert)
		case alert := <-warning:
			time.Sleep(starvationDelay)
			stats.record(alert)
		case alert := <-info:
			time.Sleep(starvationDelay)
			stats.record(alert)
		}
	}
}

func main() {
	critical := make(chan Alert, 100)
	warning := make(chan Alert, 100)
	info := make(chan Alert, 100)
	stop := make(chan struct{})
	done := make(chan struct{}, 1)
	stats := &Stats{}

	go priorityDispatcher(critical, warning, info, stop, stats, done)

	// Producers: continuous critical and info traffic.
	var prodWG sync.WaitGroup

	critSent := 0
	prodWG.Add(1)
	go func() {
		defer prodWG.Done()
		id := 0
		ticker := time.NewTicker(criticalInterval)
		defer ticker.Stop()
		timeout := time.After(starvationDuration)
		for {
			select {
			case <-timeout:
				return
			case <-ticker.C:
				id++
				critical <- Alert{
					ID:        id,
					Priority:  PriorityCritical,
					Message:   fmt.Sprintf("critical-%d", id),
					CreatedAt: time.Now(),
				}
				critSent++
			}
		}
	}()

	infoSent := 0
	prodWG.Add(1)
	go func() {
		defer prodWG.Done()
		id := 0
		ticker := time.NewTicker(infoInterval)
		defer ticker.Stop()
		timeout := time.After(starvationDuration)
		for {
			select {
			case <-timeout:
				return
			case <-ticker.C:
				id++
				info <- Alert{
					ID:        id + 1000,
					Priority:  PriorityInfo,
					Message:   fmt.Sprintf("info-%d", id),
					CreatedAt: time.Now(),
				}
				infoSent++
			}
		}
	}()

	// Wait for producers to finish, then stop dispatcher.
	prodWG.Wait()
	close(stop)
	<-done

	stats.mu.Lock()
	defer stats.mu.Unlock()

	fmt.Println("=== Starvation Analysis ===")
	fmt.Printf("Duration:         %v\n", starvationDuration)
	fmt.Printf("Critical sent:    %d\n", critSent)
	fmt.Printf("Critical handled: %d\n", stats.critProcessed)
	fmt.Printf("Info sent:        %d\n", infoSent)
	fmt.Printf("Info handled:     %d\n", stats.infoProcessed)
	fmt.Println()

	if stats.infoProcessed == 0 {
		fmt.Println("STARVATION DETECTED: zero info alerts processed")
		fmt.Println("Critical traffic saturated the dispatcher entirely")
	} else {
		infoPercent := float64(stats.infoProcessed) / float64(infoSent) * 100
		fmt.Printf("Info throughput:   %.1f%% (%d of %d)\n",
			infoPercent, stats.infoProcessed, infoSent)
	}

	if stats.critProcessed > 0 {
		avgCritLatency := stats.critLatencySum / time.Duration(stats.critProcessed)
		fmt.Printf("Avg critical latency: %v\n", avgCritLatency.Round(time.Millisecond))
	}
	if stats.infoProcessed > 0 {
		avgInfoLatency := stats.infoLatencySum / time.Duration(stats.infoProcessed)
		fmt.Printf("Avg info latency:     %v\n", avgInfoLatency.Round(time.Millisecond))
	}

	fmt.Println()
	fmt.Println("This is the fundamental priority tradeoff:")
	fmt.Println("strict priority guarantees critical-first, but starves lower priorities")
	fmt.Println("under sustained high-priority load. Mitigation: weighted fair queuing")
	fmt.Println("or guaranteed minimum slots per priority level.")
}
```

### Verification
```bash
go run -race main.go
```
Expected output (values vary):
```
=== Starvation Analysis ===
Duration:         300ms
Critical sent:    75
Critical handled: 29
Info sent:        15
Info handled:     0

STARVATION DETECTED: zero info alerts processed
Critical traffic saturated the dispatcher entirely

Avg critical latency: 98ms

This is the fundamental priority tradeoff:
strict priority guarantees critical-first, but starves lower priorities
under sustained high-priority load. Mitigation: weighted fair queuing
or guaranteed minimum slots per priority level.
```

## Common Mistakes

### Assuming Select Respects Case Order
**What happens:** A developer writes `select { case <-critical: ... case <-info: ... }` and assumes critical will be chosen first because it appears first in the code. Under load, info alerts are processed 50% of the time when both channels are ready.
**Fix:** Go's `select` is uniformly random among ready cases (Go specification). Use the nested drain pattern with non-blocking selects and explicit priority ordering.

### Spinning on Non-Blocking Select Without a Blocking Fallback
**What happens:** The priority drain loop uses only non-blocking selects (with `default`). When all channels are empty, the loop spins at 100% CPU, checking channels millions of times per second.
**Fix:** After the priority drain checks, add a final blocking `select` (without `default`) that waits until any channel has data. This puts the goroutine to sleep until there is work to do.

### Ignoring Starvation in Production
**What happens:** Priority dispatch works perfectly in testing. In production, a burst of critical alerts during an incident starves warning alerts (disk filling) and info alerts (log rotation). The disk fills up completely, making the incident worse.
**Fix:** Implement guaranteed minimum throughput per priority level: process at least 1 lower-priority item for every N high-priority items. This is weighted fair queuing -- it sacrifices strict priority ordering for overall system health.

## Review

Go's `select` is uniformly random among ready cases, so source-code order
carries no weight and naive multi-channel dispatch gives every priority equal
odds. Real priority comes from the nested drain: non-blocking selects, each with
a `default`, check the channels in strict order -- critical, then warning, then
info -- and only fall through when the higher level is empty. A final blocking
`select`, without a default, sits at the bottom so the loop sleeps instead of
spinning at 100% CPU when every channel is empty. Setting a channel to nil once
it closes removes it from consideration without disturbing the others. But
strict priority has a price: under sustained high-priority load the lower levels
starve, which is why production dispatchers reach for weighted fair queuing or a
guaranteed minimum number of slots per level.

Three checks tell you whether the pattern is yours. First, change the dispatcher
to process one info alert for every five critical ones even under a critical
flood, and confirm the starvation you measured disappears -- that is weighted
fair queuing in miniature. Second, be able to say why the final blocking
`select` does not break priority: after it wakes on any channel the loop starts
over at the top and re-drains in priority order, so the one value it accepted out
of turn is bounded, and the next pass re-establishes the ranking. Third, know the
difference between a nil channel and a closed one inside a `select`: a receive on
a nil channel blocks forever and so drops that case out of contention -- exactly
why you nil a channel once it drains closed -- whereas a receive on a closed
channel returns immediately with the zero value and `ok == false`.

## Resources
- [Go Language Specification: Select](https://go.dev/ref/spec#Select_statements) -- the rule that a ready select chooses a uniform-random case.
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- select-driven coordination and the nil-channel trick.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- the channel and select basics underpinning the drain loop.
- [Weighted Fair Queuing (Wikipedia)](https://en.wikipedia.org/wiki/Weighted_fair_queueing) -- the scheduling discipline that mitigates the starvation strict priority causes.

---

Back to [Concurrency](../../concurrency.md) | Next: [28-channel-bridge-pattern](../28-channel-bridge-pattern/28-channel-bridge-pattern.md)
