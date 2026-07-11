# Exercise 1: Hand-Rolled Spy — A Notifier Over a Sender Port

The smallest useful double is a hand-written one. This module builds a `Notifier`
that validates its input and then delegates to a one-method `Sender` port, and
tests it with a hand-rolled, concurrency-safe *spy* that records every call behind
a mutex. It is the baseline the rest of the lesson measures frameworks against: for
a one-method port, this is all a "mock" ever needs to be.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
notifier/                    independent module: example.com/notifier
  go.mod                     go 1.26
  notifier.go                Sender interface; Notifier; New; Notify; ErrEmptyRecipient/ErrEmptyBody
  cmd/
    demo/
      main.go                runnable demo wiring a stdout sender
  notifier_test.go           hand-rolled spy (mockSender) + validation/propagation/order tests
```

- Files: `notifier.go`, `cmd/demo/main.go`, `notifier_test.go`.
- Implement: a `Notifier` holding a `Sender`; `Notify(to, body)` validates then calls `Send`; sentinel errors `ErrEmptyRecipient`, `ErrEmptyBody`.
- Test: a mutex-guarded spy asserting one recorded call with the right `{to, body}`; zero calls when validation fails; the sender's error propagated via `errors.Is`; two `Notify` calls recorded in order.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/notifier/cmd/demo
cd ~/go-exercises/notifier
go mod init example.com/notifier
```

### Why the port is one method

`Notifier` needs exactly one capability from the outside world: the ability to
send a message. So the interface it defines is one method, `Send(to, body string)
error`. That is a deliberate design choice, not an accident of this toy: the
consumer declares the narrowest seam it needs, and any concrete sender — an SMTP
adapter, a Twilio client, a stdout printer in the demo, a spy in the test —
satisfies it implicitly. A one-method interface produces a two-line double. Widen
the interface and every double widens with it; that coupling is the reason to keep
consumer-side interfaces small.

`Notify` does its own validation *before* it touches the sender. Sending to an
empty recipient or with an empty body is a client error, and the point of catching
it early is that the expensive collaborator is never invoked. The tests pin exactly
that: on a validation failure the spy records *zero* calls. This "no-call-on-failure"
property is one of the few places interaction verification is genuinely the
contract — there is no returned state that captures "the sender was skipped," only
the absence of a call.

Create `notifier.go`:

```go
package notifier

import "errors"

// Sentinel errors let callers match with errors.Is regardless of wrapping.
var (
	ErrEmptyRecipient = errors.New("empty recipient")
	ErrEmptyBody      = errors.New("empty body")
)

// Sender is the one-method port the Notifier depends on. Production wires an
// SMTP/SMS adapter; tests wire a spy. Kept to a single method so the double
// stays a single method.
type Sender interface {
	Send(to, body string) error
}

// Notifier validates a message and delegates delivery to a Sender.
type Notifier struct {
	sender Sender
}

// New injects the Sender through the constructor (dependency injection). No
// global, no init-time dial.
func New(sender Sender) *Notifier {
	return &Notifier{sender: sender}
}

// Notify validates then sends. Validation short-circuits before Send, so an
// invalid message never reaches the collaborator.
func (n *Notifier) Notify(to, body string) error {
	if to == "" {
		return ErrEmptyRecipient
	}
	if body == "" {
		return ErrEmptyBody
	}
	return n.sender.Send(to, body)
}
```

### The runnable demo

The demo wires a trivial real `Sender` that prints to stdout, so you can watch the
delegation happen and see validation reject a bad message.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/notifier"
)

// stdoutSender is a real (if minimal) Sender: it satisfies the port by printing.
type stdoutSender struct{}

func (stdoutSender) Send(to, body string) error {
	fmt.Printf("sent to %s: %s\n", to, body)
	return nil
}

