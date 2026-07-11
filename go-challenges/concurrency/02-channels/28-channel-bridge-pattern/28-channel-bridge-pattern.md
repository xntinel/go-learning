# Exercise 28: Bridge Channel (Channel of Channels Flattened)

A monitoring system collects logs from dozens of microservices. Each service
produces a finite batch of log lines on its own channel, and services start and
stop independently -- a new deployment spins up a fresh log channel, a
scaled-down instance closes its own. The consumer that writes logs to storage
should not have to know how many sources exist or when they appear; it just
wants a single stream of log lines. The bridge pattern gives it exactly that,
and its whole reason for existing is a lifecycle question the naive approach
cannot answer cleanly: who closes the merged output channel, and when?

## What you'll build

```text
28-channel-bridge-pattern/
  main.go        a bridge goroutine that flattens a channel-of-channels of
                 LogLines into one output stream: sequential, concurrent, cancellable
```

- Build: a bridge goroutine that flattens a channel-of-channels of log lines into a single output stream.
- Implement: a sequential `bridge` (nested `range` loops), a `concurrentBridge` (one reader goroutine per source plus a `sync.WaitGroup`), and a `cancellableBridge` that stops early on a `done` channel.
- Verify: `go run main.go`, and `go run -race main.go` on the concurrent and cancellable steps.

### Why a single bridge owns the output

The naive approach -- launching a goroutine per source that writes to a shared
output channel -- works until you need to close the output channel. Who closes
it? You need a coordinator that knows when all sources are done. If sources
arrive dynamically, the coordinator becomes complex.

The bridge pattern solves this elegantly: a single goroutine reads from a
"registry" channel that carries per-source channels. For each source channel it
receives, the bridge reads all values from that source. When a source closes,
the bridge moves to the next one from the registry. When the registry itself
closes, the bridge knows no more sources will arrive, finishes any remaining
source, and closes the output. One goroutine, clean lifecycle, no coordination
overhead.

## Step 1 -- Bridge Over a Fixed Sequence of Sources

Start with the simplest case: three log sources are known upfront, delivered through a registry channel. The bridge goroutine reads from registry, drains each source in order, and writes every log line to a single output channel.

```go
package main

import "fmt"

const logPrefix = "bridge"

// LogLine represents a single log entry from a source.
type LogLine struct {
	Source  string
	Message string
}

// bridge reads source channels from registry and flattens all LogLines
// into the returned output channel. It closes output when registry is
// exhausted and the last source is drained.
func bridge(registry <-chan <-chan LogLine) <-chan LogLine {
	out := make(chan LogLine)
	go func() {
		defer close(out)
		for source := range registry {
			for line := range source {
				out <- line
			}
		}
	}()
	return out
}

// newLogSource creates a channel that emits n log lines then closes.
func newLogSource(name string, messages []string) <-chan LogLine {
	ch := make(chan LogLine)
	go func() {
		defer close(ch)
		for _, msg := range messages {
			ch <- LogLine{Source: name, Message: msg}
		}
	}()
	return ch
}

func main() {
	registry := make(chan (<-chan LogLine))

	go func() {
		defer close(registry)
		registry <- newLogSource("auth-svc", []string{
			"user login attempt",
			"login successful",
		})
		registry <- newLogSource("order-svc", []string{
			"order created ORD-42",
			"payment processed",
			"order confirmed",
		})
		registry <- newLogSource("inventory-svc", []string{
			"stock reserved SKU-100",
		})
	}()

	for line := range bridge(registry) {
		fmt.Printf("[%s] %s: %s\n", logPrefix, line.Source, line.Message)
	}
	fmt.Println("all sources drained")
}
```

Key observations:
- The bridge uses two nested `range` loops: outer iterates source channels from registry, inner drains each source
- When a source channel closes, the inner `range` exits and the bridge picks up the next source
- When the registry closes, the outer `range` exits, the deferred `close(out)` runs, and the consumer's `range` exits
- The output channel is created and closed by the bridge goroutine -- single ownership

### Verification
```bash
go run main.go
```
Expected output:
```
[bridge] auth-svc: user login attempt
[bridge] auth-svc: login successful
[bridge] order-svc: order created ORD-42
[bridge] order-svc: payment processed
[bridge] order-svc: order confirmed
[bridge] inventory-svc: stock reserved SKU-100
all sources drained
```

## Step 2 -- Bridge with Dynamically Arriving Sources

In production, sources do not arrive all at once. New services register their log channels over time. Simulate this with staggered source registration.

