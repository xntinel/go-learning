# Exercise 4: Channel Direction

In a data processing pipeline data flows one way: a source produces records, a
filter drops the invalid ones, a transformer reshapes what survives, and an
output stage writes the result. A stage that accidentally sends back into its
own input instead of reading from it is a bug that costs an afternoon to find
at runtime -- and nothing at all to find at compile time, if you let the type
system see the direction.

## What you'll build

```text
04-channel-direction/
  main.go        produce -> filter -> transform -> output, each stage
                 declaring its data flow in its own signature
```

- Build: a four-stage invoice pipeline where every stage names its direction.
- Implement: `produce` returning `<-chan Record`; `filter(in <-chan Record, out chan<- Record)`; a transform stage; an output stage that only receives.
- Verify: `go run main.go`, then try to break a stage on purpose and watch the compiler reject it.

### Why the compiler can enforce data flow

Go's type system lets you restrict a channel to send-only (`chan<- T`) or
receive-only (`<-chan T`). A producer that accepts `chan<- Record` cannot
accidentally receive from it. A consumer that accepts `<-chan Record` cannot
accidentally send. The restriction is not a convention or a lint rule; it is
part of the parameter's type, so violating it is a compile error.

The payoff is that data flow becomes readable in the signature. When you read
`func filter(in <-chan Record, out chan<- Record)` you know, without opening
the body, that records move from `in` to `out` -- and the compiler guarantees
the body cannot do otherwise.

## Step 1 -- Understand the Syntax

Read the arrow's direction relative to `chan`:

```
chan T      // bidirectional: can send and receive
chan<- T    // send-only: can only send (arrow points INTO chan)
<-chan T    // receive-only: can only receive (arrow points OUT of chan)
```

Mnemonic: the arrow `<-` always represents data flow. `chan<- T` means data flows into the channel (send). `<-chan T` means data flows out of the channel (receive).

## Step 2 -- CSV Record Producer

Build a producer that reads CSV records and sends them through a channel. The function returns a `<-chan` (receive-only for the caller). Only the internal goroutine can send.

```go
package main

import (
	"fmt"
	"strings"
)

const expectedCSVFields = 3

// Record represents a single row from the CSV data source.
type Record struct {
	Name   string
	Email  string
	Amount float64
}

// parseCSVLine attempts to parse a CSV line into a Record.
// Returns the record and true if parsing succeeds, or a zero Record and false otherwise.
func parseCSVLine(line string) (Record, bool) {
	fields := strings.Split(line, ",")
	if len(fields) != expectedCSVFields {
		return Record{}, false
	}
	var amount float64
	fmt.Sscanf(fields[2], "%f", &amount)
	return Record{
		Name:   strings.TrimSpace(fields[0]),
		Email:  strings.TrimSpace(fields[1]),
		Amount: amount,
	}, true
}

// readCSV returns a receive-only channel. The caller can only consume records.
// The goroutine inside owns the send side -- no one else can write to it.
func readCSV(csvData string) <-chan Record {
	ch := make(chan Record)
	go func() {
		for _, line := range strings.Split(csvData, "\n") {
			if rec, ok := parseCSVLine(line); ok {
				ch <- rec
			}
		}
		close(ch)
	}()
	return ch // auto-narrows from chan Record to <-chan Record
}

func main() {
	csv := `Alice,alice@corp.com,150.00
Bob,bob@corp.com,75.50
Carol,carol@corp.com,200.00`

	for rec := range readCSV(csv) {
		fmt.Printf("Record: %s <%s> $%.2f\n", rec.Name, rec.Email, rec.Amount)
	}
}
```

### Verification
```bash
go run main.go
# Expected:
#   Record: Alice <alice@corp.com> $150.00
#   Record: Bob <bob@corp.com> $75.50
#   Record: Carol <carol@corp.com> $200.00
```

## Step 3 -- Filter Stage with Directional Channels

A filter reads records from `<-chan` (receive-only) and writes passing records to `chan<-` (send-only). The signature enforces the data flow direction: data can only move from `in` to `out`.