func main() {
	n := notifier.New(stdoutSender{})

	if err := n.Notify("alice@example.com", "your order shipped"); err != nil {
		fmt.Println("error:", err)
	}

	// A validation failure never reaches the sender.
	if err := n.Notify("", "ignored"); err != nil {
		fmt.Println("rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sent to alice@example.com: your order shipped
rejected: empty recipient
```

### The hand-rolled spy and the tests

`mockSender` is a spy: it answers like a stub (returns its preprogrammed `err`) and
records every `{to, body}` it was called with. Because a test might one day drive
the notifier from multiple goroutines — and because the `-race` detector is part of
the gate — the recorder is guarded by a `sync.Mutex`: append under the lock, and
hand a *copy* of the slice out of `Calls()` under the lock so a reader never races
an in-flight append.

The four tests cover the whole contract. `TestNotifyCallsSender` is state-plus-
interaction: it asserts exactly one call with the right arguments (checking *what*
was sent, not merely that something was). `TestNotifyRejectsEmptyRecipient` and
`...EmptyBody` assert the short-circuit — the error matches the sentinel via
`errors.Is`, and the spy recorded zero calls. `TestNotifyPropagatesSenderError`
proves a sender failure is returned unchanged. `TestNotifyCallsSenderInOrder`
pins the ordering the original lesson asked learners to add: two `Notify` calls
land in the recorder in the order they were made.

Create `notifier_test.go`:

```go
package notifier

import (
	"errors"
	"sync"
	"testing"
)

// sendCall captures the arguments of one Send invocation.
type sendCall struct {
	to   string
	body string
}

// mockSender is a hand-rolled spy: a stub (returns err) that records calls.
// The recorder is mutex-guarded so it is safe under -race.
type mockSender struct {
	mu    sync.Mutex
	calls []sendCall
	err   error
}

func (m *mockSender) Send(to, body string) error {
	m.mu.Lock()
	m.calls = append(m.calls, sendCall{to: to, body: body})
	m.mu.Unlock()
	return m.err
}

// Calls returns a copy of the recorded calls, taken under the lock.
func (m *mockSender) Calls() []sendCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sendCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func TestNotifyCallsSender(t *testing.T) {
	t.Parallel()

	s := &mockSender{}
	n := New(s)
	if err := n.Notify("alice", "hello"); err != nil {
		t.Fatalf("Notify returned error: %v", err)
	}

	calls := s.Calls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(calls))
	}
	if calls[0].to != "alice" || calls[0].body != "hello" {
		t.Fatalf("recorded call = %+v, want {alice hello}", calls[0])
	}
}

func TestNotifyRejectsEmptyRecipient(t *testing.T) {
	t.Parallel()

	s := &mockSender{}
	n := New(s)
	if err := n.Notify("", "hello"); !errors.Is(err, ErrEmptyRecipient) {
		t.Fatalf("err = %v, want ErrEmptyRecipient", err)
	}
	if calls := s.Calls(); len(calls) != 0 {
		t.Fatalf("sender called %d times on validation failure, want 0", len(calls))
	}
}

func TestNotifyRejectsEmptyBody(t *testing.T) {
	t.Parallel()

	s := &mockSender{}
	n := New(s)
	if err := n.Notify("alice", ""); !errors.Is(err, ErrEmptyBody) {
		t.Fatalf("err = %v, want ErrEmptyBody", err)
	}
	if calls := s.Calls(); len(calls) != 0 {
		t.Fatalf("sender called %d times on validation failure, want 0", len(calls))
	}
}

func TestNotifyPropagatesSenderError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("smtp: connection refused")
	s := &mockSender{err: sentinel}
	n := New(s)
	if err := n.Notify("alice", "hello"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the sender's error", err)
	}
}

func TestNotifyCallsSenderInOrder(t *testing.T) {
	t.Parallel()

	s := &mockSender{}
	n := New(s)
	if err := n.Notify("alice", "first"); err != nil {
		t.Fatal(err)
	}
	if err := n.Notify("bob", "second"); err != nil {
		t.Fatal(err)
	}

	calls := s.Calls()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(calls))
	}
	if calls[0].to != "alice" || calls[1].to != "bob" {
		t.Fatalf("order = [%s, %s], want [alice, bob]", calls[0].to, calls[1].to)
	}
}
```

## Review

The notifier is correct when validation is a pure gate in front of delegation:
`Notify` returns `ErrEmptyRecipient` or `ErrEmptyBody` without ever calling `Send`,
and otherwise returns exactly whatever `Send` returns. The spy proves both halves —
the argument-capturing test proves the right `{to, body}` reached the sender, and
the zero-call assertions prove the short-circuit. Two traps are worth naming.
First, do not assert only that the sender was *called*; assert *what* it was called
with, or a future bug that sends the wrong body sails through green. Second, do not
drop the mutex: append and the slice-copy in `Calls()` must both hold the lock, or
`-race` will flag the spy (correctly) the moment a test drives the notifier
concurrently. Run `go test -race` to confirm.

## Resources

- [testing](https://pkg.go.dev/testing) — the standard testing package the spy and tests are built on.
- [Effective Go: interfaces](https://go.dev/doc/effective_go#interfaces) — why the consumer defines the narrow interface it needs.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors, used by the propagation test.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-test-double-taxonomy-fake-repository.md](02-test-double-taxonomy-fake-repository.md)
