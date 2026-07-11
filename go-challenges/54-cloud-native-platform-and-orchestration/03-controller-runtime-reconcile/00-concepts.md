# Reconcile Loops with controller-runtime — Concepts

A reconcile loop is not an event handler. It is a level-triggered convergence
engine, and the single most important shift a backend engineer makes when moving
from "handle the create/update/delete webhook" to "write an operator" is
internalizing that difference. controller-runtime hands your `Reconcile` method a
`reconcile.Request` that is *deliberately* nothing but a namespaced name — no
object body, no "what changed", no "this was a create". That emptiness is the
design. It forces you to re-read authoritative state from the API server every
time and drive the world toward the desired state, instead of trusting a delta
you think you received. The discipline is identical to writing an at-least-once
message consumer or a Terraform-style `apply`: never assume you saw the previous
edit, never assume ordering, never assume "this call is the create". You are
writing an idempotent function `f(observed) → desired` that will be called an
unbounded number of times, out of order, concurrently across process restarts,
against a possibly stale cache, and after arbitrary crashes. This file is the
conceptual foundation for the four independent exercises that follow.

## Concepts

### Level-triggered, not edge-triggered

Edge-triggered code reacts to transitions: "a Pod was deleted, so do X." Reconcile
is level-triggered: it observes the current level of the world and converges it to
the target, regardless of how the world got there. The framework guarantees your
reconciler is invoked *at least once* after any relevant change, and it
*coalesces* bursts of events into a single queued key. What it does not guarantee
is that you will see every intermediate state, or the order changes happened, or
that a given call corresponds to any particular event. Two rapid edits can produce
one reconcile. A reconcile can fire with no change at all (a periodic resync). A
reconcile can fire for a change you already handled before a crash. Correctness
therefore cannot come from "the event that woke me up"; it comes from reading the
authoritative object and computing the delta yourself.

The payload being just a `types.NamespacedName` is the enforcement mechanism. If
the framework handed you the changed object, you would be tempted to trust it, and
that trust breaks the moment an event is coalesced or replayed. By giving you only
the name, controller-runtime makes the re-read mandatory: your first line is
almost always a `client.Get`, and a `NotFound` there means the object is gone and
the correct response is usually `return ctrl.Result{}, nil` (via
`client.IgnoreNotFound`), not an error.

### Idempotency is the load-bearing property

Because `Reconcile` runs an unbounded number of times, it must produce the same
cluster state whether it runs once or a hundred times. The concrete tool for this
is `controllerutil.CreateOrUpdate(ctx, c, obj, mutateFn)`: it gets the object,
runs your mutate function to stamp the desired shape, and then either creates it
(if absent) or updates it (only if the mutate actually changed something). It
returns an `OperationResult` — `OperationResultCreated`, `OperationResultUpdated`,
or `OperationResultNone`. That last value is the observable signal that your loop
has *converged*: on a steady-state object with nothing to change, a correct
reconciler returns `OperationResultNone` and performs no writes. A reconciler that
keeps returning `OperationResultUpdated` on an unchanged object is thrashing — it
is writing on every pass, generating events, and re-triggering itself. The mutate
function must be deterministic: given the same desired spec it must produce a
byte-identical child, or `CreateOrUpdate` can never settle on `None`.

### The three requeue outcomes, and why conflating them hurts

Every `Reconcile` returns `(ctrl.Result, error)`, and the pair encodes one of
three fundamentally different intentions. Getting the classification wrong is the
most common operator bug in production.

- Return a non-nil `error` for a transient or unknown failure — the API server
  timed out, a dependency is briefly unreachable, an optimistic-concurrency
  `Update` lost a race. controller-runtime feeds the key back through a
  rate-limited workqueue with exponential backoff. You get retry-with-backoff for
  free; you do not manage timers.
- Return `ctrl.Result{RequeueAfter: d}` with a nil error to poll. This is for an
  external resource that is legitimately still becoming ready (a cloud database
  provisioning, a load balancer allocating an IP). You are not failing; you are
  saying "check again in `d`". Pick a sensible interval (seconds to minutes), not
  a hot `RequeueAfter: time.Second` loop that hammers the API server.
