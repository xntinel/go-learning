# Durable Execution with Temporal — Concepts

A distributed saga has no ACID transaction spanning the inventory, payments, and
shipping services. Partial failure is the normal case, not the exception: the
charge succeeds and then the shipment call times out, and now money is debited
with no order to show for it. The senior job is to make that outcome recover
deterministically instead of leaking money and support tickets. You can hand-roll
this — a saga state table, a cron relay, a resume-from-checkpoint scheme, a retry
scheduler — and you will spend the next year finding the edges. Temporal reframes
the problem: the workflow function is *durable code* whose entire execution
history is persisted and replayed on crash, so control flow, retries, timers, and
compensations survive process death, deploys, and multi-day waits without you
writing any of that machinery by hand. This file is the conceptual foundation for
the three exercises that follow; read it once and the code will make sense.

## Concepts

### The durability model: history, not RAM

A Temporal workflow is not a long-running process sitting in memory. It is a
function whose every meaningful decision — start this activity, sleep until this
time, receive this signal — is recorded to an append-only event history stored in
the Temporal service. When a worker crashes, is redeployed, or picks the workflow
up hours later, the SDK *replays* the workflow function from the beginning against
that history: it re-runs your Go code, and each command that already has a result
in history returns that recorded result instead of doing the work again. Execution
resumes exactly where it left off. State lives in the history, not in RAM, which is
why a workflow can `workflow.Sleep` for thirty days across dozens of worker
restarts and wake up with its local variables intact. This is the whole reason to
reach for Temporal over a saga table plus a cron relay: the resume mechanism you
would otherwise build is the platform.

### The determinism contract, and why it is non-negotiable

Because the workflow function is replayed, it must produce the *identical* sequence
of commands every time it runs against the same history. If replay diverges — a
different activity is scheduled, in a different order — the SDK cannot line the new
run up against the recorded events and it fails the task with a non-determinism
error. So every source of nondeterminism is banned from workflow code: wall-clock
time (`time.Now`), randomness (`rand`, `uuid.New`), map-iteration order, direct
network or database calls, and native goroutines. Each has an SDK replacement that
records its result into history so replay reproduces it: `workflow.Now` instead of
`time.Now`, `workflow.SideEffect` for a one-off nondeterministic value,
`workflow.Go`/`workflow.NewChannel`/`workflow.NewSelector` instead of `go` and raw
channels, `workflow.Sleep` instead of `time.Sleep`, and — above all —
`workflow.ExecuteActivity` for anything that touches the outside world. This is the
single hardest thing for an engineer new to Temporal to internalize, and it is the
thing that makes everything else work.

### Workflow versus activity: where effects are allowed to happen

The split is the core design skill. A *workflow* orchestrates: it decides what to
do and in what order, and it must be deterministic and free of side effects. An
*activity* is an ordinary Go function that performs one real-world action — charge
a card, write a row, call a gRPC service. Activities are the *only* place effects
happen. They run outside the replay constraint, so `time.Now`, network calls, and
randomness are all fine inside them. They are dispatched by the workflow, retried
independently under a policy the workflow owns, can time out, and can heartbeat for
long work. Note the two context types this creates: a workflow function receives a
`workflow.Context` (a deterministic, replay-aware context), while an activity
receives a plain `context.Context` as its first parameter. They are different types
with different rules, and confusing them is a common early error.

### Saga is compensation, not rollback

There is no distributed ACID transaction across the inventory, payment, and
shipping services, so "roll back" is not available. A saga instead executes forward
steps and, when a later step fails, runs *compensating actions* in reverse order
(LIFO) to semantically undo the work already committed. Compensation is a
business-level inverse — refund the charge, release the reservation, cancel the
shipment — not a storage rollback, and it operates on committed state. Two
consequences follow. First, a compensation is registered only *after* its forward
step succeeds, so a step that never ran is never undone. Second, a compensation can
itself fail (the refund gateway is down), so its error must be aggregated with the
original failure and surfaced, never swallowed. In Go the clean way to aggregate is
`errors.Join`, which returns `nil` when given no errors and otherwise a single
error that `errors.Is` can still inspect for each cause.

### Idempotency is mandatory, not optional

Temporal delivers activities *at least once*. An activity can run twice: a worker
completes the charge, then crashes before recording the result, and on retry the
same activity runs again; or a timeout fires on a call that actually succeeded.
Both forward activities and compensations must therefore be idempotent, or retries
double-charge and double-refund. The standard technique is an idempotency key
derived from stable inputs — the workflow ID plus the step name — passed to the
downstream service so it can deduplicate. Temporal removes the orchestration
problem; it does not remove this one. It moves the hard part of distributed
consistency to two places you can actually reason about and unit-test: idempotent
activities and explicit compensation ordering.

### Retryable versus terminal failures

Not every error deserves a retry. Infrastructure faults — a 503 from the payment
gateway, a database timeout, a reset connection — are transient and should be
retried with capped exponential backoff. Business rejections — insufficient funds,
an invalid card, a validation failure — are terminal: retrying them wastes the
retry budget, delays the compensation, and loads a downstream that will keep saying
no. The retry policy is orchestration *policy*, owned by the workflow, not by the
activity: `temporal.RetryPolicy` carries `InitialInterval`, `BackoffCoefficient`,
`MaximumInterval`, `MaximumAttempts`, and `NonRetryableErrorTypes`. An activity
signals "do not retry me" by returning
`temporal.NewNonRetryableApplicationError(message, errType, cause)`, which Temporal
fails fast; an ordinary error is retried under the policy. The workflow can then
inspect the returned error with `errors.As` against `*temporal.ApplicationError`
and branch on its `Type()`.

