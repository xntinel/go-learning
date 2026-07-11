# Exercise 2: Channel as Synchronization

When a web server starts it must finish several initialization tasks before it can accept traffic: load configuration from disk, warm the cache, establish a database connection. Those tasks run concurrently, but the server must not start listening until every one of them is done. The naive approach -- `time.Sleep` for two seconds and hope -- is a guess, not a guarantee: on a slow machine, under load, or with a remote database, that guess fails silently and the server accepts requests before the database is connected, so users get 500s. A channel replaces the guess with a guarantee: when you receive from a done channel you know the goroutine completed, because completing is what sent the signal, and the receiver waits exactly as long as the work actually takes -- 1ms or 10 seconds.

## What you'll build

```text
02-channel-as-synchronization/
  main.go        a server startup that evolves from fragile time.Sleep to
                 done channels, struct{} signals, and result-carrying channels
```

- Build: a server startup sequence that waits deterministically for concurrent initialization to finish.
- Implement: `initComponent`/`waitForComponents` done-channel signaling, `chan struct{}` pure signals, and a result channel carrying an `InitResult` per task.
- Verify: `go run main.go`

### Why a channel is a guarantee and a sleep is a guess

A `time.Sleep` picks a fixed duration and bets that all the concurrent work finishes inside it. The bet is invisible when it holds and catastrophic when it does not -- the failure is a race, not a crash, so it surfaces only under the slow or loaded conditions you cannot reproduce on your laptop.

Receiving from a channel inverts that. The receive blocks until a sender signals, so the wait is derived from the work rather than assumed ahead of it. To wait for N goroutines you perform N receives, and the total elapsed time settles at the slowest task rather than the sum -- no more, and never less than needed.

## Step 1 -- The Fragile Sleep Version

Start with a web server startup that uses `time.Sleep` to wait for initialization. Observe how it breaks when a task takes longer than expected.

```go
package main

import (
	"fmt"
	"time"
)

const (
	configLoadTime   = 100 * time.Millisecond
	cacheWarmTime    = 200 * time.Millisecond
	dbConnectTime    = 400 * time.Millisecond
	fragileWaitGuess = 300 * time.Millisecond
)

// simulateInit pretends to initialize a component for the given duration.
func simulateInit(name string, duration time.Duration) {
	fmt.Printf("  [%s] loading...\n", name)
	time.Sleep(duration)
	fmt.Printf("  [%s] done\n", name)
}

func main() {
	fmt.Println("Server: starting initialization...")

	go simulateInit("config", configLoadTime)
	go simulateInit("cache", cacheWarmTime)
	go simulateInit("db", dbConnectTime)

	// Hope that 300ms is enough... but database needs 400ms.
	time.Sleep(fragileWaitGuess)
	fmt.Println("Server: listening on :8080 (database NOT ready!)")
}
```

The database connection needs 400ms but main only waits 300ms. The server starts accepting requests before the database is ready -- a real bug that causes errors in production.

### Verification
```bash
go run main.go
# You will see the database "done" message is missing or appears after "listening"
```

## Step 2 -- Convert to Done Channels

Replace `time.Sleep` with a done channel. Each initialization task signals completion by sending on the channel. Main receives once per task, guaranteeing all finish before the server starts listening.

```go
package main

import (
	"fmt"
	"time"
)

const componentCount = 3

// initComponent simulates initialization work and signals completion
// by sending its name on the done channel.
func initComponent(name string, duration time.Duration, done chan<- string) {
	fmt.Printf("  [%s] loading...\n", name)
	time.Sleep(duration)
	done <- name
}

// waitForComponents receives exactly count signals from the done
// channel, printing each component as it becomes ready.
func waitForComponents(done <-chan string, count int) {
	for i := 0; i < count; i++ {
		component := <-done
		fmt.Printf("  [%s] ready\n", component)
	}
}

func main() {
	fmt.Println("Server: starting initialization...")
	done := make(chan string)

	go initComponent("config", 100*time.Millisecond, done)
	go initComponent("cache", 200*time.Millisecond, done)
	go initComponent("db", 400*time.Millisecond, done)

	waitForComponents(done, componentCount)
	fmt.Println("Server: all components initialized -- listening on :8080")
}
```

