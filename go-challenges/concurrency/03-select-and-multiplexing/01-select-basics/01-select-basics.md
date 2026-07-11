# Exercise 1: Select Basics

A service in production rarely depends on one backend. It talks to a database, a
cache, and an external API, and each responds at its own pace. Check them one at
a time -- database, then cache, then API -- and a slow database makes the whole
request wait even though the cache replied long ago. The `select` statement is
Go's multiplexer for channel operations: it blocks until one of its cases can
proceed, runs that one, and when several are ready it picks among them at random
so no channel can starve the others.

## What you'll build

```text
01-select-basics/
  main.go        a service-health monitor that reacts to whichever backend
                 responds first, plus a fairness experiment over ready channels
```

- Build: a service-health monitor that reacts to whichever backend answers first.
- Implement: `awaitFirstResponse` over two channels, `awaitFirstOfThree`, a fairness experiment that counts random wins, a collect-all loop, and a `select` with send cases.
- Verify: `go run main.go`

### Why select is a switch for channels

The `select` statement is Go's multiplexer for channel operations. It blocks
until one of its cases can proceed, then executes that case. If multiple cases
are ready simultaneously, it picks one at random with uniform probability. This
randomness is intentional: it prevents starvation and ensures no single channel
monopolizes the goroutine's attention.

Think of `select` as a `switch` statement for channels. Where `switch`
evaluates values, `select` evaluates communication readiness. It is the
foundation for almost every concurrent pattern in Go: timeouts, cancellation,
fan-in, heartbeats, and priority handling.

## Step 1 -- Monitor Two Microservices

Create channels representing health check responses from a database and a cache. Each service reports on its own channel at its own pace. Use `select` to react to whichever service responds first.

```go
package main

import (
	"fmt"
	"time"
)

func simulateHealthCheck(serviceName string, latency time.Duration, report chan<- string) {
	time.Sleep(latency)
	report <- fmt.Sprintf("%s: healthy (%v)", serviceName, latency)
}

func awaitFirstResponse(dbHealth, cacheHealth <-chan string) string {
	// select blocks until one service reports.
	// The cache responds in 50ms, the database in 150ms — cache wins.
	// Without select, we would block on the database and miss
	// the fact that the cache responded 100ms earlier.
	select {
	case result := <-dbHealth:
		return result
	case result := <-cacheHealth:
		return result
	}
}

func main() {
	dbHealth := make(chan string)
	cacheHealth := make(chan string)

	go simulateHealthCheck("db", 150*time.Millisecond, dbHealth)
	go simulateHealthCheck("cache", 50*time.Millisecond, cacheHealth)

	fmt.Println("first response:", awaitFirstResponse(dbHealth, cacheHealth))
}
```

Without `select`, a sequential check (`<-dbHealth` then `<-cacheHealth`) would block on the database for 150ms, even though the cache replied in 50ms. With `select`, the goroutine reacts to whichever channel is ready first.

### Verification
Run the program. You should see:
```
first response: cache: healthy (50ms)
```
Swap the sleep durations (database=50ms, cache=150ms) and confirm the output changes to the database message.

## Step 2 -- Monitor Three Services

Extend the monitor to include an external API. `select` works with any number of cases. The first service to respond wins.

```go
package main

import (
	"fmt"
	"time"
)

func simulateHealthCheck(serviceName string, latency time.Duration, report chan<- string) {
	time.Sleep(latency)
	report <- fmt.Sprintf("%s: healthy (%v)", serviceName, latency)
}

func awaitFirstOfThree(dbHealth, cacheHealth, apiHealth <-chan string) string {
	select {
	case result := <-dbHealth:
		return result
	case result := <-cacheHealth:
		return result
	case result := <-apiHealth:
		return result
	}
}

func main() {
	dbHealth := make(chan string)
	cacheHealth := make(chan string)
	apiHealth := make(chan string)

	go simulateHealthCheck("db", 200*time.Millisecond, dbHealth)
	go simulateHealthCheck("cache", 80*time.Millisecond, cacheHealth)
	go simulateHealthCheck("api", 120*time.Millisecond, apiHealth)

	fmt.Println("first response:", awaitFirstOfThree(dbHealth, cacheHealth, apiHealth))
}
```

### Verification
```
first response: cache: healthy (80ms)
```
The cache responds fastest (80ms), so it wins. The other two goroutines complete after `main` exits; their messages are never received.

## Step 3 -- Observe Random Selection When Services Tie

When two services respond at exactly the same time (both channels are ready), `select` picks one at random with uniform probability. Use buffered channels to simulate simultaneous responses.

```go
package main

import "fmt"

const totalTrials = 10

func pickFromReadyChannels(dbHealth, cacheHealth <-chan string) string {
	select {
	case result := <-dbHealth:
		return result
	case result := <-cacheHealth:
		return result
	}
}

func runFairnessExperiment(trials int) (dbWins, cacheWins int) {
	for trial := 0; trial < trials; trial++ {
		dbHealth := make(chan string, 1)
		cacheHealth := make(chan string, 1)
		dbHealth <- "db: healthy"
		cacheHealth <- "cache: healthy"

		result := pickFromReadyChannels(dbHealth, cacheHealth)
		fmt.Printf("trial %d: %s\n", trial, result)

		if result == "db: healthy" {
			dbWins++
		} else {
			cacheWins++
		}
	}
	return dbWins, cacheWins
}

func main() {
	dbWins, cacheWins := runFairnessExperiment(totalTrials)
	fmt.Printf("db wins: %d, cache wins: %d\n", dbWins, cacheWins)
}
```

Since both channels already contain a value, both cases are ready every time. The runtime picks one uniformly at random.

