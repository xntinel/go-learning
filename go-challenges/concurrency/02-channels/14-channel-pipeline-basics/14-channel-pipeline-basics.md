# Exercise 14: Channel Pipeline Basics

A pipeline is a chain of stages connected by channels: the first stage generates
data, each middle stage transforms what it receives, and the last stage consumes
the result -- every stage a goroutine, all running concurrently. The discipline
that keeps it from deadlocking or leaking is ownership: each stage creates its
own output channel and closes it when its input runs dry, and no stage ever
closes a channel it did not create. This exercise builds a three-stage pipeline
-- generate, validate, create welcome emails -- and traces how closing the first
channel cascades a clean shutdown all the way to the consumer.

## What you'll build

```text
14-channel-pipeline-basics/
  main.go        generator, validator, and email stages, plus an
                 ownership-annotated variant and a metrics-instrumented one
```

- Build: a three-stage user-record pipeline (generate -> validate -> welcome email), plus an ownership-annotated variant and a metrics-instrumented one.
- Implement: `generateUsers`, `validateEmails`, `createWelcomeEmails`, the annotated `stage1`/`stage2`/`stage3` functions, and a `StageMetrics` type.
- Verify: `go run main.go`, and `go run -race main.go` on the steps that use it.

### Why ownership makes shutdown automatic

This is how Go programs process data at scale. An ETL job reads records from a
database, validates them, transforms the format, and writes them to a
destination -- each step running concurrently. A web crawler fetches pages,
extracts links, filters duplicates, and indexes content. A log processor reads
entries, parses structured fields, enriches with metadata, and routes to
storage.

The pipeline pattern separates concerns cleanly: each stage does one thing, can
be tested independently, and overall throughput is limited only by the slowest
stage. This exercise builds a simple three-stage pipeline (read, validate,
generate) that turns user records into welcome emails.

## Step 1 -- Single Stage: The Generator

Every pipeline starts with a generator -- a function that produces data and sends it to a channel. The generator creates the output channel, launches a goroutine to populate it, and returns the channel. The goroutine closes the channel when done.

```go
package main

import "fmt"

type UserRecord struct {
	Name  string
	Email string
}

func generateUsers(users []UserRecord) <-chan UserRecord {
	out := make(chan UserRecord)
	go func() {
		defer close(out)
		for _, user := range users {
			out <- user
			fmt.Printf("[generate] sent: %s <%s>\n", user.Name, user.Email)
		}
	}()
	return out
}

func main() {
	users := []UserRecord{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: "bob@example.com"},
		{Name: "Carol", Email: "carol@example.com"},
	}

	for user := range generateUsers(users) {
		fmt.Printf("[main] received: %s\n", user.Name)
	}
	fmt.Println("Generator closed the channel, range exits cleanly")
}
```

Key pattern: the function that creates the channel also closes it (via the goroutine it launches). The caller receives a read-only channel (`<-chan`) and never worries about closing.

### Verification
```bash
go run main.go
# Expected: all 3 users generated and received, clean exit
```

## Step 2 -- Two Stages: Generator + Validator

Add a validation stage that reads from the generator's output, checks email format, and sends valid records to its own output channel. Invalid records are logged and dropped. Each stage owns its output channel.

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

func generateUsers(users []UserRecord) <-chan UserRecord {
	out := make(chan UserRecord)
	go func() {
		defer close(out)
		for _, user := range users {
			out <- user
		}
	}()
	return out
}

func validateEmails(input <-chan UserRecord) <-chan UserRecord {
	out := make(chan UserRecord) // validator creates its own output channel
	go func() {
		defer close(out) // validator closes what it created
		for user := range input {
			if user.Email == "" {
				fmt.Printf("[validate] SKIP %s: empty email\n", user.Name)
				continue
			}
			if !strings.Contains(user.Email, "@") {
				fmt.Printf("[validate] SKIP %s: missing @ in %q\n", user.Name, user.Email)
				continue
			}
			parts := strings.SplitN(user.Email, "@", 2)
			if !strings.Contains(parts[1], ".") {
				fmt.Printf("[validate] SKIP %s: no domain in %q\n", user.Name, user.Email)
				continue
			}
			fmt.Printf("[validate] OK   %s <%s>\n", user.Name, user.Email)
			out <- user
		}
	}()
	return out
}

