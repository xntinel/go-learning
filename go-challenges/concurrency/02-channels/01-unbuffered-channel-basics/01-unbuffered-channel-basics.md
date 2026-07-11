# Exercise 1: Unbuffered Channel Basics

Imagine a print queue in an office. When someone sends a document to the
printer, they wait until the printer picks it up before they can send the next
one -- there is no tray to hold pending documents, so the handoff is direct,
person to printer. Unbuffered channels work exactly this way, and that forced
rendezvous is the foundation of Go's concurrency model: it is what lets two
goroutines exchange a value with no lock, no race, and no guessing about whether
the other side actually received it.

## What you'll build

```text
01-unbuffered-channel-basics/
  main.go        a print queue modeled with an unbuffered channel: main hands
                 jobs to a worker goroutine, one synchronous handoff at a time
```

- Build: a print-queue model where the main goroutine hands jobs to a worker over an unbuffered channel.
- Implement: `processJob`, `submitJob`, and `printWorker` over a `chan string`, plus a typed `PrintJob` struct sent through a channel.
- Verify: `go run main.go`.

### Why the handoff is a rendezvous

A send blocks until a receiver is ready, and a receive blocks until a sender is
ready. This creates a *rendezvous point*: both goroutines must arrive at the
channel operation at the same time for the exchange to happen. The sender knows
the receiver has the data the moment the send completes. No race conditions, no
data corruption -- just a clean, synchronous handoff.

This forced synchronization is the foundation of Go's concurrency model. Without
it, goroutines would be isolated workers with no safe way to share results.

## Step 1 -- A Print Queue: Sending Jobs to a Worker

Create an unbuffered channel to model a print queue. The main goroutine sends print jobs, and a worker goroutine processes them one at a time. Because the channel is unbuffered, main cannot send the next job until the worker has received the current one.

```go
package main

import "fmt"

// processJob receives a single print job from the queue and prints it.
// The queue parameter is receive-only: this function can only consume jobs.
func processJob(queue <-chan string) {
	job := <-queue
	fmt.Println("Printer: processing", job)
}

func main() {
	// make(chan T) creates an unbuffered channel of type T.
	// Zero capacity means every send blocks until a receiver is ready.
	printQueue := make(chan string)

	go processJob(printQueue)

	// Send a print job. This blocks until the worker calls <-printQueue.
	printQueue <- "invoice-2024.pdf"
	fmt.Println("Main: job accepted by printer")
}
```

Key observations:
- `make(chan string)` creates a channel that transports `string` values
- `printQueue <- value` is a **send** -- the arrow points *into* the channel
- `<-printQueue` is a **receive** -- the arrow points *out of* the channel
- `main` blocks at the send until the worker goroutine receives

### Verification
```bash
go run main.go
# Expected:
#   Printer: processing invoice-2024.pdf
#   Main: job accepted by printer
```

## Step 2 -- Observe the Synchronous Handoff

The rendezvous property means the sender is suspended until a receiver is ready. This program proves it with timestamps: the main goroutine delays its receive, and the worker is stuck waiting the entire time.

```go
package main

import (
	"fmt"
	"time"
)

const printerBusyDelay = 500 * time.Millisecond
const drainDelay = 50 * time.Millisecond

// submitJob sends a document into the print queue, blocking until
// the printer is ready to accept it.
func submitJob(queue chan<- string, document string) {
	fmt.Println("Worker: ready to send job to printer (will block here)")
	queue <- document
	fmt.Println("Worker: job handed off to printer successfully")
}

// acceptJob simulates a busy printer that becomes ready after a delay,
// then receives the next job from the queue.
func acceptJob(queue <-chan string, busyDuration time.Duration) string {
	time.Sleep(busyDuration)
	fmt.Println("Printer: now ready to accept job")
	return <-queue
}

func main() {
	printQueue := make(chan string)

	go submitJob(printQueue, "quarterly-report.pdf")

	job := acceptJob(printQueue, printerBusyDelay)
	fmt.Printf("Printer: received %q\n", job)

	time.Sleep(drainDelay)
}
```

You will see that `"Worker: ready to send"` prints immediately, then after the 500ms delay, `"Printer: now ready"` prints, and only then does the worker's `"job handed off"` appear. The worker was blocked on the send the entire time -- no guessing, no polling.

### Verification
```bash
go run main.go
# Expected output order:
#   Worker: ready to send job to printer (will block here)
#   Printer: now ready to accept job
#   Printer: received "quarterly-report.pdf"
#   Worker: job handed off to printer successfully
```

## Step 3 -- Processing Multiple Jobs Sequentially

Each send/receive pair is a separate synchronization point. Three print jobs flow through the same channel, one at a time, in FIFO order. The worker cannot receive job 2 until job 1 has been handed off.

```go
package main

import (
	"fmt"
	"time"
)

const printDuration = 100 * time.Millisecond
const finalDrainDelay = 150 * time.Millisecond

// printWorker receives exactly jobCount jobs from the queue,
// simulating a print operation for each one.
func printWorker(queue <-chan string, jobCount int) {
	for i := 0; i < jobCount; i++ {
		job := <-queue
		fmt.Printf("Printer: printing %s...\n", job)
		time.Sleep(printDuration)
		fmt.Printf("Printer: finished %s\n", job)
	}
}

// submitJobs sends each job to the print queue. Each send blocks
// until the worker receives, proving the synchronous handoff.
func submitJobs(queue chan<- string, jobs []string) {
	for _, job := range jobs {
		queue <- job
		fmt.Printf("Main: %s accepted by printer\n", job)
	}
}

func main() {
	printQueue := make(chan string)
	jobs := []string{"invoice.pdf", "contract.pdf", "memo.pdf"}

	go printWorker(printQueue, len(jobs))
	submitJobs(printQueue, jobs)

	time.Sleep(finalDrainDelay)
}
```

