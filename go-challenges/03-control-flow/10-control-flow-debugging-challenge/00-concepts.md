# Control-Flow Debugging Challenge — Concepts

Chapter 03 does not close with another syntax drill. It closes with the skill a
senior backend engineer actually spends the day on: reading failing output and
reasoning about control flow. You write a branch once; you read the branch that
misbehaves in production a hundred times. Every module in this lesson ships a
production-shaped artifact — a connection state machine, a bounded retry, a batch
importer, an HTTP status classifier, an inventory reconciler, a worker select
loop, a panic-recovery middleware, a duplicate scanner, an event router — that
carries a planted control-flow defect and a test that pins the correct behavior.
The deliverable is the loop: read the failure as data, form a hypothesis from the
code, apply the minimal fix, re-run under `-race`, confirm the invariant. The
bugs are the ones that pass casual review and reach production: a terminal-state
transition accepted as a no-op, an off-by-one in a bounded retry, `defer`
accumulating file descriptors until the function returns, a `range` value-copy
mutation that silently does nothing, a `select` loop that leaks a goroutine
because it never watches `ctx.Done()`, a `recover` that answers 200 for a panic,
an inner `break` that fails to leave the outer loop, and a type-switch `default`
that hides an unhandled event. Read this file once; it is the model behind all
nine independent exercises that follow.

## Concepts

### Debugging is a loop, not a guess

The failure message is structured data, not noise. Go's test output names the
file, the line, the expected value, and the observed value. In the state-machine
module the line `Close on closed = <nil>, want ErrInvalidTransition` points
straight at a missing guard: the second `Close()` returned no error. The
discipline is to read that line *before* touching the implementation. The
hypothesis follows from the data — "a second close returns nil, so the code must
be accepting the closed→closed transition somewhere" — and the hypothesis tells
you exactly which lines to inspect. Changing code first and iterating until green
is how a one-line fix consumes an afternoon: you are searching a space blindly
instead of reading the map the test already handed you.

### A state machine is guard clauses at the boundary

Every `Transition(from, to)` is a set of preconditions encoded once. A connection
that is `Closed` cannot go to `Connected`; a connection that is `New` cannot jump
to `Authenticated`. Encoding those rules in one place — a
`map[State]map[State]bool` transition table — means the rest of the system never
has to defend against invalid states. The table is the source of truth, and its
lookup is constant time. The subtle failure is a same-state transition: a table
entry `Closed → Closed` looks harmless but turns a second close into a silent
no-op. A `from == to` short-circuit at the top of `Transition` generalizes to
every terminal state at once; adding the rule per-state in the table would need a
fresh edit for each new terminal state you invent later.

### Loop-boundary bugs are the most common control-flow defect

Off-by-one, a misplaced `break`, a stray `continue`, an early `return` inside the
loop — these change iteration counts by one and are invisible to a test that only
checks success versus failure. `for i := range n` runs exactly `n` times, indices
`0..n-1`. A bounded retry that calls `fn` before checking the counter runs one
too many times; one that returns inside the loop before the final attempt runs
one too few; a `continue` placed above the backoff skips the sleep it was meant
to precede. The defense is to test the *exact* number of iterations or attempts
with a call counter, not merely that the operation eventually succeeded or
failed.

### defer runs at function return, not loop-iteration end

`defer f.Close()` inside a `for` loop registers one deferred call per iteration
and releases nothing until the enclosing function returns. Over a small batch it
looks fine; over ten thousand files it exhausts the process's file descriptors,
and the same shape leaks database rows, network connections, and held locks under
load. Cleanup that must happen once per iteration belongs in a per-iteration
closure or helper function — where the `defer` fires at *that* function's return —
or as an explicit `Close()` on every exit path of the loop body, including the
error path. Moving the body into `func() error { ... }` and calling it per
iteration is the idiomatic fix: the `defer` now scopes to the iteration.

### switch does not fall through in Go

Each `case` in a Go `switch` breaks implicitly; control does not slide into the
next case as it does in C. An explicit `fallthrough` transfers to the *next
case's body unconditionally* — it does not re-evaluate that case's condition — so
a `fallthrough` in a `4xx` arm dumps that code into the `5xx` arm regardless of
the value. A missing `default` returns the zero value for any unmatched input,
which in a status classifier silently mislabels an out-of-range code and in a
dispatcher silently drops a message. Both are classic misroute bugs. A tagless
`switch { case cond: ... }` is the readable form for range checks, and the order
of arms matters: put the specific case (429 is retryable) before the broad range
(`400..499` is a client error).

### range copies

`for _, v := range items` binds `v` to a *copy* of each element; assigning to
`v.Field` mutates the copy and never touches the backing array. This is the
single most common silent no-op in Go slice processing. To mutate in place, index
the slice — `items[i].Field = ...` — or range over `[]*T` so the loop variable is
a pointer to the shared struct. Ranging over a slice of pointers works because the
copied pointer still addresses the same struct; ranging over a slice of values and
writing through the copy does not. A test that asserts the *original* slice
reflects the mutation catches this instantly; one that reads the loop variable
does not.

### select with context is how goroutines shut down