### Compensation under cancellation

Here is the failure mode that is invisible on the happy path and becomes a
production incident. When a workflow is cancelled — a client requested cancel, or a
parent cancelled a child — its `workflow.Context` enters a cancelled state. Any
activity you schedule on that cancelled context fails *immediately* with a cancelled
error, so a compensation scheduled on the inherited context silently does not run,
and the rollback you carefully wrote never executes.
`workflow.NewDisconnectedContext(parent)` returns a fresh context detached from the
parent's cancellation (plus its own cancel function), and scheduling compensations
on *that* context lets cleanup complete even though the workflow itself is being
cancelled. A saga helper that does not do this looks correct in every test that
does not cancel, and loses money the first time someone cancels an order mid-flight.

### Testing without a cluster

None of this requires Docker or a running server to test. `testsuite.WorkflowTestSuite`
gives you an in-memory `TestWorkflowEnvironment` that runs the real workflow logic,
lets you mock each activity with `OnActivity(fn, matchers...).Return(...)`, simulates
timers and retry backoff instantly, and exposes assertions: `ExecuteWorkflow`,
`IsWorkflowCompleted`, `GetWorkflowError`, `GetWorkflowResult`,
`AssertActivityCalled`/`AssertNotCalled`, and `RegisterDelayedCallback` +
`CancelWorkflow` for cancellation scenarios. Saga and compensation logic becomes
ordinary, fast, hermetic unit-testable code — which is exactly how you prove
correctness before it ever touches a cluster. The exercises here are tested this
way end to end; only the client/worker wiring lives behind a build tag because that
is the sole piece that needs a real server.

### Where Temporal fits, and its costs

Temporal replaces bespoke orchestration — outbox plus relay plus state machines
plus schedulers — but it is not free. It adds an operational dependency (the
Temporal service and its datastore). Workflow history has a size limit, so an
unbounded loop must periodically Continue-As-New to start a fresh history.
Deployed workflow code is under a versioning constraint: reordering activities or
inserting a step can break the replay of in-flight executions, so such changes go
through `workflow.GetVersion` (patching). Payloads have size limits; large blobs
belong in object storage with a reference passed through. And knowing when *not* to
reach for it is part of senior judgment: a single-service transaction that a
transactional outbox already covers does not need a workflow engine. Use Temporal
for multi-step, multi-service, long-lived orchestration where partial failure and
recovery are the core problem.

## Common Mistakes

### Doing I/O directly in workflow code

Calling the database, `http.Get`, `time.Now`, `uuid.New`, or `rand` inside a
workflow breaks determinism: on replay these produce different values than the
recorded history and corrupt the execution. Every effect and every nondeterministic
value must go through an activity or an SDK primitive (`workflow.Now`,
`workflow.SideEffect`).

### Using native concurrency primitives in a workflow

`go`, `time.Sleep`, `time.After`, raw channels, and `select` are all
nondeterministic under replay. Use `workflow.Go`, `workflow.Sleep`,
`workflow.NewChannel`, and `workflow.NewSelector` instead; they record their
scheduling into history so replay reproduces it.

### Assuming activities run exactly once

At-least-once delivery means an activity can run twice — a retry after a success the
worker never observed. An activity that is not idempotent double-charges or
double-refunds. Derive an idempotency key from the workflow ID plus step and pass it
to the downstream service.

### Scheduling compensation on the cancelled context

When a workflow is cancelled its context is already cancelled, so a compensation
activity scheduled on it fails instantly and rollback silently does not run. Run
compensations on a `workflow.NewDisconnectedContext` so cleanup executes even under
cancellation.

### Treating every error as retryable

Retrying a business rejection (invalid card, insufficient funds) burns the whole
retry budget before the saga finally compensates, adding latency and load. Mark
terminal errors with `temporal.NewNonRetryableApplicationError` or list their types
in `RetryPolicy.NonRetryableErrorTypes`.

### Swallowing compensation errors

On rollback, dropping a compensation error makes a failed refund invisible. Join it
with the original failure via `errors.Join` so both are observable.

### Compensating in the wrong order or unconditionally

Register a compensation only after its forward step succeeds, and run compensations
in reverse (LIFO) of registration. Registering unconditionally undoes a step that
never happened; running forward-order can undo a later step before the earlier one
it depended on.

### Omitting StartToCloseTimeout

Setting only `ScheduleToCloseTimeout` (or no timeout at all) lets a hung downstream
call block an activity indefinitely with nothing to trigger a retry. Always set
`StartToCloseTimeout` so a stuck attempt times out and the retry policy engages.

### Changing deployed workflow code without versioning

Reordering activities or adding a step to code with in-flight executions breaks
their replay with a non-determinism error. Gate incompatible changes behind
`workflow.GetVersion` (patching).

### Testing against a real server in unit tests

Spinning up a cluster in unit tests makes them slow, flaky, and non-hermetic. Use
`testsuite.TestWorkflowEnvironment` with `OnActivity` mocks; it simulates time and
retries in memory.

Next: [01-saga-compensation-workflow.md](01-saga-compensation-workflow.md)