### Verification
```bash
go run main.go
# Expected: jobs are printed sequentially, each send blocks until the worker picks it up
```

## Step 4 -- Deadlock Detection

If you receive from a channel with no sender (or vice versa), all goroutines are stuck. Go's runtime detects this and panics with a deadlock error. This acts as a safety net during development.

```go
package main

func main() {
    printQueue := make(chan string)
    <-printQueue // no goroutine will ever send -- deadlock!
}
```

### Verification
```bash
go run main.go
# Expected: fatal error: all goroutines are asleep - deadlock!
```

Common causes of deadlock:
- Receiving without a corresponding send
- Sending without a corresponding receive
- Sending and receiving in the same goroutine on an unbuffered channel

## Step 5 -- Channels Are Strongly Typed: Real Job Structs

In production, you rarely send raw strings. Channels carry any Go type -- structs, errors, slices, even other channels. Here, a `PrintJob` struct carries both the document name and its priority, giving the worker all the information it needs.

```go
package main

import "fmt"

// PrintJob represents a document queued for printing.
type PrintJob struct {
	Document string
	Pages    int
	Priority string
}

// processPrintJob receives a single PrintJob and displays its details.
func processPrintJob(queue <-chan PrintJob) {
	job := <-queue
	fmt.Printf("Printer: %s (%d pages, priority: %s)\n",
		job.Document, job.Pages, job.Priority)
}

// sendError simulates an asynchronous error report by sending
// an error value through the provided channel.
func sendError(errCh chan<- error, msg string) {
	errCh <- fmt.Errorf("%s", msg)
}

func main() {
	printQueue := make(chan PrintJob)
	go processPrintJob(printQueue)

	printQueue <- PrintJob{
		Document: "annual-report.pdf",
		Pages:    42,
		Priority: "high",
	}

	// Error channels are equally common in production code.
	errCh := make(chan error)
	go sendError(errCh, "printer offline: paper tray empty")

	err := <-errCh
	fmt.Println("Error received:", err)
}
```

### Verification
```bash
go run main.go
# Expected:
#   Printer: annual-report.pdf (42 pages, priority: high)
#   Error received: printer offline: paper tray empty
```

## Common Mistakes

### Sending and Receiving in the Same Goroutine

**Wrong:**
```go
package main

func main() {
    ch := make(chan string)
    ch <- "job.pdf"    // blocks forever -- no other goroutine to receive
    job := <-ch
    _ = job
}
```

**What happens:** The send blocks because no one is receiving, but the receive can never execute because the send has not completed. Deadlock.

**Correct:**
```go
package main

import "fmt"

func sendDocument(queue chan<- string, document string) {
	queue <- document
}

func main() {
	ch := make(chan string)
	go sendDocument(ch, "job.pdf")
	job := <-ch
	fmt.Println("Received:", job)
}
```

### Forgetting That var Declares a Nil Channel

**Wrong:**
```go
package main

func main() {
    var ch chan string // zero value is nil
    ch <- "job.pdf"   // blocks forever on nil channel!
}
```

**What happens:** A nil channel blocks on both send and receive, forever. The runtime detects the deadlock in simple programs, but in larger programs with other goroutines it can go unnoticed.

**Correct:**
```go
package main

import "fmt"

func sendDocument(queue chan<- string, document string) {
	queue <- document
}

func main() {
	ch := make(chan string) // always initialize with make
	go sendDocument(ch, "job.pdf")
	fmt.Println(<-ch)
}
```

## Review

`make(chan T)` creates an unbuffered channel that carries values of type `T`;
`ch <- val` sends and `val := <-ch` receives, and each of those operations
blocks until the other side is present. That mutual blocking is the rendezvous:
both goroutines must be ready simultaneously, so the moment a send completes the
receiver provably has the value. It is why the print-queue analogy fits -- no
tray, direct handoff -- and why the runtime can catch mismatched sends and
receives as a deadlock rather than letting them corrupt data silently. Two
habits keep it safe: always initialize with `make` (a nil channel blocks
forever, since `var ch chan T` gives you nil), and remember that channels are
strongly typed, so a `chan string` carries only strings and a `chan PrintJob`
carries only that struct.

Read the exercise back and answer three things without running it. What happens
if you send on an unbuffered channel and no goroutine is ready to receive? Can
you send an `int` on a `chan string`, and why or why not? And what makes the
print-queue analogy a genuinely accurate model of unbuffered behavior rather
than just a memorable one?

## Resources

- [A Tour of Go: Channels](https://go.dev/tour/concurrency/2) -- the interactive introduction to send/receive syntax and blocking.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- idiomatic channel use, including the synchronous-handoff patterns shown here.
- [Go Spec: Channel types](https://go.dev/ref/spec#Channel_types) -- the precise rules for channel creation, direction, and blocking behavior.

---

Back to [Concurrency](../../concurrency.md) | Next: [02-channel-as-synchronization](../02-channel-as-synchronization/02-channel-as-synchronization.md)