A worker built as `for { select { ... } }` needs an explicit
`case <-ctx.Done(): return` arm. Omit it and the goroutine keeps running after
cancellation — a leak that outlives the request, the connection, or the whole
graceful-shutdown window. Adding a `default:` arm is the opposite failure: it
turns a blocking select into a busy-spin that pins a core at 100% because the
loop never parks. Ticker-driven work must `Stop()` the ticker on exit so its
runtime resources are released. The way to prove shutdown is a `sync.WaitGroup`
plus a deadline: start the worker, cancel, and assert `Wait()` returns before a
`time.After` fires — under `-race`, so the shutdown handshake is checked for data
races too.

### recover must own the control flow after it recovers

A `recover()` at a stable boundary — the top of an HTTP handler chain, a worker's
job loop — is legitimate: it contains a panic instead of crashing the process.
But once it recovers it must *fully own* what happens next: log the panic with
context and write exactly one terminal response, an HTTP 500. The failure is a
`recover` that swallows the panic and then falls through, letting the framework's
default 200 stand, or returns without writing any status at all. That reports
success for a request that blew up — it hides the incident instead of containing
it. Because a status line can only be committed once, the middleware should track
whether a header was already written and only synthesize the 500 when nothing has
been sent yet, so it never emits a superfluous second `WriteHeader`.

### labeled break and continue exist for nested loops

A plain `break` or `continue` affects only the innermost enclosing loop. When the
decision to stop is made in an inner loop but must terminate the *outer* scan —
"I found the first cross-batch duplicate, stop everything" — a plain `break`
leaves the inner loop and the outer loop keeps going, so the scan reports a later
match or double-counts. The fix is a labeled break: `break Outer`. Labeled
`continue Outer` is its counterpart, advancing the outer loop from within the
inner one. `goto` exists for forward jumps to shared cleanup, but labeled
break/continue cover almost every real nested-loop case more readably.

### a type switch is a dispatch table, and its default is a policy

`switch v := e.(type)` routes on the dynamic type of an interface value; it is a
dispatch table over concrete types. Its `default` arm is a deliberate policy
choice, not a harmless fallback. You decide between handling every known type and
returning a typed `ErrUnhandledEvent` for the rest. A `default` that returns `nil`
silently drops any type you forgot to add a case for, which in an event pipeline
looks exactly like data loss with no error to page on. Note that `fallthrough` is
*not permitted* in a type switch, so a "leak into the next arm" bug there takes a
different form — usually a copy-pasted case body that files an event under the
wrong bucket. The nil-interface value matches `default` (or an explicit
`case nil:`), so pin that path too.

## Common Mistakes

### Changing the implementation before reading the failure output

Wrong: read the test, edit the code, re-run, repeat until green. Fix: read the
failure message first. It already names the line and the expected-versus-observed
values; the hypothesis follows from that data. In the state-machine module the
line is `Close on closed = <nil>`, and that exact message tells you where to look.

### Asserting that every action in a sequence fails

Wrong: a test that loops over a mix of valid and invalid actions and asserts an
error on each — the first valid action passes and the test fails confusingly.
Fix: split into a setup phase that reaches a known state and a single-action
assertion on the transition under test, as `TestClosedToClosedIsRejected` does:
setup runs `Connect` and `Close`, and the assertion is on the second `Close`.

### Asserting on error strings with ==

Wrong: `if err.Error() == "invalid state transition"`. The moment the message
gains context via `%w` the comparison breaks. Fix: `errors.Is(err, ErrSentinel)`
against a package-level sentinel, or `errors.As` for a typed error.

### Putting defer resource.Close() inside a for loop

Wrong: assuming the `defer` runs each iteration — it runs at function return, so a
large batch leaks descriptors. Fix: a per-iteration helper or closure whose
`defer` scopes to the iteration, or an explicit `Close()` on every exit path
including the error path.

### Assuming Go switch falls through like C

Wrong: expecting the next case to execute automatically, or adding `fallthrough`
and expecting the next case's condition to be re-checked — `fallthrough` enters
the next case's body unconditionally. Fix: rely on implicit break, add
`fallthrough` only when you mean the unconditional jump, and always provide a
`default` for exhaustiveness.

### Mutating the range value variable and expecting the slice to change

Wrong: `for _, v := range items { v.X = ... }`. `v` is a copy. Fix: index with
`items[i].X = ...`, or range over `[]*T`.

### A worker select loop with no exit and no blocking path

Wrong: `for { select { case j := <-jobs: ... } }` with no `case <-ctx.Done():
return` (goroutine leak) or with a `default:` that busy-spins the CPU. Fix: give
the loop an explicit cancellation arm and keep every other arm blocking.

### A recover that logs nothing and reports success

Wrong: `defer func() { recover() }()` that swallows the panic and lets a 200
stand. Fix: during development let the panic propagate; at a boundary, recover,
log with context, and write a terminal 500 exactly once.

### A plain break inside nested loops when you meant to leave the outer loop

Wrong: `break` when the intent is to stop the whole scan — the outer loop keeps
running. Fix: `break Outer` with a label on the outer `for`.

### Adding a state or event type but forgetting the table or switch entry

Wrong: extend the `iota` block or the event set and forget the matching
`allowed` entry or type-switch case, so the new value returns `ErrUnknownState`
or gets silently dropped. Fix: the map and the switch are the source of truth;
update them in lockstep with the type set.

Next: [01-connection-state-machine-closed-twice.md](01-connection-state-machine-closed-twice.md)