```go
package main

import (
	"fmt"
	"strings"
)

const (
	expectedFields     = 3
	highValueThreshold = 100.00
)

// Record represents a single row from the CSV data source.
type Record struct {
	Name   string
	Email  string
	Amount float64
}

func parseCSVLine(line string) (Record, bool) {
	fields := strings.Split(line, ",")
	if len(fields) != expectedFields {
		return Record{}, false
	}
	var amount float64
	fmt.Sscanf(fields[2], "%f", &amount)
	return Record{
		Name:   strings.TrimSpace(fields[0]),
		Email:  strings.TrimSpace(fields[1]),
		Amount: amount,
	}, true
}

func readCSV(csvData string) <-chan Record {
	ch := make(chan Record)
	go func() {
		for _, line := range strings.Split(csvData, "\n") {
			if rec, ok := parseCSVLine(line); ok {
				ch <- rec
			}
		}
		close(ch)
	}()
	return ch
}

// filterHighValue reads from in (receive-only) and writes to out (send-only).
// Only records with Amount >= minAmount pass through.
// Try adding `val := <-out` inside this function -- the compiler rejects it.
func filterHighValue(in <-chan Record, out chan<- Record, minAmount float64) {
	for rec := range in {
		if rec.Amount >= minAmount {
			out <- rec
		}
	}
	close(out)
}

func main() {
	csv := `Alice,alice@corp.com,150.00
Bob,bob@corp.com,75.50
Carol,carol@corp.com,200.00
Dave,dave@corp.com,50.00
Eve,eve@corp.com,300.00`

	raw := readCSV(csv)
	filtered := make(chan Record)
	go filterHighValue(raw, filtered, highValueThreshold)

	for rec := range filtered {
		fmt.Printf("High-value: %s $%.2f\n", rec.Name, rec.Amount)
	}
}
```

### Verification
```bash
go run main.go
# Expected:
#   High-value: Alice $150.00
#   High-value: Carol $200.00
#   High-value: Eve $300.00
```

## Step 4 -- Full Pipeline: Filter, Transform, Output

Connect three stages into a complete data processing pipeline. Each stage uses directional channels to enforce data flow. The compiler guarantees no stage can accidentally read from its output or write to its input.

```go
package main

import (
	"fmt"
	"strings"
)

const (
	csvExpectedFields  = 3
	filterMinAmount    = 100.00
	taxMultiplier      = 1.1
)

// Record represents a raw CSV row with client data.
type Record struct {
	Name   string
	Email  string
	Amount float64
}

// InvoiceRecord is the final output after filtering and tax application.
type InvoiceRecord struct {
	Label string
	Total string
}

func parseCSVLine(line string) (Record, bool) {
	fields := strings.Split(line, ",")
	if len(fields) != csvExpectedFields {
		return Record{}, false
	}
	var amount float64
	fmt.Sscanf(fields[2], "%f", &amount)
	return Record{
		Name:   strings.TrimSpace(fields[0]),
		Email:  strings.TrimSpace(fields[1]),
		Amount: amount,
	}, true
}

func readCSV(csvData string) <-chan Record {
	ch := make(chan Record)
	go func() {
		for _, line := range strings.Split(csvData, "\n") {
			if rec, ok := parseCSVLine(line); ok {
				ch <- rec
			}
		}
		close(ch)
	}()
	return ch
}

func filterHighValue(in <-chan Record, out chan<- Record, minAmount float64) {
	for rec := range in {
		if rec.Amount >= minAmount {
			out <- rec
		}
	}
	close(out)
}

// applyTax reads Records and produces InvoiceRecords with tax applied.
// in is receive-only, out is send-only -- data flows one direction.
func applyTax(in <-chan Record, out chan<- InvoiceRecord, multiplier float64) {
	for rec := range in {
		out <- InvoiceRecord{
			Label: fmt.Sprintf("%s (%s)", rec.Name, rec.Email),
			Total: fmt.Sprintf("$%.2f", rec.Amount*multiplier),
		}
	}
	close(out)
}

// printInvoiceReport drains the transformed channel and prints each record.
func printInvoiceReport(records <-chan InvoiceRecord) {
	fmt.Println("=== Invoice Report (High-Value Clients, +10% Tax) ===")
	for rec := range records {
		fmt.Printf("  %s => %s\n", rec.Label, rec.Total)
	}
}

func main() {
	csv := `Alice,alice@corp.com,150.00