func main() {
	users := []UserRecord{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: "invalid-email"},
		{Name: "Carol", Email: "carol@example.com"},
		{Name: "Dave", Email: ""},
		{Name: "Eve", Email: "eve@localhost"},
	}

	generated := generateUsers(users)
	validated := validateEmails(generated)

	fmt.Println("=== Valid Users ===")
	for user := range validated {
		fmt.Printf("  %s <%s>\n", user.Name, user.Email)
	}
}
```

Data flows: `users slice -> generateUsers -> validateEmails -> main`. Each arrow is a channel. Each stage reads its input until the channel closes, then closes its own output. The pipeline shuts down automatically from left to right.

### Verification
```bash
go run main.go
# Expected:
#   Alice and Carol pass validation
#   Bob (no @), Dave (empty), Eve (no domain) are skipped
```

## Step 3 -- Three Stages: The Complete Pipeline

Add the final stage: generate a welcome email for each validated user. The full pipeline is: generate -> validate -> create welcome emails.

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

type WelcomeEmail struct {
	To      string
	Subject string
	Body    string
}

func generateUsers(users []UserRecord) <-chan UserRecord {
	out := make(chan UserRecord)
	go func() {
		defer close(out)
		for _, user := range users {
			out <- user
		}
	}()
	return out
}

func validateEmails(input <-chan UserRecord) <-chan UserRecord {
	out := make(chan UserRecord)
	go func() {
		defer close(out)
		for user := range input {
			if user.Email == "" || !strings.Contains(user.Email, "@") {
				fmt.Printf("[validate] REJECT %s: invalid email\n", user.Name)
				continue
			}
			parts := strings.SplitN(user.Email, "@", 2)
			if !strings.Contains(parts[1], ".") {
				fmt.Printf("[validate] REJECT %s: no domain\n", user.Name)
				continue
			}
			out <- user
		}
	}()
	return out
}

func createWelcomeEmails(input <-chan UserRecord) <-chan WelcomeEmail {
	out := make(chan WelcomeEmail)
	go func() {
		defer close(out)
		for user := range input {
			email := WelcomeEmail{
				To:      user.Email,
				Subject: fmt.Sprintf("Welcome, %s!", user.Name),
				Body: fmt.Sprintf(
					"Hi %s,\n\nYour account is ready. Log in at https://app.example.com\n\nBest,\nThe Team",
					user.Name,
				),
			}
			out <- email
		}
	}()
	return out
}

func main() {
	users := []UserRecord{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: "bad-email"},
		{Name: "Carol", Email: "carol@example.com"},
		{Name: "Dave", Email: ""},
		{Name: "Eve", Email: "eve@company.org"},
	}

	// Build the pipeline: each stage feeds the next.
	generated := generateUsers(users)
	validated := validateEmails(generated)
	emails := createWelcomeEmails(validated)

	// Consume the final stage.
	fmt.Println("=== Welcome Emails ===")
	count := 0
	for email := range emails {
		count++
		fmt.Printf("\n--- Email %d ---\n", count)
		fmt.Printf("To:      %s\n", email.To)
		fmt.Printf("Subject: %s\n", email.Subject)
		fmt.Printf("Body:\n%s\n", email.Body)
	}

	fmt.Printf("\n=== Pipeline Complete: %d emails generated ===\n", count)
}
```

The three stages run concurrently as three goroutines. Data flows through two channels. Each stage starts processing as soon as data arrives -- the validator does not wait for all records to be generated, and the email creator does not wait for all records to be validated.

### Verification
```bash
go run main.go
# Expected:
#   Bob and Dave rejected by validator
#   3 welcome emails generated for Alice, Carol, Eve
```

## Step 4 -- Understanding Channel Ownership in Pipelines

This diagram makes the ownership explicit. Annotate each channel with who creates it, who writes to it, who reads from it, and who closes it.

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

type WelcomeEmail struct {
	To      string
	Subject string
}

// Stage 1: CREATES out, WRITES to out, CLOSES out.
// Caller: READS from out.
func stage1Generate(users []UserRecord) <-chan UserRecord {
	out := make(chan UserRecord) // created here
	go func() {
		defer close(out) // closed here
		for _, u := range users {
			out <- u // written here
		}
	}()
	return out
}

