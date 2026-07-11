# Exercise 7: Or-Channel: First to Finish

The or-channel pattern runs the same work in several goroutines and takes
whichever result lands first, cancelling the rest -- speculative execution that
trades extra CPU for lower latency. The motivating case is reading from database
replicas: any replica usually answers in a few milliseconds, but every so often
one hits a GC pause or disk contention and spikes to half a second, dragging
your p99 with it. Query one replica and that tail is yours; race three and take
the fastest, and the odds of all three stalling at once are vanishingly small.
Google's "The Tail at Scale" made this the standard way to tame tail latency.

## What you'll build

```text
07-or-channel-first-to-finish/
  main.go        a basic replica race, a cancellation version, a tail-latency
                 benchmark, and a reusable recursive orChannel combiner
```

- Build: a replica racer that queries N sources and takes the fastest, cancelling the losers.
- Implement: a buffered-channel `RaceQuery`, a `context.WithCancel` version that cancels losing goroutines, a tail-latency benchmark, and a reusable recursive `orChannel` combiner.
- Verify: `go run main.go`.

### Why racing replicas cuts tail latency

The pattern has three parts: launch N goroutines doing equivalent work, select the first result from any of them, and cancel the rest immediately. Without proper cancellation, the losing goroutines waste resources running to completion.

```
  Database Replica Racing

  query ---> replica 1 (5ms)       --+
         --> replica 2 (500ms GC!) --+--> take first (5ms), cancel rest
         --> replica 3 (8ms)       --+

  User sees 5ms instead of 500ms. Tail latency reduced by 100x.
```

## Step 1 -- Basic Replica Race

Create multiple goroutines that query database replicas with different simulated latencies and take the fastest.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const slowReplicaProbability = 0.3

// QueryResult holds the response from a database replica.
type QueryResult struct {
	Data    string
	Replica string
	Latency time.Duration
}

// ReplicaRacer sends the same query to multiple replicas and takes the fastest.
type ReplicaRacer struct {
	replicas []string
}

func NewReplicaRacer(replicas []string) *ReplicaRacer {
	return &ReplicaRacer{replicas: replicas}
}

func simulateReplicaLatency() time.Duration {
	latency := time.Duration(5+rand.IntN(15)) * time.Millisecond
	if rand.Float64() < slowReplicaProbability {
		latency = time.Duration(200+rand.IntN(300)) * time.Millisecond
	}
	return latency
}

func (rr *ReplicaRacer) queryReplica(name string, ch chan<- QueryResult) {
	latency := simulateReplicaLatency()
	time.Sleep(latency)
	ch <- QueryResult{
		Data:    "user_profile{name:alice,id:42}",
		Replica: name,
		Latency: latency,
	}
}

func (rr *ReplicaRacer) RaceQuery() QueryResult {
	ch := make(chan QueryResult, len(rr.replicas))
	for _, replica := range rr.replicas {
		go rr.queryReplica(replica, ch)
	}
	return <-ch
}

func main() {
	fmt.Println("=== Database Replica Race ===")
	fmt.Println()

	racer := NewReplicaRacer([]string{"us-east-1", "us-west-2", "eu-west-1"})
	winner := racer.RaceQuery()

	fmt.Printf("  Winner: %s responded in %v\n", winner.Replica, winner.Latency)
	fmt.Printf("  Data: %s\n", winner.Data)
}
```

The channel is buffered so that losing goroutines can send their results without blocking, even after the consumer has moved on. Without the buffer, losers would leak.

### Verification
```bash
go run main.go
```
Expected: the fastest replica wins, varying between runs:
```
=== Database Replica Race ===

  Winner: us-west-2 responded in 8ms
  Data: user_profile{name:alice,id:42}
```

## Step 2 -- Race with Cancellation

Use `context.WithCancel` to properly cancel losing replicas and free their resources.

```go
package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	slowReplicaProbability = 0.3
	cancelSettleDelay      = 20 * time.Millisecond
)

