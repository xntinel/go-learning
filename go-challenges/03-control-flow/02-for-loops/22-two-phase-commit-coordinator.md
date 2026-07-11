# Exercise 22: Two-Phase Commit Coordination with Deadline and Abort

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A distributed transaction across an inventory service and a payments service
needs every participant to agree it *can* commit before any of them actually
does — and if one participant is slow to respond, the whole transaction has
to give up and tell everyone to abort rather than commit some services and
not others. This module builds a small two-phase-commit coordinator: an
outer loop drives the prepare phase per participant, an inner loop polls
each one's readiness against a shared deadline, and a labeled `break` is what
lets one participant's timeout abort the *entire* transaction instead of
just its own poll.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
twopc/                         module example.com/twopc
  go.mod                       go 1.24
  twopc.go                     Participant; Config; Run(cfg) (bool, error); ErrDeadlineExceeded
  twopc_test.go                  all ready, polled readiness, prepare error, deadline mid-poll, commit error
  cmd/demo/
    main.go                     two participants, one needs a second poll before it is ready
```

- Files: `twopc.go`, `twopc_test.go`, `cmd/demo/main.go`.
- Implement: `Run(cfg Config) (bool, error)` — a labeled outer `for _, p := range cfg.Participants` prepare loop, each iteration wrapping an inner `for { ... }` that polls `p.Prepare()` until ready or the deadline passes, and a plain `for _, p := range cfg.Participants` commit loop that only runs once every participant is ready.
- Test: every participant ready on the first poll; a participant that needs several polls before it is ready; a participant erroring during prepare (abort everyone); the deadline expiring mid-poll (abort everyone, and the labeled break must skip any participant not yet reached); a commit error after every participant prepared (no abort — commit is the point of no return).
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/twopc/cmd/demo
cd ~/go-exercises/twopc
go mod init example.com/twopc
go mod edit -go=1.24
```

### Why the outer loop needs a label, not just the inner one

`Run` has two loops nested for a genuine reason: the outer loop's unit of
work is "get this participant to ready," the inner loop's unit of work is
"poll once and check the clock." The inner loop's own two exits — `ready ==
true` (plain `break`, move on to the next participant) and "deadline passed"
— are not equivalent, and that is exactly where the label earns its keep. A
plain `break` on deadline would only stop polling *that* participant; the
outer `range` would then move on and start preparing the *next* participant
anyway, which is precisely backwards — once the shared deadline has passed,
no further participant should be asked to prepare at all, because the
transaction is already going to abort. `break prepare` exits the outer loop
in the same statement, so the moment the clock runs out, every participant
after the current one is left completely untouched — never even asked.

The other production detail worth noting is the deliberate asymmetry between
the two phases: `abortAll` is only ever called *before* any `Commit` runs.
Once the commit loop starts, a failure there is reported as an error but
does **not** trigger an abort of the already-committed participants — real
two-phase-commit protocols treat commit as the point of no return precisely
because a participant that has committed cannot always be un-committed, and
mixing "abort" semantics into the commit phase would silently claim a
rollback happened when it may not have. `TestRunCommitErrorDoesNotAbortAlreadyCommitted`
is the test that pins this down.

Create `twopc.go`:

```go
package twopc

import (
	"errors"
	"fmt"
	"time"
)

// ErrDeadlineExceeded means the prepare phase did not finish before Deadline.
var ErrDeadlineExceeded = errors.New("twopc: deadline exceeded during prepare")

// Participant is one resource manager taking part in the transaction.
// Prepare may need to be polled: it returns (false, nil) to mean "not ready
// yet, ask again," not an error.
type Participant struct {
	Name    string
	Prepare func() (ready bool, err error)
	Commit  func() error
	Abort   func() error
}

// Config groups everything Run needs to drive one transaction.
type Config struct {
	Participants []Participant
	PollInterval time.Duration
	Deadline     time.Time
	Now          func() time.Time
	Sleep        func(time.Duration)
}