// Stage 2: READS from input (does not close it).
// CREATES out, WRITES to out, CLOSES out.
func stage2Validate(input <-chan UserRecord) <-chan UserRecord {
	out := make(chan UserRecord) // created here
	go func() {
		defer close(out) // closed here
		for u := range input { // read here (range exits when input closes)
			if strings.Contains(u.Email, "@") && strings.Contains(u.Email, ".") {
				out <- u // written here
			}
		}
	}()
	return out
}

// Stage 3: READS from input (does not close it).
// CREATES out, WRITES to out, CLOSES out.
func stage3Email(input <-chan UserRecord) <-chan WelcomeEmail {
	out := make(chan WelcomeEmail) // created here
	go func() {
		defer close(out) // closed here
		for u := range input { // read here
			out <- WelcomeEmail{ // written here
				To:      u.Email,
				Subject: fmt.Sprintf("Welcome, %s!", u.Name),
			}
		}
	}()
	return out
}

func main() {
	users := []UserRecord{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: "bad"},
		{Name: "Carol", Email: "carol@example.com"},
	}

	// Channel ownership chain:
	//   stage1 OWNS ch1 -> stage2 READS ch1, OWNS ch2 -> stage3 READS ch2, OWNS ch3
	ch1 := stage1Generate(users)
	ch2 := stage2Validate(ch1)
	ch3 := stage3Email(ch2)

	// main READS ch3 (does not close it).
	fmt.Println("Ownership Map:")
	fmt.Println("  ch1: created by stage1, closed by stage1")
	fmt.Println("  ch2: created by stage2, closed by stage2")
	fmt.Println("  ch3: created by stage3, closed by stage3")
	fmt.Println()

	for email := range ch3 {
		fmt.Printf("  Send: %s -> %s\n", email.Subject, email.To)
	}

	fmt.Println()
	fmt.Println("Shutdown cascade:")
	fmt.Println("  stage1 finishes -> closes ch1")
	fmt.Println("  stage2 range exits -> closes ch2")
	fmt.Println("  stage3 range exits -> closes ch3")
	fmt.Println("  main range exits -> done")
}
```

The shutdown cascade is the elegant consequence of ownership: when stage 1 finishes and closes its output, stage 2's `range` loop exits, which causes stage 2 to close its output, which causes stage 3's `range` loop to exit, and so on. The pipeline drains itself.

### Verification
```bash
go run -race main.go
# Expected: 2 emails sent, ownership map printed, no race warnings
```

## Step 5 -- Pipeline with Metrics

A production-ready version that tracks how many records each stage processes, rejects, and passes through.

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

type WelcomeEmail struct {
	To      string
	Subject string
}

type StageMetrics struct {
	mu       sync.Mutex
	name     string
	received int
	passed   int
	rejected int
}

func NewStageMetrics(name string) *StageMetrics {
	return &StageMetrics{name: name}
}

func (m *StageMetrics) RecordPass()   { m.mu.Lock(); m.received++; m.passed++; m.mu.Unlock() }
func (m *StageMetrics) RecordReject() { m.mu.Lock(); m.received++; m.rejected++; m.mu.Unlock() }
func (m *StageMetrics) Report() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("%-12s received=%d passed=%d rejected=%d",
		m.name, m.received, m.passed, m.rejected)
}

func generate(users []UserRecord, metrics *StageMetrics) <-chan UserRecord {
	out := make(chan UserRecord)
	go func() {
		defer close(out)
		for _, u := range users {
			metrics.RecordPass()
			out <- u
		}
	}()
	return out
}

func validate(input <-chan UserRecord, metrics *StageMetrics) <-chan UserRecord {
	out := make(chan UserRecord)
	go func() {
		defer close(out)
		for u := range input {
			if u.Email == "" || !strings.Contains(u.Email, "@") || !strings.Contains(u.Email, ".") {
				metrics.RecordReject()
				continue
			}
			metrics.RecordPass()
			out <- u
		}
	}()
	return out
}

func createEmails(input <-chan UserRecord, metrics *StageMetrics) <-chan WelcomeEmail {
	out := make(chan WelcomeEmail)
	go func() {
		defer close(out)
		for u := range input {
			metrics.RecordPass()
			out <- WelcomeEmail{
				To:      u.Email,
				Subject: fmt.Sprintf("Welcome, %s!", u.Name),
			}
		}
	}()
	return out
}

func main() {
	users := []UserRecord{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: "invalid"},
		{Name: "Carol", Email: "carol@example.com"},
		{Name: "Dave", Email: ""},
		{Name: "Eve", Email: "eve@company.org"},
		{Name: "Frank", Email: "frank@nodomain"},
	}

	genMetrics := NewStageMetrics("generate")
	valMetrics := NewStageMetrics("validate")
	emailMetrics := NewStageMetrics("email")

	ch1 := generate(users, genMetrics)
	ch2 := validate(ch1, valMetrics)
	ch3 := createEmails(ch2, emailMetrics)

	fmt.Println("=== Emails Generated ===")
	for email := range ch3 {
		fmt.Printf("  %s -> %s\n", email.Subject, email.To)
	}

	fmt.Println()
	fmt.Println("=== Pipeline Metrics ===")
	fmt.Println(genMetrics.Report())
	fmt.Println(valMetrics.Report())
	fmt.Println(emailMetrics.Report())

	fmt.Println()
	fmt.Println("=== Data Flow ===")
	fmt.Println("  6 records in -> 3 validated -> 3 emails out")
	fmt.Println("  3 records rejected at validation stage")
}
```