// QueryResult holds the response from a database replica.
type QueryResult struct {
	Data    string
	Replica string
	Latency time.Duration
}

// ReplicaRacer sends the same query to multiple replicas, takes the fastest, and cancels the rest.
type ReplicaRacer struct {
	replicas []string
}

func NewReplicaRacer(replicas []string) *ReplicaRacer {
	return &ReplicaRacer{replicas: replicas}
}

func simulateReplicaLatency() time.Duration {
	latency := time.Duration(5+rand.IntN(15)) * time.Millisecond
	if rand.Float64() < slowReplicaProbability {
		latency = time.Duration(200+rand.IntN(300)) * time.Millisecond
	}
	return latency
}

func (rr *ReplicaRacer) queryWithContext(ctx context.Context, replica string) (QueryResult, error) {
	latency := simulateReplicaLatency()

	select {
	case <-time.After(latency):
		return QueryResult{
			Data:    "user_profile{name:alice,id:42}",
			Replica: replica,
			Latency: latency,
		}, nil
	case <-ctx.Done():
		fmt.Printf("  [%s] canceled (was going to take %v)\n", replica, latency)
		return QueryResult{}, ctx.Err()
	}
}

func (rr *ReplicaRacer) RaceWithCancellation() QueryResult {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan QueryResult, 1)

	for _, replica := range rr.replicas {
		go func(name string) {
			result, err := rr.queryWithContext(ctx, name)
			if err != nil {
				return
			}
			select {
			case ch <- result:
			case <-ctx.Done():
			}
		}(replica)
	}

	winner := <-ch
	cancel()
	return winner
}

func main() {
	fmt.Println("=== Replica Race with Cancellation ===")
	fmt.Println()

	racer := NewReplicaRacer([]string{"us-east-1", "us-west-2", "eu-west-1"})
	winner := racer.RaceWithCancellation()

	fmt.Printf("  Winner: %s in %v\n", winner.Replica, winner.Latency)
	fmt.Printf("  Data: %s\n\n", winner.Data)

	time.Sleep(cancelSettleDelay)
	fmt.Println("  Losing replicas were canceled and their goroutines exited cleanly.")
}
```

After receiving the first result, `cancel()` triggers `ctx.Done()` in all goroutines, causing them to exit cleanly.

### Verification
```bash
go run main.go
```
Expected: one winner, other replicas report cancellation:
```
=== Replica Race with Cancellation ===

  Winner: eu-west-1 in 7ms
  Data: user_profile{name:alice,id:42}

  [us-east-1] canceled (was going to take 245ms)
  Losing replicas were canceled and their goroutines exited cleanly.
```

## Step 3 -- Measure Tail Latency Improvement

Run multiple queries to show the statistical improvement from racing replicas.

```go
package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"
)

const (
	benchmarkIterations    = 100
	replicaCount           = 3
	slowQueryProbability   = 0.2
)

// LatencyBenchmark measures tail latency with and without replica racing.
type LatencyBenchmark struct {
	iterations   int
	replicaCount int
}

func NewLatencyBenchmark() *LatencyBenchmark {
	return &LatencyBenchmark{
		iterations:   benchmarkIterations,
		replicaCount: replicaCount,
	}
}

func simulateQuery(ctx context.Context) time.Duration {
	latency := time.Duration(5+rand.IntN(15)) * time.Millisecond
	if rand.Float64() < slowQueryProbability {
		latency = time.Duration(200+rand.IntN(300)) * time.Millisecond
	}
	select {
	case <-time.After(latency):
		return latency
	case <-ctx.Done():
		return 0
	}
}

func percentile(latencies []time.Duration, p float64) time.Duration {
	slices.Sort(latencies)
	idx := int(float64(len(latencies)) * p)
	if idx >= len(latencies) {
		idx = len(latencies) - 1
	}
	return latencies[idx]
}