// Run drives a two-phase-commit transaction: every participant must reach
// "ready" during the prepare phase before any commit happens, and the whole
// prepare phase is bounded by cfg.Deadline. If any participant errors, or the
// deadline is hit before every participant is ready, every participant is
// told to abort and no commit is ever issued -- once Commit is called for one
// participant, this implementation no longer aborts, matching how real 2PC
// commit phases behave (commit is the point of no return).
//
// The outer loop over cfg.Participants drives the prepare phase; the inner
// loop for each participant polls its readiness, bounded by cfg.Deadline.
// The labeled break on the outer loop is what lets a single participant's
// timeout abort the *whole* transaction immediately, instead of only ending
// that one participant's poll and letting the loop move on to ask the next
// participant to prepare anyway.
func Run(cfg Config) (bool, error) {
prepare:
	for _, p := range cfg.Participants {
		for {
			ready, err := p.Prepare()
			if err != nil {
				abortAll(cfg.Participants)
				return false, fmt.Errorf("prepare %s: %w", p.Name, err)
			}
			if ready {
				break
			}
			if !cfg.Now().Before(cfg.Deadline) {
				break prepare
			}
			cfg.Sleep(cfg.PollInterval)
		}
	}

	if !cfg.Now().Before(cfg.Deadline) {
		abortAll(cfg.Participants)
		return false, ErrDeadlineExceeded
	}

	for _, p := range cfg.Participants {
		if err := p.Commit(); err != nil {
			return false, fmt.Errorf("commit %s: %w", p.Name, err)
		}
	}
	return true, nil
}

// abortAll asks every participant to abort, best-effort. Errors are ignored:
// once prepare has failed or timed out, abort is a cleanup step, and a
// participant that also fails to abort will be reconciled out-of-band.
func abortAll(participants []Participant) {
	for _, p := range participants {
		_ = p.Abort()
	}
}
```

### The runnable demo

The demo drives two participants: `inventory` is ready on its first
`Prepare` call, `payments` needs a second poll before it reports ready. Both
then commit successfully.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/twopc"
)

type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time          { return c.t }
func (c *manualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func main() {
	clock := &manualClock{t: time.Unix(0, 0)}

	polls := 0
	cfg := twopc.Config{
		Participants: []twopc.Participant{
			{
				Name:    "inventory",
				Prepare: func() (bool, error) { return true, nil },
				Commit:  func() error { fmt.Println("inventory: committed"); return nil },
				Abort:   func() error { fmt.Println("inventory: aborted"); return nil },
			},
			{
				Name: "payments",
				Prepare: func() (bool, error) {
					polls++
					fmt.Printf("payments: prepare poll #%d\n", polls)
					return polls >= 2, nil
				},
				Commit: func() error { fmt.Println("payments: committed"); return nil },
				Abort:  func() error { fmt.Println("payments: aborted"); return nil },
			},
		},
		PollInterval: 50 * time.Millisecond,
		Deadline:     clock.t.Add(time.Second),
		Now:          clock.Now,
		Sleep:        func(d time.Duration) { clock.Advance(d) },
	}

	ok, err := twopc.Run(cfg)
	fmt.Printf("committed=%v err=%v\n", ok, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
payments: prepare poll #1
payments: prepare poll #2
inventory: committed
payments: committed
committed=true err=<nil>
```

### Tests

The suite walks through five scenarios: the trivial all-ready case; polled
readiness (asserting the exact poll count); a prepare error that must abort
every participant, including ones that already reported ready;
`TestRunDeadlineDuringPollAbortsAndSkipsRemainingParticipants`, the sharpest
one, which puts a participant that never becomes ready first and confirms
the *second* participant's `Prepare` is never called at all once the
deadline trips; and the commit-phase edge case where a failure after
everyone prepared must not trigger an abort of the participant that already
committed. A `manualClock` advanced only by `Sleep` calls keeps every
assertion deterministic without a real wall-clock wait.

Create `twopc_test.go`:

```go
package twopc

import (
	"errors"
	"testing"
	"time"
)

type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time          { return c.t }
func (c *manualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func alwaysReady() (bool, error) { return true, nil }
func noopErr() error             { return nil }

func TestRunAllReadyImmediatelyCommits(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	var commits []string
	commit := func(name string) func() error {
		return func() error {
			commits = append(commits, name)
			return nil
		}
	}

	cfg := Config{
		Participants: []Participant{
			{Name: "a", Prepare: alwaysReady, Commit: commit("a"), Abort: noopErr},
			{Name: "b", Prepare: alwaysReady, Commit: commit("b"), Abort: noopErr},
		},
		PollInterval: 10 * time.Millisecond,
		Deadline:     clock.t.Add(time.Second),
		Now:          clock.Now,
		Sleep:        func(d time.Duration) { clock.Advance(d) },
	}

	ok, err := Run(cfg)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Fatal("committed = false, want true")
	}
	if len(commits) != 2 || commits[0] != "a" || commits[1] != "b" {
		t.Fatalf("commits = %v, want [a b]", commits)
	}
}

func TestRunPollsUntilReadyThenCommits(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	polls := 0
	slowPrepare := func() (bool, error) {
		polls++
		return polls >= 3, nil // ready on the third poll
	}

	cfg := Config{
		Participants: []Participant{
			{Name: "slow", Prepare: slowPrepare, Commit: noopErr, Abort: noopErr},
		},
		PollInterval: 100 * time.Millisecond,
		Deadline:     clock.t.Add(time.Second),
		Now:          clock.Now,
		Sleep:        func(d time.Duration) { clock.Advance(d) },
	}

	ok, err := Run(cfg)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Fatal("committed = false, want true")
	}
	if polls != 3 {
		t.Fatalf("polls = %d, want 3", polls)
	}
}

func TestRunAbortsAllOnPrepareError(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	var aborted []string
	abort := func(name string) func() error {
		return func() error {
			aborted = append(aborted, name)
			return nil
		}
	}
	errBroken := errors.New("participant unreachable")

	cfg := Config{
		Participants: []Participant{
			{Name: "a", Prepare: alwaysReady, Commit: noopErr, Abort: abort("a")},
			{Name: "b", Prepare: func() (bool, error) { return false, errBroken }, Commit: noopErr, Abort: abort("b")},
		},
		PollInterval: 10 * time.Millisecond,
		Deadline:     clock.t.Add(time.Second),
		Now:          clock.Now,
		Sleep:        func(d time.Duration) { clock.Advance(d) },
	}

	ok, err := Run(cfg)
	if ok {
		t.Fatal("committed = true, want false")
	}
	if !errors.Is(err, errBroken) {
		t.Fatalf("err = %v, want wrapping %v", err, errBroken)
	}
	if len(aborted) != 2 {
		t.Fatalf("aborted = %v, want both a and b aborted", aborted)
	}
}

func TestRunDeadlineDuringPollAbortsAndSkipsRemainingParticipants(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	secondPrepareCalls := 0

	cfg := Config{
		Participants: []Participant{
			{
				Name:    "stuck",
				Prepare: func() (bool, error) { return false, nil }, // never ready
				Commit:  noopErr,
				Abort:   noopErr,
			},
			{
				Name: "never-reached",
				Prepare: func() (bool, error) {
					secondPrepareCalls++
					return true, nil
				},
				Commit: noopErr,
				Abort:  noopErr,
			},
		},
		PollInterval: 100 * time.Millisecond,
		Deadline:     clock.t.Add(250 * time.Millisecond),
		Now:          clock.Now,
		Sleep:        func(d time.Duration) { clock.Advance(d) },
	}

	ok, err := Run(cfg)
	if ok {
		t.Fatal("committed = true, want false")
	}
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
	}
	if secondPrepareCalls != 0 {
		t.Fatalf("second participant's Prepare called %d times, want 0 (labeled break should skip it)", secondPrepareCalls)
	}
}

func TestRunCommitErrorDoesNotAbortAlreadyCommitted(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	aAborted := false
	errCommitFailed := errors.New("disk full")

	cfg := Config{
		Participants: []Participant{
			{Name: "a", Prepare: alwaysReady, Commit: noopErr, Abort: func() error { aAborted = true; return nil }},
			{Name: "b", Prepare: alwaysReady, Commit: func() error { return errCommitFailed }, Abort: noopErr},
		},
		PollInterval: 10 * time.Millisecond,
		Deadline:     clock.t.Add(time.Second),
		Now:          clock.Now,
		Sleep:        func(d time.Duration) { clock.Advance(d) },
	}

	ok, err := Run(cfg)
	if ok {
		t.Fatal("committed = true, want false")
	}
	if !errors.Is(err, errCommitFailed) {
		t.Fatalf("err = %v, want wrapping %v", err, errCommitFailed)
	}
	if aAborted {
		t.Fatal("participant a must not be aborted once the commit phase has started")
	}
}
```

## Review

`Run` is correct when a commit only ever happens after *every* participant
reported ready, and when any prepare failure or deadline miss results in
every participant being told to abort with zero commits issued. The common
mistake this design avoids is a single un-labeled `break` on the deadline
check — it would compile and pass the simple all-ready tests, but
`TestRunDeadlineDuringPollAbortsAndSkipsRemainingParticipants` catches it
immediately: without the label, the loop would move on to poll
`never-reached`'s `Prepare` even though the transaction is already doomed to
abort, wasting a call (and, in a real system, doing real work) that can
never matter. Run `go test -count=1 ./...`.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — the labeled form used to abort both phases together.
- [Two-Phase Commit Protocol (Jim Gray, Notes on Data Base Operating Systems)](https://en.wikipedia.org/wiki/Two-phase_commit_protocol) — the prepare/commit state machine this module implements.
- [context package](https://pkg.go.dev/context) — `Context.Deadline`, the production analogue of this module's injected `Deadline`/`Now`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-metrics-time-bucket-roundrobin.md](21-metrics-time-bucket-roundrobin.md) | Next: [23-task-queue-priority-dequeuer.md](23-task-queue-priority-dequeuer.md)