### Verification
```bash
go run main.go
# Expected: all three components print "ready", then the server starts listening
#   Server: starting initialization...
#   [config] loading configuration...
#   [cache] warming cache...
#   [db] connecting to database...
#   [config] ready
#   [cache] ready
#   [db] ready
#   Server: all components initialized -- listening on :8080
```

## Step 3 -- Signal Without Data: struct{}

When a channel is used purely for signaling (the value itself does not matter), use `chan struct{}` instead of `chan bool`. It communicates intent clearly and uses zero memory per value.

```go
package main

import (
	"fmt"
	"time"
)

// signalWhenReady simulates initialization work and closes the signal
// channel when complete. Closing (instead of sending) makes the intent
// unambiguous: this is purely a synchronization signal.
func signalWhenReady(name string, duration time.Duration, signal chan struct{}) {
	time.Sleep(duration)
	fmt.Printf("  [%s] ready\n", name)
	signal <- struct{}{}
}

func main() {
	fmt.Println("Server: starting initialization...")
	configReady := make(chan struct{})
	cacheReady := make(chan struct{})
	dbReady := make(chan struct{})

	go signalWhenReady("config", 100*time.Millisecond, configReady)
	go signalWhenReady("cache", 200*time.Millisecond, cacheReady)
	go signalWhenReady("db", 400*time.Millisecond, dbReady)

	// struct{}{} carries no data -- the synchronization IS the message.
	<-configReady
	<-cacheReady
	<-dbReady
	fmt.Println("Server: all systems go -- listening on :8080")
}
```

Why `struct{}` over `bool`? Three reasons:
1. **Intent**: `chan struct{}` says "this is purely a signal" at the type level
2. **Memory**: `struct{}` is zero bytes; `bool` is one byte (negligible, but principled)
3. **Convention**: idiomatic Go uses `chan struct{}` for done/quit channels

### Verification
```bash
go run main.go
# Same deterministic behavior, but with clearer intent in the code
```

## Step 4 -- Collecting Initialization Results

In practice, initialization tasks produce results -- a config object, a cache handle, a database connection. The channel carries both the data AND the synchronization in one operation.

```go
package main

import (
	"fmt"
	"time"
)

// InitResult carries both the outcome and timing of an initialization task.
type InitResult struct {
	Component string
	Status    string
	Duration  time.Duration
}

// runInitTask simulates initialization work and reports the result
// (including elapsed time) on the results channel.
func runInitTask(component string, status string, workDuration time.Duration, results chan<- InitResult) {
	start := time.Now()
	time.Sleep(workDuration)
	results <- InitResult{
		Component: component,
		Status:    status,
		Duration:  time.Since(start),
	}
}

// collectResults receives exactly count results and prints each one.
func collectResults(results <-chan InitResult, count int) {
	for i := 0; i < count; i++ {
		r := <-results
		fmt.Printf("  [%s] %s (took %v)\n",
			r.Component, r.Status, r.Duration.Round(time.Millisecond))
	}
}

func main() {
	fmt.Println("Server: starting initialization...")
	results := make(chan InitResult)

	go runInitTask("config", "loaded 47 settings", 100*time.Millisecond, results)
	go runInitTask("cache", "warmed 1200 entries", 200*time.Millisecond, results)
	go runInitTask("database", "connected to postgres://prod:5432", 400*time.Millisecond, results)

	collectResults(results, 3)
	fmt.Println("Server: ready to accept traffic")
}
```

### Verification
```bash
go run main.go
# Expected: each component reports its status and timing, then server starts
```

## Step 5 -- Total Time Equals the Slowest Task

The power of concurrent initialization: total elapsed time equals the slowest task, not the sum. With channels, you wait exactly as long as the slowest component -- no more, no less.