func (lb *LatencyBenchmark) measureSingleReplica() []time.Duration {
	latencies := make([]time.Duration, lb.iterations)
	for i := 0; i < lb.iterations; i++ {
		latencies[i] = simulateQuery(context.Background())
	}
	return latencies
}

func (lb *LatencyBenchmark) measureRacedReplicas() []time.Duration {
	latencies := make([]time.Duration, lb.iterations)
	for i := 0; i < lb.iterations; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan time.Duration, lb.replicaCount)
		for r := 0; r < lb.replicaCount; r++ {
			go func() {
				if lat := simulateQuery(ctx); lat > 0 {
					ch <- lat
				}
			}()
		}
		latencies[i] = <-ch
		cancel()
	}
	return latencies
}

func printLatencyStats(label string, latencies []time.Duration) {
	fmt.Printf("  %s:\n", label)
	fmt.Printf("    p50:  %v\n", percentile(latencies, 0.50))
	fmt.Printf("    p90:  %v\n", percentile(latencies, 0.90))
	fmt.Printf("    p99:  %v\n", percentile(latencies, 0.99))
	fmt.Printf("    max:  %v\n\n", percentile(latencies, 1.0))
}

func (lb *LatencyBenchmark) Run() {
	fmt.Printf("=== Tail Latency Comparison (%d queries) ===\n\n", lb.iterations)

	singleLatencies := lb.measureSingleReplica()
	racedLatencies := lb.measureRacedReplicas()

	printLatencyStats("Single replica", singleLatencies)
	printLatencyStats("Three replicas (raced)", racedLatencies)

	fmt.Println("  Racing replicas dramatically reduces tail latency (p90, p99).")
	fmt.Println("  The cost is 3x the queries, but user-facing latency improves significantly.")
}

func main() {
	benchmark := NewLatencyBenchmark()
	benchmark.Run()
}
```

### Verification
```bash
go run main.go
```
Expected: p50 similar, but p90/p99 dramatically lower with racing:
```
=== Tail Latency Comparison (100 queries) ===

  Single replica:
    p50:  12ms
    p90:  298ms
    p99:  467ms
    max:  489ms

  Three replicas (raced):
    p50:  8ms
    p90:  14ms
    p99:  18ms
    max:  22ms

  Racing replicas dramatically reduces tail latency (p90, p99).
  The cost is 3x the queries, but user-facing latency improves significantly.
```

## Step 4 -- Reusable Or-Channel Function

Implement a reusable `or` function that takes multiple `<-chan struct{}` channels and returns a channel that closes when any of them closes. This is the general-purpose "first signal wins" combiner, useful for combining multiple cancellation signals.

```go
package main

import (
	"fmt"
	"time"
)

// ReplicaSignal represents a replica with a known response time.
type ReplicaSignal struct {
	Name    string
	Latency time.Duration
}

// orChannel combines multiple signal channels; closes when any input closes.
func orChannel(channels ...<-chan struct{}) <-chan struct{} {
	switch len(channels) {
	case 0:
		return nil
	case 1:
		return channels[0]
	}

	orDone := make(chan struct{})
	go func() {
		defer close(orDone)
		switch len(channels) {
		case 2:
			select {
			case <-channels[0]:
			case <-channels[1]:
			}
		default:
			select {
			case <-channels[0]:
			case <-channels[1]:
			case <-channels[2]:
			case <-orChannel(append(channels[3:], orDone)...):
			}
		}
	}()
	return orDone
}

func makeReplicaSignal(rs ReplicaSignal) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		time.Sleep(rs.Latency)
		fmt.Printf("  [%s] responded in %v\n", rs.Name, rs.Latency)
	}()
	return ch
}