- Return `reconcile.TerminalError(err)` for a permanent, user-caused failure — an
  invalid spec, a reference to something that will never exist. The error is
  logged and counted in metrics but *not* requeued through the rate limiter, so it
  does not hot-loop forever burning CPU and apiserver quota on a problem no retry
  can fix. The object will be reconciled again when the user edits it (a new
  event), which is exactly the right trigger.

Conflating these is expensive: returning a plain error for a permanent problem
hot-loops; returning `TerminalError` for a transient one silently stalls until the
next unrelated event; returning `RequeueAfter` for a hard failure masks it as
progress.

### Result.Requeue is deprecated

Older examples set `ctrl.Result{Requeue: true}`. That field is deprecated. It
requeues through the rate limiter for no articulated reason, muddling the
transient-vs-poll distinction. Modern code returns an `error` when it wants
rate-limited retry, or `RequeueAfter` when it wants a known delay. Recognize the
old form so you do not copy it out of a stale blog post.

### Spec versus status, and the /status subresource

Spec is user intent; status is controller-observed truth. Kubernetes tracks a
`metadata.generation` that bumps on every spec change (not on status changes).
When you write observed state, you must write it through the status subresource:
`r.Status().Update(ctx, obj)`, not a plain `r.Update`. Two reasons. First, a plain
`Update` that carries your status can clobber a concurrent spec edit the user just
made, and vice versa; the subresource split lets spec and status be written
independently. Second, writing status via a plain `Update` bumps the object and
can re-trigger your own reconcile in a feedback loop. Pair the status subresource
with `predicate.GenerationChangedPredicate` on the primary watch so that
status-only churn (including your own writes) does not re-enqueue the object.

### Owner references do two jobs

`ctrl.SetControllerReference(owner, child, scheme)` stamps the child with a
controller owner reference. That reference does two distinct things. First,
cascading garbage collection: when the owner is deleted, Kubernetes deletes the
child automatically — you do not hand-delete children in your reconciler. Second,
event fan-in: when you wire the controller with `Owns(&ChildType{})`, a change to
any child enqueues its *owner* for reconcile, using exactly that owner reference to
find the owner. `SetControllerReference` enforces the single-controller invariant:
an object may have many owners but only one *controller* owner, so two controllers
cannot both claim to manage the same child.

### Watches and fan-out mapping

The controller builder has three ways to feed the workqueue. `For(&Primary{})`
establishes the primary watch — changes to the CR you own enqueue that CR. `Owns(
&Child{})` watches children and enqueues their owner via the owner reference.
`Watches(&Secondary{}, handler.EnqueueRequestsFromMapFunc(mapFn))` handles the
general many-to-one case: a shared external resource (a Secret, a shared
ConfigMap) changes, and every CR that depends on it must be re-enqueued. The map
function is the interesting part — it receives the changed secondary object and
returns the `[]reconcile.Request` of affected primaries. It runs on *every*
secondary event, so it must be cheap and side-effect-free: a lookup, never
expensive work or its own API mutations. Because the map function is a pure
`func(context.Context, client.Object) []reconcile.Request`, you can extract it and
unit-test it directly without a cluster, which is the testable core of watch
wiring. (`source.Kind` is the lower-level source primitive the builder's `Watches`
wraps; you rarely construct it by hand when using the builder.)

### Finalizers make deletion a reconcile

By default the API server hard-deletes an object and it vanishes. If your operator
provisioned an *external* side effect — a real cloud bucket, a queue, a DNS
record, a database — a hard delete leaks it, because your reconciler never runs on
a deleted object. A finalizer fixes this. When you `controllerutil.AddFinalizer`
and persist it, a user's delete does not remove the object; instead the API server
sets `metadata.deletionTimestamp` and waits. Your reconciler now sees a live
object with a non-zero `DeletionTimestamp`, which is the signal to run external
cleanup and then `controllerutil.RemoveFinalizer`. Only when the last finalizer is
gone does the API server complete the delete. This is the one correct place to
tear down external resources before the object disappears. The cleanup must be
idempotent, because a crash between "cleanup done" and "finalizer removed" replays
the whole reconcile: the cleanup runs again on an already-deleted external
resource and must treat "already gone" as success, or deletion blocks forever.

### Concurrency and the workqueue

