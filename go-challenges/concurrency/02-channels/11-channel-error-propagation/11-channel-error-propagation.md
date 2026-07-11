# Exercise 11: Channel Error Propagation

When each stage of a pipeline runs in its own goroutine, errors have nowhere to
go: a validation goroutine has no caller to `return err` to, so a dropped error
becomes a silently wrong result and an unhandled one becomes a crashed program.
The fix is to make errors first-class data -- wrap each value together with its
error in a `Result` struct and send it down the same channel as the successes.
The consumer then decides the policy: stop on the first failure, skip and keep
going, or split the streams entirely. This exercise builds all three.

## What you'll build

```text
11-channel-error-propagation/
  main.go        the lost-error problem, the Result type, stop-on-first,
                 a two-channel validator, and a strategy-selection guide
```

- Build: three error-propagation strategies for a record-validation pipeline.
- Implement: a `Result`/`ValidationResult` struct carrying value-or-error, a stop-on-first producer that returns after the first bad record, and a two-channel `Validator` with independent data and error consumers.
- Verify: `go run main.go` for each strategy.

### Why errors must travel as data

Channels solve this by making errors first-class data. You wrap your value and error into a Result struct and send it down the same channel as successful results. The consumer decides what to do: stop immediately, skip the bad record and continue, or collect all errors for a report. This is the standard pattern in production Go pipelines -- from ETL jobs to API request processors.

## Step 1 -- The Problem: Lost Errors

A naive pipeline that validates user records silently drops invalid ones. The caller has no way to know records were skipped or why.

```go
package main

import (
	"fmt"
	"strings"
	"sync"
)

type UserRecord struct {
	Name  string
	Email string
}

func validate(records []UserRecord) []UserRecord {
	var valid []UserRecord
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, r := range records {
		wg.Add(1)
		go func(rec UserRecord) {
			defer wg.Done()
			if !strings.Contains(rec.Email, "@") {
				return // error silently swallowed
			}
			mu.Lock()
			valid = append(valid, rec)
			mu.Unlock()
		}(r)
	}

	wg.Wait()
	return valid
}

func main() {
	records := []UserRecord{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: "not-an-email"},
		{Name: "Carol", Email: "carol@example.com"},
		{Name: "Dave", Email: ""},
	}

	valid := validate(records)
	fmt.Printf("Valid: %d out of %d\n", len(valid), len(records))
	fmt.Println("But which records failed? Why? We have no idea.")
}
```

### Verification
```bash
go run main.go
# Expected: Valid: 2 out of 4 -- but no error details available
```

## Step 2 -- The Result Type: Value or Error, Never Both

Define a `Result` struct that pairs a value with an optional error. Send Results through a single channel. The consumer inspects each Result and decides how to handle failures.

```go
package main

import (
	"fmt"
	"strings"
)

type UserRecord struct {
	Name  string
	Email string
}

type ValidationResult struct {
	Record UserRecord
	Err    error
}

func validateRecords(records []UserRecord) <-chan ValidationResult {
	results := make(chan ValidationResult)

	go func() {
		defer close(results)
		for _, rec := range records {
			if rec.Email == "" {
				results <- ValidationResult{
					Record: rec,
					Err:    fmt.Errorf("record %q: email is empty", rec.Name),
				}
				continue
			}
			if !strings.Contains(rec.Email, "@") {
				results <- ValidationResult{
					Record: rec,
					Err:    fmt.Errorf("record %q: invalid email %q", rec.Name, rec.Email),
				}
				continue
			}
			results <- ValidationResult{Record: rec, Err: nil}
		}
	}()

	return results
}

func main() {
	records := []UserRecord{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: "not-an-email"},
		{Name: "Carol", Email: "carol@example.com"},
		{Name: "Dave", Email: ""},
	}

	var valid []UserRecord
	var errors []error

	for result := range validateRecords(records) {
		if result.Err != nil {
			errors = append(errors, result.Err)
			continue
		}
		valid = append(valid, result.Record)
	}

	fmt.Printf("Valid records: %d\n", len(valid))
	for _, r := range valid {
		fmt.Printf("  %s <%s>\n", r.Name, r.Email)
	}
	fmt.Printf("\nErrors: %d\n", len(errors))
	for _, err := range errors {
		fmt.Printf("  %v\n", err)
	}
}
```

The key insight: the producer sends ALL records (valid and invalid) through the same channel. The consumer separates successes from failures. Nothing is silently lost.

### Verification
```bash
go run main.go
# Expected:
#   Valid records: 2
#     Alice <alice@example.com>
#     Carol <carol@example.com>
#   Errors: 2
#     record "Bob": invalid email "not-an-email"
#     record "Dave": email is empty
```

## Step 3 -- Strategy: Stop on First Error