### Verification
Run the program multiple times. You should see a roughly 50/50 split:
```
trial 0: cache: healthy
trial 1: db: healthy
...
db wins: 4, cache wins: 6
```
The exact numbers vary each run. This randomness is critical: it prevents one service from starving others for attention.

## Step 4 -- Collect All Health Reports

In a real service monitor, you want results from ALL services, not just the first. Use a loop with `select` to drain all channels.

```go
package main

import (
	"fmt"
	"time"
)

const serviceCount = 3

func simulateHealthCheck(serviceName string, latency time.Duration, report chan<- string) {
	time.Sleep(latency)
	report <- fmt.Sprintf("%s: healthy (%v)", serviceName, latency)
}

func collectAllReports(dbHealth, cacheHealth, apiHealth <-chan string, count int) {
	for i := 0; i < count; i++ {
		select {
		case result := <-dbHealth:
			fmt.Printf("report %d: %s\n", i+1, result)
		case result := <-cacheHealth:
			fmt.Printf("report %d: %s\n", i+1, result)
		case result := <-apiHealth:
			fmt.Printf("report %d: %s\n", i+1, result)
		}
	}
}

func main() {
	dbHealth := make(chan string)
	cacheHealth := make(chan string)
	apiHealth := make(chan string)

	go simulateHealthCheck("db", 100*time.Millisecond, dbHealth)
	go simulateHealthCheck("cache", 30*time.Millisecond, cacheHealth)
	go simulateHealthCheck("api", 70*time.Millisecond, apiHealth)

	collectAllReports(dbHealth, cacheHealth, apiHealth, serviceCount)
	fmt.Println("all services reported")
}
```

### Verification
```
report 1: cache: healthy (30ms)
report 2: api: healthy (70ms)
report 3: db: healthy (100ms)
```
Each `select` picks whichever channel has data ready. The loop ensures all three reports are collected in order of response time.

## Step 5 -- Select with Send Cases

`select` is not limited to receives. Send operations are valid cases too. This is useful when a health check result needs to be forwarded to whichever downstream consumer is ready first.

```go
package main

import "fmt"

func routeToFirstAvailable(alertCh chan<- string, logCh chan<- string, message string) string {
	select {
	case alertCh <- message:
		return "alert system"
	case logCh <- message:
		return "log system"
	}
}

func main() {
	alertCh := make(chan string, 1) // Buffered — ready to accept
	logCh := make(chan string)      // Unbuffered — no reader waiting

	destination := routeToFirstAvailable(alertCh, logCh, "db: unhealthy")
	fmt.Println("sent to", destination)
	fmt.Println("alert channel has:", <-alertCh)
}
```

### Verification
```
sent to alert system
alert channel has: db: unhealthy
```
The buffered channel has space, so its send case succeeds immediately. The unbuffered channel has no receiver, so it blocks and loses.

## Common Mistakes

### 1. Assuming Case Order Matters
Unlike `switch`, the position of cases in `select` has zero effect on priority. Go's runtime uses a pseudo-random shuffle to guarantee fairness. This code does NOT prioritize the database check:

```go
package main

import "fmt"

func main() {
	dbHealth := make(chan string, 1)
	cacheHealth := make(chan string, 1)
	dbHealth <- "db: ok"
	cacheHealth <- "cache: ok"

	// Case order does NOT create priority. Both are equally likely.
	select {
	case report := <-dbHealth:
		fmt.Println(report) // NOT guaranteed to run first
	case report := <-cacheHealth:
		fmt.Println(report)
	}
}
```

### 2. Forgetting that Select Blocks Without Default
If no case is ready and there is no `default`, the goroutine blocks forever. This is a common source of deadlocks:

```go
package main

func main() {
	ch := make(chan string) // nobody sends

	// DEADLOCK: no case is ready, no default, blocks forever
	select {
	case result := <-ch:
		_ = result
	}
}
```

Expected output:
```
fatal error: all goroutines are asleep - deadlock!
```

### 3. Using Select with a Single Case
A `select` with one case is equivalent to a plain channel operation. It compiles but adds no value and obscures intent:

```go
// Unnecessary — identical to: result := <-healthCh
select {
case result := <-healthCh:
    processResult(result)
}
```

## Review

The `select` statement multiplexes across multiple channel operations. It blocks
until at least one case is ready, then executes it. When multiple cases are
ready simultaneously, the runtime picks one uniformly at random -- a deliberate
fairness guarantee that prevents any channel from starving the others -- and the
position of a case in the source has no effect on that choice. Cases can be
receives or sends, and a `select` with no ready case and no `default` blocks
until one becomes ready. In the service-monitor scenario this is what lets you
react to whichever backend responds first instead of blocking on a slow
dependency while faster ones sit idle, which is why `select` is the building
block under timeouts, cancellation, fan-in, and heartbeats.

You should be able to answer, without rereading: when does `select` block and
when does it proceed immediately? What happens when several cases are ready at
once, and why does case order not decide it? And you should be able to write a
`select` that listens on three or more channels, and one that mixes send and
receive cases in the same statement.

## Resources
- [Go Spec: Select statements](https://go.dev/ref/spec#Select_statements) -- the precise blocking and random-choice rules for `select`.
- [Effective Go: Multiplexing](https://go.dev/doc/effective_go#multiplexing) -- worked examples of `select` combining several channels.
- [A Tour of Go: Select](https://go.dev/tour/concurrency/5) -- the two-minute interactive introduction to `select` and its `default`.

---

Back to [Concurrency](../../concurrency.md) | Next: [02-select-with-default](../02-select-with-default/02-select-with-default.md)