```go
package main

import (
	"fmt"
	"time"
)

const expectedSequentialTime = 780 * time.Millisecond

// StartupTask defines a named initialization task with its expected duration.
type StartupTask struct {
	Name     string
	Duration time.Duration
}

// runTask simulates an initialization task and signals completion on the done channel.
func runTask(task StartupTask, done chan<- struct{}) {
	fmt.Printf("  [init] %s (%v)\n", task.Name, task.Duration)
	time.Sleep(task.Duration)
	done <- struct{}{}
}

// launchAllTasks starts every task concurrently and waits for all to finish.
func launchAllTasks(tasks []StartupTask) {
	done := make(chan struct{})

	for _, task := range tasks {
		go runTask(task, done)
	}

	for range tasks {
		<-done
	}
}

func main() {
	tasks := []StartupTask{
		{"load TLS certs", 100 * time.Millisecond},
		{"parse config", 50 * time.Millisecond},
		{"warm cache", 200 * time.Millisecond},
		{"connect to database", 400 * time.Millisecond},
		{"register health check", 30 * time.Millisecond},
	}

	start := time.Now()
	launchAllTasks(tasks)
	elapsed := time.Since(start).Round(10 * time.Millisecond)

	fmt.Printf("Total startup time: %v (parallel -- not %v sequential)\n",
		elapsed, expectedSequentialTime)
}
```

### Verification
```bash
go run main.go
# Expected: Total startup time ~400ms (slowest task), NOT ~780ms (sum of all)
```

With `time.Sleep` you would have to guess the maximum. With channels, the wait is exactly as long as the slowest task requires.

## Verification

Run your programs and confirm:
1. The sleep-based version misses the database initialization
2. The channel-based version always waits for all components
3. The total time is determined by the slowest task, not the sum

## Common Mistakes

### Mismatched Send/Receive Count

**Wrong:**
```go
package main

import "fmt"

func main() {
    done := make(chan struct{})
    for i := 0; i < 5; i++ {
        go func() { done <- struct{}{} }()
    }
    for i := 0; i < 3; i++ { // only waiting for 3!
        <-done
    }
    fmt.Println("done") // 2 goroutines still running (or trying to send)
}
```

**What happens:** Main exits while 2 goroutines are still running. You lose work.

**Correct:** Always match the number of receives to the number of goroutines launched:
```go
for i := 0; i < 5; i++ {
    <-done // receive 5 times for 5 goroutines
}
```

### Sending Before the Work Is Done

**Wrong:**
```go
package main

import (
    "fmt"
    "time"
)

func main() {
    done := make(chan struct{})
    go func() {
        done <- struct{}{} // signal sent BEFORE work!
        time.Sleep(1 * time.Second)
        fmt.Println("database connected")
    }()
    <-done
    fmt.Println("server: accepting traffic") // database is NOT connected yet!
}
```

**What happens:** The signal arrives before the work completes. The server starts accepting traffic with no database connection.

**Correct:** Always send the done signal as the LAST operation in the goroutine.

## Review

The exercise turns `time.Sleep`-based startup, which guesses at a duration and fails silently when the guess is wrong, into deterministic synchronization where a receive blocks until the sender signals. The done-channel pattern carries that: each goroutine sends as its last operation, and to wait for N goroutines you perform N receives, so the total concurrent time settles at the slowest task rather than the sum. When the value itself is irrelevant, `chan struct{}` states "this is purely a signal" at the type level and costs zero bytes per value; when the work produces something -- a config object, a connection handle -- a result channel folds the data transfer and the synchronization into the same operation.

You should be able to say why `time.Sleep` fails as a synchronization mechanism at all -- that it assumes a timing it cannot guarantee -- and what goes wrong if a goroutine signals on the done channel before its work is actually finished, which lets the server accept traffic against a component that is not ready. You should also be able to justify reaching for `chan struct{}` over `chan string` whenever the signal is the message and no payload needs to travel with it.

## Resources
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the principle behind using a channel signal instead of shared state and a sleep.
- [Go Blog: Go Concurrency Patterns](https://go.dev/blog/concurrency-patterns) -- done channels, signaling, and coordinating multiple goroutines.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- the mechanics of blocking sends and receives that make the wait deterministic.

---

Back to [Concurrency](../../concurrency.md) | Next: [03-buffered-channels](../03-buffered-channels/03-buffered-channels.md)