Sometimes a single invalid record means the entire batch is bad (e.g., a financial transaction file with a corrupted header). In this pattern, the pipeline stops as soon as any stage produces an error.

```go
package main

import (
	"fmt"
	"strings"
)

type Record struct {
	ID    int
	Email string
}

type Result struct {
	Record Record
	Err    error
}

func validateStream(records []Record) <-chan Result {
	out := make(chan Result)
	go func() {
		defer close(out)
		for _, rec := range records {
			if !strings.Contains(rec.Email, "@") {
				out <- Result{Record: rec, Err: fmt.Errorf("record %d: invalid email %q", rec.ID, rec.Email)}
				return
			}
			out <- Result{Record: rec}
		}
	}()
	return out
}

func processWithStopOnFirst(records []Record) ([]Record, error) {
	var processed []Record
	for result := range validateStream(records) {
		if result.Err != nil {
			return processed, result.Err
		}
		processed = append(processed, result.Record)
	}
	return processed, nil
}

func main() {
	records := []Record{
		{ID: 1, Email: "alice@example.com"},
		{ID: 2, Email: "bad-email"},
		{ID: 3, Email: "carol@example.com"},
	}

	processed, err := processWithStopOnFirst(records)
	fmt.Printf("Processed %d records before stopping\n", len(processed))
	if err != nil {
		fmt.Printf("Stopped: %v\n", err)
		fmt.Println("Record 3 was never processed (stop-on-first)")
	}
}
```

When the producer sends an error Result and returns, it closes the channel. The consumer breaks out of the range loop and reports the error. Records after the failure point are never processed.

### Verification
```bash
go run main.go
# Expected:
#   Processed 1 records before stopping
#   Stopped: record 2: invalid email "bad-email"
#   Record 3 was never processed (stop-on-first)
```

## Step 4 -- Strategy: Separate Error Channel

For pipelines where valid data should keep flowing regardless of individual failures, use two channels: one for data, one for errors. This lets different consumers handle each stream independently.

```go
package main

import (
	"fmt"
	"strings"
	"sync"
)

type UserRecord struct {
	Name  string
	Email string
}

type ValidationError struct {
	Record UserRecord
	Reason string
}

func (ve ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", ve.Record.Name, ve.Reason)
}

type Validator struct {
	data   chan UserRecord
	errors chan error
}

func NewValidator() *Validator {
	return &Validator{
		data:   make(chan UserRecord),
		errors: make(chan error),
	}
}

func (v *Validator) Run(records []UserRecord) {
	go func() {
		defer close(v.data)
		defer close(v.errors)
		for _, rec := range records {
			if rec.Email == "" {
				v.errors <- ValidationError{Record: rec, Reason: "empty email"}
				continue
			}
			if !strings.Contains(rec.Email, "@") {
				v.errors <- ValidationError{Record: rec, Reason: "missing @ in email"}
				continue
			}
			if !strings.Contains(rec.Email, ".") {
				v.errors <- ValidationError{Record: rec, Reason: "missing domain in email"}
				continue
			}
			v.data <- rec
		}
	}()
}

func (v *Validator) Data() <-chan UserRecord { return v.data }
func (v *Validator) Errors() <-chan error     { return v.errors }

func main() {
	records := []UserRecord{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: "not-an-email"},
		{Name: "Carol", Email: "carol@example.com"},
		{Name: "Dave", Email: ""},
		{Name: "Eve", Email: "eve@localhost"},
	}

	validator := NewValidator()
	validator.Run(records)

	var wg sync.WaitGroup
	var validRecords []UserRecord
	var collectedErrors []error

	wg.Add(2)

	go func() {
		defer wg.Done()
		for rec := range validator.Data() {
			validRecords = append(validRecords, rec)
		}
	}()

	go func() {
		defer wg.Done()
		for err := range validator.Errors() {
			collectedErrors = append(collectedErrors, err)
		}
	}()

	wg.Wait()

	fmt.Println("=== Valid Records (sent to next stage) ===")
	for _, r := range validRecords {
		fmt.Printf("  %s <%s>\n", r.Name, r.Email)
	}
	fmt.Println()
	fmt.Println("=== Errors (sent to error log) ===")
	for _, err := range collectedErrors {
		fmt.Printf("  %v\n", err)
	}
	fmt.Printf("\nTotal: %d valid, %d errors out of %d records\n",
		len(validRecords), len(collectedErrors), len(records))
}
```

Two channels, two consumers. The data consumer builds the next pipeline stage. The error consumer writes to a log or alert system. Neither blocks the other. This is the most flexible pattern for high-throughput pipelines.