Bob,bob@corp.com,75.50
Carol,carol@corp.com,200.00
Dave,dave@corp.com,50.00
Eve,eve@corp.com,300.00`

	// Pipeline: readCSV -> filter (>= $100) -> applyTax (+10%) -> output
	raw := readCSV(csv)
	filtered := make(chan Record)
	invoices := make(chan InvoiceRecord)

	go filterHighValue(raw, filtered, filterMinAmount)
	go applyTax(filtered, invoices, taxMultiplier)

	printInvoiceReport(invoices)
}
```

### Verification
```bash
go run main.go
# Expected:
#   === Invoice Report (High-Value Clients, +10% Tax) ===
#     Alice (alice@corp.com) => $165.00
#     Carol (carol@corp.com) => $220.00
#     Eve (eve@corp.com) => $330.00
```

## Step 5 -- Compile-Time Protection in Action

This step demonstrates the compile-time errors you get when you violate channel direction. Try each mistake and observe the compiler error.

```go
package main

import "fmt"

// produceValue can ONLY send on the channel.
func produceValue(out chan<- int) {
	// Uncomment to see compile error:
	// val := <-out  // invalid operation: cannot receive from send-only channel
	out <- 42
}

// consumeValue can ONLY receive from the channel.
func consumeValue(in <-chan int) {
	val := <-in
	fmt.Println(val)
	// Uncomment to see compile error:
	// in <- 99      // invalid operation: cannot send to receive-only channel
	// close(in)     // invalid operation: cannot close receive-only channel
}

func main() {
	ch := make(chan int)
	go produceValue(ch) // auto-narrows to chan<- int
	consumeValue(ch)    // auto-narrows to <-chan int

	// You CANNOT widen permissions:
	// var readOnly <-chan int = make(chan int)
	// produceValue(readOnly) // compile error: cannot use readOnly as chan<- int
}
```

### Verification
```bash
go run main.go
# Expected: 42
# Uncomment the broken lines to see compile errors
```

## Verification

Review your pipeline and confirm:
1. Each stage function clearly declares its data flow via `<-chan` and `chan<-`
2. The compiler rejects any attempt to read from an output or write to an input
3. Each stage closes its output channel when its input is exhausted

## Common Mistakes

### Trying to Close a Receive-Only Channel

**Wrong:**
```go
func consumer(in <-chan int) {
    for val := range in {
        fmt.Println(val)
    }
    close(in) // compile error!
}
```

**What happens:** Compile error. Only the sender should close a channel. The type system enforces this convention.

**Fix:** Remove the close. The producer closes the channel when done.

### Trying to Widen Permissions

**Wrong:**
```go
func needsBidirectional(ch chan int) { /* ... */ }

var readOnly <-chan int = make(chan int)
needsBidirectional(readOnly) // compile error!
```

**What happens:** Compile error. You cannot widen permissions -- a receive-only channel cannot become bidirectional.

**Fix:** Pass the bidirectional channel, or change the function signature to accept the narrower type.

## Review

The whole exercise turns on one asymmetry: a bidirectional `chan T` converts
implicitly to `chan<- T` or `<-chan T` when you pass it to a function, but the
conversion never runs backwards. That is what makes the narrow type a real
guarantee rather than a hint -- once a stage holds a `<-chan Record`, no code
in that stage can send on it, and no cast will give the permission back. It is
also why `close` is only available on the send side: closing is a producer's
statement that nothing more is coming, and a receive-only channel makes that
statement unspeakable.

Read your own pipeline back and answer three questions without running it.
What does `chan<- Record` mean, and how does it differ from `<-chan Record`?
Why does the filter stage take `<-chan` in and `chan<-` out rather than two
bidirectional channels? And who closes each channel -- the sender or the
receiver? If a stage's signature does not already answer the third question,
the direction is not doing its job.

## Resources

- [A Tour of Go: Channel Directions](https://go.dev/tour/concurrency/4) -- the two-minute version of the syntax, with an exercise.
- [Go Spec: Channel types](https://go.dev/ref/spec#Channel_types) -- the exact conversion rule that lets bidirectional narrow but never widen.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- how directional stages compose into real pipelines, and who owns closing.

---

Back to [Concurrency](../../concurrency.md) | Next: [05-ranging-over-channels](../05-ranging-over-channels/05-ranging-over-channels.md)