`controller.Options{MaxConcurrentReconciles: n}` runs up to `n` reconciles in
parallel for throughput. The workqueue guarantees the same key is never processed
by two workers at once, so per-object logic stays serialized while different
objects proceed concurrently — you do not need your own per-object locking.
Because reads come from an informer cache and writes go to the API server,
optimistic-concurrency conflicts (`apierrors.IsConflict`) are *expected* under
contention: two reconciles or a reconcile and a user edit raced on the same
object. A conflict is not a hard failure; return the error so the workqueue
requeues and the next pass re-reads fresh state and retries.

### Caches and stale reads

The client you `Get` and `List` through reads from an informer cache, not directly
from etcd. A `Get` immediately after your own `Create`/`Update` can return the
pre-write view, because the cache has not yet observed your write. Do not fight
this with sleeps or read-after-write hacks. Design so a stale read simply causes
another reconcile that converges — which is precisely why `CreateOrUpdate` and
idempotency matter. A stale read that makes you think a child is missing must not
cause a double-create; `CreateOrUpdate` re-checks and settles. Level-triggering
turns "the cache is slightly behind" from a correctness bug into a harmless extra
pass.

## Common Mistakes

### Treating Reconcile as an event handler

Wrong: "this call must be the create, so create the child" — reacting to an
imagined event. It breaks the instant an event is coalesced, missed, or replayed
after a restart. Fix: re-read authoritative state and converge; the request is
only a name, so compute the delta yourself every time.

### Returning an error for an expected NotFound

Wrong: returning the raw error when `Get` reports the object is gone, producing
endless backoff noise for an object that no longer exists. Fix: wrap the get with
`client.IgnoreNotFound(err)` so a deleted object yields `(ctrl.Result{}, nil)`.

### Hot-polling with a one-second requeue

Wrong: `return ctrl.Result{RequeueAfter: time.Second}, nil` (or the deprecated
`Requeue: true`) to poll an external resource, hammering the API server. Fix: use
a realistic poll interval, or better, watch the external resource so readiness
becomes an event instead of a poll.

### Writing status with the wrong call

Wrong: `r.Update(ctx, obj)` to persist observed state — it fails on or fights the
status subresource and can self-trigger reconcile. Fix: `r.Status().Update(ctx,
obj)`, and filter status churn with `predicate.GenerationChangedPredicate`.

### Forgetting SetControllerReference

Wrong: creating a child with no controller owner reference. The child is orphaned
on owner deletion (it leaks), and `Owns()` can never enqueue the owner when the
child changes (no fan-in). Fix: call `ctrl.SetControllerReference(owner, child,
scheme)` inside the mutate function before create/update.

### External cleanup without a finalizer

Wrong: tearing down a cloud resource in a normal reconcile with no finalizer. When
the user deletes the CR, the API server removes it before your cleanup runs and the
external resource leaks forever. Fix: register a finalizer, do cleanup when
`DeletionTimestamp` is set, then remove the finalizer.

### Non-idempotent finalizer cleanup

Wrong: cleanup that assumes it runs exactly once and errors on an
already-deleted external resource. A crash between cleanup and `RemoveFinalizer`
replays it, the second run errors, and deletion is blocked permanently. Fix: make
cleanup treat "already gone" as success so it is safe to run any number of times.

### Miscategorizing the failure class

Wrong: `reconcile.TerminalError` for a transient failure (it never retries), or a
plain error for a permanent user error (it hot-loops). Fix: transient → return the
error; still-becoming-ready → `RequeueAfter`; permanent user error →
`TerminalError`.

### Test mistakes with the fake client

Wrong: building the fake client without `WithStatusSubresource(&MyCR{})` and then
asserting on status that was silently dropped, or asserting on `ResourceVersion`/
`Generation` that the fake client does not model faithfully. Fix: register the
status subresource explicitly and assert on object content, not version bumps.

### Expensive work in the map function

Wrong: doing API calls or heavy computation inside the `Watches` map function; it
runs on every secondary event and will dominate your controller's cost. Fix: keep
it a cheap, side-effect-free lookup that returns the affected requests.

Next: [01-idempotent-reconcile-child-resource.md](01-idempotent-reconcile-child-resource.md)