```go
package main

import (
	"fmt"
	"time"
)

const (
	registrationDelay = 100 * time.Millisecond
	sourceDelay       = 30 * time.Millisecond
)

type LogLine struct {
	Source    string
	Message  string
	Received time.Time
}

func bridge(registry <-chan <-chan LogLine) <-chan LogLine {
	out := make(chan LogLine)
	go func() {
		defer close(out)
		for source := range registry {
			for line := range source {
				line.Received = time.Now()
				out <- line
			}
		}
	}()
	return out
}

func newDelayedSource(name string, messages []string, delay time.Duration) <-chan LogLine {
	ch := make(chan LogLine)
	go func() {
		defer close(ch)
		for _, msg := range messages {
			time.Sleep(delay)
			ch <- LogLine{Source: name, Message: msg}
		}
	}()
	return ch
}

func main() {
	registry := make(chan (<-chan LogLine))
	epoch := time.Now()

	go func() {
		defer close(registry)

		registry <- newDelayedSource("auth-svc", []string{
			"startup complete",
			"ready to serve",
		}, sourceDelay)

		time.Sleep(registrationDelay)
		registry <- newDelayedSource("order-svc", []string{
			"connected to db",
			"processing backlog",
			"backlog cleared",
		}, sourceDelay)

		time.Sleep(registrationDelay)
		registry <- newDelayedSource("cache-svc", []string{
			"warming cache",
			"cache ready",
		}, sourceDelay)
	}()

	for line := range bridge(registry) {
		elapsed := line.Received.Sub(epoch).Round(time.Millisecond)
		fmt.Printf("+%v [%s] %s\n", elapsed, line.Source, line.Message)
	}
	fmt.Println("all dynamic sources drained")
}
```

Notice the timestamps: auth-svc lines appear first, then after a pause order-svc lines appear, then cache-svc. The bridge processes sources strictly in registration order -- it fully drains auth-svc before starting order-svc, even if order-svc's channel already has data waiting.

### Verification
```bash
go run main.go
```
Expected output (timestamps approximate):
```
+30ms [auth-svc] startup complete
+60ms [auth-svc] ready to serve
+160ms [order-svc] connected to db
+190ms [order-svc] processing backlog
+220ms [order-svc] backlog cleared
+310ms [cache-svc] warming cache
+340ms [cache-svc] cache ready
all dynamic sources drained
```

## Step 3 -- Bridge with Concurrent Source Consumption

The sequential bridge from Step 2 has a latency problem: if auth-svc is slow, order-svc lines pile up in their channel. For a real log collector, we want to read from all registered sources concurrently and merge their output into one stream.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	concSourceDelay   = 50 * time.Millisecond
	concRegDelay      = 80 * time.Millisecond
)

type LogLine struct {
	Source  string
	Message string
}

// concurrentBridge spawns a reader goroutine for each source channel
// received from registry. All readers write to a shared output channel.
// Output closes only when registry is closed AND all readers are done.
func concurrentBridge(registry <-chan <-chan LogLine) <-chan LogLine {
	out := make(chan LogLine)
	var wg sync.WaitGroup

	go func() {
		for source := range registry {
			wg.Add(1)
			go func(src <-chan LogLine) {
				defer wg.Done()
				for line := range src {
					out <- line
				}
			}(source)
		}
		wg.Wait()
		close(out)
	}()

	return out
}

func newSource(name string, messages []string, delay time.Duration) <-chan LogLine {
	ch := make(chan LogLine)
	go func() {
		defer close(ch)
		for _, msg := range messages {
			time.Sleep(delay)
			ch <- LogLine{Source: name, Message: msg}
		}
	}()
	return ch
}

func main() {
	registry := make(chan (<-chan LogLine))
	epoch := time.Now()

	go func() {
		defer close(registry)
		registry <- newSource("auth-svc", []string{
			"startup", "ready", "first request",
		}, concSourceDelay)

		time.Sleep(concRegDelay)
		registry <- newSource("order-svc", []string{
			"db connected", "processing", "done",
		}, concSourceDelay)

		time.Sleep(concRegDelay)
		registry <- newSource("cache-svc", []string{
			"warming", "hot",
		}, concSourceDelay)
	}()

	for line := range concurrentBridge(registry) {
		elapsed := time.Since(epoch).Round(10 * time.Millisecond)
		fmt.Printf("+%v [%s] %s\n", elapsed, line.Source, line.Message)
	}
	fmt.Println("all concurrent sources drained")
}
```

Key differences from the sequential bridge:
- Each source gets its own reader goroutine writing to the shared `out` channel
- `sync.WaitGroup` tracks active readers so `out` closes only when all are done
- Log lines from different sources interleave based on arrival time, not registration order
- The outer goroutine reads registry and spawns readers; it waits for all readers before closing output

### Verification
```bash
go run -race main.go
```
Expected output (lines from different sources interleave):
```
+50ms [auth-svc] startup
+100ms [auth-svc] ready
+130ms [order-svc] db connected
+150ms [auth-svc] first request
+180ms [order-svc] processing
+210ms [cache-svc] warming
+230ms [order-svc] done
+260ms [cache-svc] hot
all concurrent sources drained
```
Note: exact interleaving depends on scheduling, but lines from multiple sources overlap in time.

## Step 4 -- Clean Shutdown with Done Channel

Add cancellation support so the log collector can stop the bridge before all sources are drained -- essential for graceful server shutdown.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const shutdownSourceDelay = 40 * time.Millisecond

type LogLine struct {
	Source  string
	Message string
}

// cancellableBridge supports early termination via the done channel.
func cancellableBridge(done <-chan struct{}, registry <-chan <-chan LogLine) <-chan LogLine {
	out := make(chan LogLine)
	var wg sync.WaitGroup

	go func() {
		defer func() {
			wg.Wait()
			close(out)
		}()

		for {
			select {
			case <-done:
				return
			case source, ok := <-registry:
				if !ok {
					return
				}
				wg.Add(1)
				go func(src <-chan LogLine) {
					defer wg.Done()
					for {
						select {
						case <-done:
							return
						case line, ok := <-src:
							if !ok {
								return
							}
							select {
							case out <- line:
							case <-done:
								return
							}
						}
					}
				}(source)
			}
		}
	}()

	return out
}

func newSource(name string, count int) <-chan LogLine {
	ch := make(chan LogLine)
	go func() {
		defer close(ch)
		for i := 1; i <= count; i++ {
			time.Sleep(shutdownSourceDelay)
			ch <- LogLine{
				Source:  name,
				Message: fmt.Sprintf("line %d/%d", i, count),
			}
		}
	}()
	return ch
}

func main() {
	done := make(chan struct{})
	registry := make(chan (<-chan LogLine))

	go func() {
		defer close(registry)
		registry <- newSource("auth-svc", 20)
		registry <- newSource("order-svc", 20)
		registry <- newSource("cache-svc", 20)
	}()

	out := cancellableBridge(done, registry)

	collected := 0
	cutoff := 8
	for line := range out {
		collected++
		fmt.Printf("#%d [%s] %s\n", collected, line.Source, line.Message)
		if collected >= cutoff {
			fmt.Println("--- shutdown signal sent ---")
			close(done)
		}
	}
	fmt.Printf("collected %d lines before shutdown (sources had 60 total)\n", collected)
}
```

