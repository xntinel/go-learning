# Exercise 16: Event Ledger — Deferred Flush After Transactional Commit

**Nivel: Intermedio** — validacion rapida (un test corto).

A service that publishes domain events as it accumulates them, before its
transaction has actually committed, will publish events for writes that
later fail to commit — a downstream consumer sees an order as "processed"
that the database never persisted. The fix is to accumulate events on the
transaction and publish them from a single deferred closure that only fires
the publish call once the function's final outcome is known to be success.
This module builds that gate with a named-return `err` and shows why the
closure must read that named variable, not a local one. The module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
ledger/                     independent module: example.com/event-ledger-deferred-flush
  go.mod                     go 1.24
  ledger.go                  Event, Tx, Publisher, Write(input, shouldFailCommit, publish) error
  cmd/
    demo/
      main.go                runnable demo: success, commit-failure, validation-failure runs
  ledger_test.go             table test over validation/commit-failure/success outcomes
```

- Files: `ledger.go`, `cmd/demo/main.go`, `ledger_test.go`.
- Implement: `Event`, `Tx` (`Record`, `Commit`, `Rollback`), `Publisher`, and `Write(input string, shouldFailCommit bool, publish Publisher) (err error)`.
- Test: a table over validation failure, commit failure, and success, asserting `err` and whether `publish` ran.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/event-ledger-deferred-flush/cmd/demo
cd ~/go-exercises/event-ledger-deferred-flush
go mod init example.com/event-ledger-deferred-flush
go mod edit -go=1.24
```

### Why the closure must read the named return

`Write` records events into `tx.events` as it goes — one per verified step —
and defers a single closure right after `tx` is created. That closure reads
`err`, the function's *named* return value, after every branch below has run
and assigned it. If `err` is non-nil — whichever of the two failure branches
set it — the closure rolls back and returns without publishing anything. If
`err` is still `nil` when the closure runs, every branch that could have
failed the write has already been passed, so `tx.events` is exactly the set
of events for a transaction that genuinely committed, and `publish` sees all
of them. The mechanism depends entirely on `err` being the function's real
named return, not a shadowed local: a closure that instead captured a fresh
`err := ...` declared inside one of the branches would only ever see that
branch's outcome, and would publish on the branches where no such local
exists at all — silently publishing events for a transaction that actually
failed elsewhere.

Create `ledger.go`:

```go
package ledger

import "errors"

// ErrValidation means the input failed validation before any write happened.
var ErrValidation = errors.New("ledger: validation failed")

// ErrCommit means the transaction's commit step failed.
var ErrCommit = errors.New("ledger: commit failed")

// Event is one domain event accumulated while a transaction runs.
type Event struct {
	Name string
}

// Publisher hands a finished transaction's events to whatever downstream
// system fans them out. Production code publishes to a message bus; here
// it is injected so the test and demo can capture what was published.
type Publisher func([]Event)

// Tx accumulates events for one transactional write and tracks whether it
// committed or rolled back.
type Tx struct {
	events     []Event
	rolledBack bool
}

// Record appends a domain event to the transaction's ledger.
func (tx *Tx) Record(e Event) { tx.events = append(tx.events, e) }

// Commit marks the transaction as committed.
func (tx *Tx) Commit() { tx.rolledBack = false }

// Rollback marks the transaction as rolled back.
func (tx *Tx) Rollback() { tx.rolledBack = true }

// Write runs a transactional write, recording domain events as it goes, and
// publishes those events only if the transaction actually commits.
// shouldFailCommit simulates a downstream commit failure for tests.
//
// The deferred closure below is registered once and reads the function's
// named return value err after every branch has run and decided it --
// validation, the simulated commit failure, or the success path. Because it
// reads the *named* return rather than a local shadowed err, it always sees
// the real final outcome: non-nil means roll back and never call publish,
// nil means the transaction genuinely committed and its events are safe to
// fan out.
func Write(input string, shouldFailCommit bool, publish Publisher) (err error) {
	tx := &Tx{}

	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		publish(tx.events)
	}()

	if input == "" {
		err = ErrValidation
		return
	}
	tx.Record(Event{Name: "validated:" + input})
	tx.Record(Event{Name: "processed:" + input})

	if shouldFailCommit {
		err = ErrCommit
		return
	}

	tx.Commit()
	tx.Record(Event{Name: "committed"})
	return nil
}
```

### The runnable demo

The demo runs `Write` three times — success, a simulated commit failure, and
a validation failure — printing what got published (if anything) and the
returned error for each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/event-ledger-deferred-flush"
)

func main() {
	publish := func(events []ledger.Event) {
		fmt.Printf("published %d events:\n", len(events))
		for _, e := range events {
			fmt.Printf("  %s\n", e.Name)
		}
	}

	fmt.Println("-- success --")
	err := ledger.Write("order-1", false, publish)
	fmt.Println("error:", err)

	fmt.Println("-- commit failure --")
	err = ledger.Write("order-2", true, publish)
	fmt.Println("error:", err)

	fmt.Println("-- validation failure --")
	err = ledger.Write("", false, publish)
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
-- success --
published 3 events:
  validated:order-1
  processed:order-1
  committed
error: <nil>
-- commit failure --
error: ledger: commit failed
-- validation failure --
error: ledger: validation failed
```

### Tests

`TestWritePublishesOnlyOnCommit` drives `Write` over all three outcomes and
asserts both the returned error and whether `publish` ran at all.

Create `ledger_test.go`:

```go
package ledger

import (
	"errors"
	"testing"
)

func TestWritePublishesOnlyOnCommit(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		failCommit    bool
		wantErr       error
		wantPublished bool
	}{
		{"validation failure suppresses publish", "", false, ErrValidation, false},
		{"commit failure suppresses publish", "order-2", true, ErrCommit, false},
		{"success publishes all events", "order-1", false, nil, true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var published []Event
			publish := func(events []Event) { published = events }

			err := Write(tc.input, tc.failCommit, publish)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}

			if tc.wantPublished && len(published) == 0 {
				t.Fatal("expected events to be published, got none")
			}
			if !tc.wantPublished && published != nil {
				t.Fatalf("expected no publish, got %d events", len(published))
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The property under test is that `publish` fires exactly when the
transaction really committed, and never on either failure branch, no matter
which one fired or how many events had already been recorded before it did.
That property is only true because the deferred closure branches on the
function's named `err`, evaluated after every earlier assignment to it has
already happened. The mistake this design heads off is publishing eagerly
inside the success branch itself — which looks correct until a later commit
step is added after that branch and can now fail after the events already
went out.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — deferred functions can access and modify named result parameters.
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer) — the canonical commit/rollback idiom this ledger extends to event publication.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-connection-pool-health-check.md](15-connection-pool-health-check.md) | Next: [17-request-context-deadline-deferred-cleanup.md](17-request-context-deadline-deferred-cleanup.md)