### Verification
```bash
go run main.go
# Expected:
#   === Valid Records (sent to next stage) ===
#     Alice <alice@example.com>
#     Carol <carol@example.com>
#   === Errors (sent to error log) ===
#     Bob: missing @ in email
#     Dave: empty email
#     Eve: missing domain in email
#   Total: 2 valid, 3 errors out of 5 records
```

## Step 5 -- Choosing the Right Strategy

| Strategy | When to Use | Trade-off |
|---|---|---|
| Result struct (collect all) | Batch processing, data import, validation reports | All records processed; consumer decides policy |
| Stop on first error | Financial transactions, config parsing, migrations | Fast failure; skips remaining records |
| Separate error channel | High-throughput streaming, log pipelines | Most flexible; two consumers needed |

```go
package main

import "fmt"

func main() {
	fmt.Println("Error Propagation Strategy Guide")
	fmt.Println("================================")
	fmt.Println()
	fmt.Println("1. Result struct (value OR error in same channel)")
	fmt.Println("   Best for: batch jobs where you want a full report")
	fmt.Println("   Pattern:  type Result struct { Value T; Err error }")
	fmt.Println()
	fmt.Println("2. Stop on first error")
	fmt.Println("   Best for: all-or-nothing operations")
	fmt.Println("   Pattern:  producer returns after sending first error Result")
	fmt.Println()
	fmt.Println("3. Separate error channel")
	fmt.Println("   Best for: streaming where valid data must keep flowing")
	fmt.Println("   Pattern:  data chan T + errors chan error, two consumers")
	fmt.Println()
	fmt.Println("Rule of thumb: start with the Result struct.")
	fmt.Println("Split into separate channels only when consumers need independence.")
}
```

### Verification
```bash
go run main.go
# Expected: strategy guide printed
```

## Verification

Run all programs and confirm:
1. The naive version loses error information completely
2. The Result struct version reports all failures with context
3. The stop-on-first version halts processing at the first error
4. The separate channel version processes valid and invalid records independently

## Common Mistakes

### Sending Only Errors, Not Successes

**Wrong:**
```go
// Only send when there is an error
if err != nil {
    results <- Result{Err: err}
}
// Valid records vanish -- consumer never sees them
```

**Fix:** Send every record through the channel, wrapping valid records with `Err: nil`. The consumer uses the Err field to branch, not the presence or absence of a message.

### Forgetting to Close Both Channels

**Wrong:**
```go
go func() {
    defer close(data)
    // forgot to close(errors) -- error consumer blocks forever
    for _, rec := range records {
        if rec.invalid {
            errors <- ValidationError{...}
            continue
        }
        data <- rec
    }
}()
```

**Fix:** When using separate channels, the producer must close both. Use `defer close(data)` and `defer close(errors)` at the top of the goroutine.

### Creating Error Types Without the Error Interface

**Wrong:**
```go
type ValidationError struct {
    Record UserRecord
    Reason string
}
// Missing Error() method -- cannot send on chan error
```

**Fix:** Implement `func (ve ValidationError) Error() string` to satisfy the `error` interface.

## Review

A goroutine has no caller to hand an error back to, so the only honest way to
move a failure out of a pipeline stage is to send it as data. The `Result`
struct -- a value paired with an `Err error` -- does exactly that: the producer
sends every record through one channel, valid ones with `Err: nil` and invalid
ones carrying the reason, and the consumer branches on the `Err` field rather
than on whether a message arrived. Nothing is silently lost. From that base the
three strategies are just consumer policies: collect-all accumulates successes
and errors side by side for a full report; stop-on-first returns the moment the
producer sends an error and closes the channel, leaving later records
unprocessed; and the two-channel `Validator` splits data and errors into
independent streams read by independent consumers. Whichever you choose, the
producer owns and closes every channel it created -- and with separate channels
that means closing both, or the error consumer blocks forever.

Three questions should now answer themselves. A goroutine cannot use
`return err` because it runs detached from whoever started it -- there is no
return path back to `main`, only the channel. In the stop-on-first pattern every
record after the failure is never processed: the producer returns and closes the
channel, and the consumer's range loop ends. And you reach for separate error
channels when valid data must keep flowing to the next stage regardless of
individual failures, so a data consumer and an error consumer can work at their
own pace without one blocking the other. Reach for the single `Result` channel
first; split only when the consumers genuinely need that independence.

## Resources
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- how pipeline stages connect over channels and how failures and cancellation flow through them.
- [Go Blog: Error handling and Go](https://go.dev/blog/error-handling-and-go) -- the error-as-value philosophy that the Result struct extends into channels.
- [Effective Go: Errors](https://go.dev/doc/effective_go#errors) -- implementing the `error` interface, which the ValidationError type relies on.

---

Back to [Concurrency](../../concurrency.md) | Next: [12-channel-ownership-patterns](../12-channel-ownership-patterns/12-channel-ownership-patterns.md)