Every `select` in `cancellableBridge` checks `done`. When done closes:
- The registry reader stops accepting new sources
- Each source reader stops reading and exits
- The `wg.Wait()` waits for all readers to finish, then closes `out`
- The consumer's `range` over `out` terminates

### Verification
```bash
go run -race main.go
```
Expected output:
```
#1 [auth-svc] line 1/20
#2 [auth-svc] line 2/20
...
#8 [...] line .../20
--- shutdown signal sent ---
collected 8 lines before shutdown (sources had 60 total)
```
The bridge stops after approximately 8 lines even though 60 were available. No race warnings.

## Common Mistakes

### Closing the Output Channel from Outside the Bridge
**What happens:** If the consumer closes the output channel, the bridge goroutine panics on the next `out <- line` send. Even worse, if multiple goroutines (concurrent bridge) are writing to `out`, any of them can hit the panic.

**Fix:** The bridge goroutine that creates the channel is the sole owner. It closes `out` via `defer close(out)`. No external code should close it.

### Forgetting WaitGroup in Concurrent Bridge
**What happens:** Without a `WaitGroup`, the outer goroutine closes `out` as soon as the registry is drained -- but source readers may still be running. This causes either a send on closed channel (panic) or lost log lines.

**Fix:** Track every reader goroutine with `wg.Add(1)` before launch and `defer wg.Done()` inside. Close `out` only after `wg.Wait()`.

### Missing Done Check on Output Send
**What happens:** In the cancellable bridge, if you check `done` on the source read but not on the `out <-` send, the goroutine blocks if the consumer has stopped reading. The goroutine leaks.

**Fix:** Wrap every channel operation in a `select` with a `done` case:
```go
select {
case out <- line:
case <-done:
    return
}
```

## Review

The bridge flattens a channel-of-channels into one output stream, and the three
versions trace a single design tension. The sequential bridge drains sources in
registration order with nested `range` loops -- simple, but a slow early source
makes later ones pile up. The concurrent bridge spawns a reader goroutine per
source so output interleaves by arrival time, and it pays for that with a
`sync.WaitGroup`: because several goroutines now write to the shared output, it
must close that channel only after `wg.Wait()` returns, never the moment the
registry drains. The cancellable bridge threads a `done` channel through every
`select` -- including the `out <- line` send -- so early termination cannot leave
a reader blocked and leaking. Underneath all three is the ownership rule: the
goroutine that creates the output channel is the only one allowed to close it,
which is why no consumer ever closes `out`.

To confirm you can extend the pattern rather than just read it, modify the
concurrent bridge so at most three source readers run at once, using a buffered
channel as a counting semaphore, and add a per-reader counter that reports how
many lines each forwarded before shutdown.

## Resources

- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the fan-in and merge slides this bridge generalizes to dynamic sources.
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- the done-channel idiom that drives the cancellable bridge's clean teardown.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- the range-over-channel and closing semantics the nested loops rely on.
- [Concurrency in Go (Katherine Cox-Buday) -- Bridge Channel](https://www.oreilly.com/library/view/concurrency-in-go/9781491941294/) -- the book chapter that names and develops this exact pattern.

---

Back to [Concurrency](../../concurrency.md) | Next: [29-channel-load-balancer](../29-channel-load-balancer/29-channel-load-balancer.md)