func main() {
	fmt.Println("=== Or-Channel: First Replica Wins ===")
	fmt.Println()

	replicas := []ReplicaSignal{
		{"us-east-1", 300 * time.Millisecond},
		{"us-west-2", 50 * time.Millisecond},
		{"eu-west-1", 150 * time.Millisecond},
	}

	signals := make([]<-chan struct{}, len(replicas))
	for i, rs := range replicas {
		signals[i] = makeReplicaSignal(rs)
	}

	start := time.Now()
	<-orChannel(signals...)
	fmt.Printf("\n  First response received after %v\n",
		time.Since(start).Round(time.Millisecond))
}
```

### Verification
```bash
go run main.go
```
```
=== Or-Channel: First Replica Wins ===

  [us-west-2] responded in 50ms

  First response received after 50ms
```

## Common Mistakes

### Unbuffered Channel Causes Goroutine Leaks
**Wrong:**
```go
package main

import "fmt"

func main() {
	type result struct{ replica string }
	ch := make(chan result) // unbuffered
	for i := 0; i < 3; i++ {
		go func(id int) {
			ch <- result{fmt.Sprintf("replica-%d", id)}
		}(i)
	}
	winner := <-ch
	fmt.Println(winner)
	// two goroutines are stuck trying to send forever
}
```
**What happens:** The losing goroutines block on send forever because nobody reads their values.

**Fix:** Either buffer the channel to hold all results, or use context cancellation to stop losers.

### Not Canceling Losing Goroutines
**Wrong:**
```go
winner := <-ch
// forget to cancel -- losing goroutines run to completion
```
**What happens:** Losing goroutines waste CPU, memory, and database connections completing work whose result is discarded.

**Fix:** Use `context.WithCancel` and call `cancel()` after receiving the first result.

### Race Condition on the Result Channel
If multiple goroutines finish at the same instant, only one value is read. The others either block (unbuffered) or sit in the buffer (buffered). This is correct behavior -- you wanted only the first -- but make sure your channel and cancellation strategy handle it.

## Review

The or-channel pattern races N goroutines at equivalent work, reads the first
result, and cancels the rest -- three parts that must all be present, because
without the cancel the losers run to completion and waste the CPU, memory, and
connections you were trying to save. Two ways keep the losers from leaking on the
result channel: buffer it wide enough to hold every result, so a late sender
never blocks, or thread a `context.WithCancel` through the goroutines and call
`cancel()` the moment the winner arrives. The second scales better -- the buffer
just holds garbage nobody reads, while cancellation actually stops the work. The
payoff is statistical: p50 barely moves, but p90 and p99 collapse, because the
chance that all N replicas stall at the same instant is the product of small
probabilities.

The reusable form is the recursive `orChannel`, which folds any number of
`<-chan struct{}` signals into one channel that closes as soon as the first input
does -- the general "first signal wins" combiner behind every cancellation tree.
Run the exercise and each piece should prove itself: the basic race reporting a
single winning replica, the cancellation version selecting a winner while the
losers report clean cancellation, the benchmark showing p90 and p99 far lower
across three raced replicas than one, and the `orChannel` returning after roughly
the fastest signal's delay. If you can explain why the result channel must be
buffered or cancelled -- and why "first wins" means the others are correctly
discarded rather than lost -- the pattern is yours.

## Resources
- [Go Concurrency Patterns (Rob Pike)](https://www.youtube.com/watch?v=f6kdp27TYZs) -- the foundational talk on channels, select, and fan-in/fan-out.
- [Advanced Go Concurrency Patterns](https://www.youtube.com/watch?v=QDDwwePbDtw) -- Sameer Ajmani's talk where the recursive or-channel combiner comes from.
- [The Tail at Scale (Google)](https://research.google/pubs/pub40801/) -- the paper that motivates hedged, redundant requests to cut tail latency.

---

Back to [Concurrency](../../concurrency.md) | Next: [08-tee-channel-split-stream](../08-tee-channel-split-stream/08-tee-channel-split-stream.md)