### Verification
```bash
go run -race main.go
# Expected:
#   3 welcome emails generated
#   generate:  received=6 passed=6 rejected=0
#   validate:  received=6 passed=3 rejected=3
#   email:     received=3 passed=3 rejected=0
#   No race warnings
```

## Verification

Run all programs and confirm:
1. The generator stage produces data and closes its output channel
2. The validator filters invalid records and passes valid ones downstream
3. The complete pipeline chains three goroutines through two channels
4. Channel ownership is clear: each stage creates, writes, and closes its own output
5. The shutdown cascade propagates cleanly from first stage to last

## Common Mistakes

### Reading Stage Closes the Input Channel

**Wrong:**
```go
func validate(input <-chan UserRecord) <-chan UserRecord {
    out := make(chan UserRecord)
    go func() {
        defer close(out)
        defer close(input) // WRONG: validate does not own input
        for u := range input {
            out <- u
        }
    }()
    return out
}
```

**Fix:** Never close a channel you received as a parameter. The stage that created the channel is responsible for closing it.

### Forgetting to Close the Output Channel

**Wrong:**
```go
func validate(input <-chan UserRecord) <-chan UserRecord {
    out := make(chan UserRecord)
    go func() {
        // forgot defer close(out)
        for u := range input {
            out <- u
        }
    }()
    return out
}
```

**Fix:** The next stage's `range` loop will block forever because the channel never closes. Always `defer close(out)` at the top of the goroutine.

### Building the Pipeline Sequentially Instead of Concurrently

**Wrong:**
```go
// This runs stages one at a time, not concurrently.
var validated []UserRecord
for _, u := range users {
    if isValid(u) {
        validated = append(validated, u)
    }
}
for _, u := range validated {
    sendEmail(u)
}
```

**Fix:** The pipeline pattern is specifically designed for concurrent processing. Each stage runs in its own goroutine and processes records as they arrive, not after the previous stage finishes entirely.

## Review

A pipeline is a chain of stage goroutines joined by channels. Each stage function
creates its output channel, launches a goroutine that reads its input, does its
one job, sends downstream, and closes the output when the input closes. The
consumer never closes a channel it did not create. Because closing stage N's
output makes stage N+1's `range` exit -- which closes N+1's output in turn -- the
shutdown cascades left to right on its own, and no stage needs to know how many
stages sit behind or ahead of it. Communicating only through channels is what
lets each stage be tested in isolation, fed a channel of known data.

You should be able to answer the pipeline's own questions without scrolling back.
The channel between two stages is created by the upstream stage, as its output,
and closed by that same stage when its work is done -- the downstream stage only
reads it. If a middle stage forgets to close its output, the next stage's
`range` never returns and the pipeline hangs there forever. And the shutdown
cascade is the payoff of that ownership rule: when the first stage finishes and
closes its output, the second stage's range exits and closes its own output,
which ends the third stage's range, and so on until the consumer's loop returns
-- the pipeline drains itself from front to back.

## Resources
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the canonical write-up of stage ownership and the shutdown cascade.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- channel fundamentals: creation, direction, and closing.
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the talk that introduced these composable channel-stage patterns.

---

Back to [Concurrency](../../concurrency.md) | Next: [15-channel-event-bus](../15-channel-event-bus/15-channel-event-bus.md)
