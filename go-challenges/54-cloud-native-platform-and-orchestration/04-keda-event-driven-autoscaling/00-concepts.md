# Event-Driven Autoscaling with KEDA — Concepts

You own a queue-backed Go consumer: it pulls from SQS, a Redis list, a Kafka
topic, or an internal broker, and does work per message. The default Kubernetes
autoscaler, the HPA on CPU or memory, is the wrong signal for this workload. An
I/O-bound worker can sit near-idle on CPU while a backlog of a hundred thousand
messages waits, because the bottleneck is downstream latency, not the pod's
processor. Scaling on CPU under-provisions exactly when you have the most work.
KEDA (Kubernetes Event-Driven Autoscaling) fixes this by scaling on the real
demand signal — queue depth, consumer lag, a business KPI — and by scaling the
deployment to zero when the signal is empty, which is how you stop paying for
idle workers off-hours.

This lesson is about the two Go-facing surfaces of that story. When no built-in
scaler fits your metric, you ship an **external scaler**: a gRPC service that
implements KEDA's `ExternalScaler` contract, computing the metric in Go from
whatever source you can reach. That is real, whole-task, on-the-job work. The
second surface is the scaled workload itself: because every scale-down kills
pods, an at-least-once consumer must **drain gracefully and be idempotent** or it
loses or double-processes messages. And you must understand the HPA replica math,
or you will either thrash or detonate into a thousand replicas. The pragmatic
low-code alternative — instrument the workload with Prometheus and let KEDA's
built-in `prometheus` scaler scrape it — is worth owning too, and this lesson
covers all three.

## Concepts

### Architecture and the division of labor

KEDA is not a replacement for the HPA; it is a layer that drives one. Two
components matter. The **keda-operator** watches `ScaledObject` custom resources
and, for each one, creates and owns a standard `HorizontalPodAutoscaler`. The
**keda-metrics-apiserver** implements the Kubernetes `external.metrics.k8s.io`
API so that the HPA it created can read your event metric the same way it would
read any external metric. The consequence that trips everyone up: KEDA itself
only handles **activation**, the transition across the 0↔1 boundary. Scaling from
1 to N and back down to 1 is done entirely by the ordinary HPA using the metric
KEDA serves. If you internalize nothing else, internalize this split: activation
is KEDA's job, 1..N is the HPA's job.

### Activation versus scaling

Two thresholds govern behavior, and confusing them is the most common source of
"it won't scale" tickets. The **activation threshold** gates the 0↔1 edge: it
decides whether the workload is scaled up from zero, and on the way down whether
it is allowed to reach zero. The plain **threshold** (the target) is the
per-replica value the HPA drives toward for 1..N scaling. Activation has
priority. With `threshold = 10` and `activationThreshold = 50`, a backlog of 40
scales the deployment to **zero**, even though the HPA math (below) would happily
want replicas — because 40 is under the activation gate, KEDA reports the scaler
inactive and the workload never leaves zero. One more sharp edge: if
`minReplicaCount >= 1`, the scaler is always active and `activationThreshold` is
ignored entirely. You cannot gate activation on a workload that is never allowed
to reach zero.

### The HPA replica math

The HPA computes desired replicas with a single formula:

```text
desiredReplicas = ceil( currentMetricValue / target )
```

This one equation dictates the entire external-scaler contract. `GetMetrics`
must return the **aggregate** metric — the *total* backlog across the whole
queue, not a per-pod slice — and `GetMetricSpec`'s `targetSizeFloat` is the
**per-replica** target. If the queue holds 250 messages and each replica should
handle 50, you return 250 and 50, and the HPA computes ceil(250/50) = 5. Return
an already-divided per-replica value and the HPA divides again, so you get
ceil(50/50) = 1 and the workload never scales out. Symmetrically, setting the
target to the total desired depth instead of the per-replica target gives
ceil(250/250) = 1 for the same reason.

### The external scaler contract: poll and push

An external scaler is a gRPC service implementing four RPCs. `GetMetricSpec` is
called during reconciliation to shape the HPA — it declares the metric name and
the per-replica target. `IsActive` and `GetMetrics` are called every
`pollingInterval`: `IsActive` answers the activation question, `GetMetrics`
returns the aggregate value the HPA divides. That is the **poll** model.
`StreamIsActive` is the **push** model: KEDA opens a long-lived server stream and
the scaler sends an `IsActiveResponse` whenever activation changes, decoupled
from `pollingInterval`. This is what cuts scale-from-zero latency — instead of
waiting up to a full poll interval for KEDA to notice the first message, the
scaler signals the 0→1 transition immediately. The non-negotiable obligation of
`StreamIsActive` is to honor `stream.Context()`: when KEDA cancels the stream (a
reconcile, a reconnect, a shutdown), the context is cancelled and your loop must
return, or you leak a goroutine per reconcile for the life of the process.

The proto has a deprecation you must respect. `MetricSpec.targetSize` and
`MetricValue.metricValue` are `int64` and deprecated; use the `double` fields
`targetSizeFloat` and `metricValueFloat`. Fractional targets (a target of 2.5
messages per replica) truncate to garbage on the int64 fields.

### Timing knobs, and why scaling is never instant

Three layers of timing explain why nothing happens the instant a message
arrives. `pollingInterval` (default 30s) is how often KEDA runs `IsActive` and
`GetMetrics`. `cooldownPeriod` (default 300s) applies **only** to scale-to-zero:
after the last activity, KEDA waits this long before dropping to zero. It does
*not* govern 1..N downscaling — that is the HPA's own stabilization window
(default 300s of look-back on downscale). Assuming `cooldownPeriod` throttles
ordinary downscaling, or setting a tiny `pollingInterval` expecting instant
reaction, produces thrash rather than responsiveness. Aggressive settings fight
the HPA's stabilization instead of cooperating with it.

### At-least-once delivery plus scale-down equals mandatory drain

Every scale-down, and especially every scale-to-zero, sends the pod `SIGTERM`
and then `SIGKILL` after `terminationGracePeriodSeconds` (default 30s). A queue
consumer that ignores `SIGTERM` gets killed mid-message; under at-least-once
delivery that message is redelivered, so an operation that was half-applied runs
again. Correct autoscaling therefore *requires* two properties from the
workload. First, **graceful drain**: on `SIGTERM` stop pulling new messages, let
in-flight handlers finish within the grace deadline, and cleanly abandon
(negatively acknowledge) anything still running when the deadline hits so it
redelivers rather than vanishing. Second, **idempotency**: because abandoned and
redelivered messages will be processed more than once, every handler must be safe
to run twice. Flip a readiness probe to not-ready as drain begins so the load
balancer and any HTTP surface stop routing to a pod that is on its way out.

### Multiple triggers combine with max/OR

A `ScaledObject` may declare several triggers. Desired replicas is the **max**
across all of them, and the object is **active** if **any** trigger is active.
Design each trigger's threshold independently: a Kafka-lag trigger and a
Prometheus-KPI trigger on the same deployment each get their own per-replica
target, and whichever demands more replicas wins.

### ScaledObject versus ScaledJob

Use a `ScaledObject` for a long-lived `Deployment` that tolerates interruption:
the drain-and-redeliver pattern above makes killing a pod safe. Use a
`ScaledJob` when each message is a long-running unit of work that must not be
killed mid-flight — KEDA creates one `Job` per item and lets it run to
completion. If a single message takes twenty minutes and cannot be safely
resumed, a `ScaledObject` on a `Deployment` is the wrong tool; the pod would be
killed at the next scale-down and the work restarted from scratch.

### Failure modes to reason about

When the scaler or metrics endpoint is unreachable, the HPA reports
`ScalingActive=false`; the workload can get stuck — no scale-up, and depending on
configuration no safe scale-down either. Stale metrics cause over- or
under-scaling. A target of 1 against a large burst produces a replica explosion
capped only by `maxReplicaCount`, so omitting `maxReplicaCount` is a standing
invitation to a surprise cloud bill. A noisy signal flapping around the
activation threshold causes the workload to bounce between zero and one. Each of
these is a design decision, not an accident: bound the blast radius with
`maxReplicaCount`, smooth noisy signals before they reach the scaler, and treat
an unreachable scaler as a first-class alert.

### Built-in scaler versus external scaler

A built-in trigger (`prometheus`, `aws-sqs-queue`, `redis`, `kafka`) is
zero-code: you author a `ScaledObject` and KEDA does the rest, but you are
coupled to that pipeline and, for Prometheus, to the correctness of your PromQL
aggregation. An external gRPC scaler is more work — you run and operate a
service — but it lets you scale on any signal you can compute in Go and gives you
push-based activation for low scale-from-zero latency. The pragmatic default is
the built-in scaler; reach for an external one when your demand signal has no
built-in and cannot be reasonably expressed as a Prometheus query.

## Common Mistakes

### Setting the threshold to the total desired depth

Wrong: making `threshold` the total backlog you tolerate, so
desiredReplicas = ceil(total/total) = 1 and the workload never scales out. Fix:
`threshold` is the **per-replica** target — how much backlog one replica should
be responsible for. The HPA multiplies it back up by dividing the aggregate.

### Returning a per-replica value from GetMetrics

Wrong: dividing the backlog by the current replica count inside the scaler
before returning it. The HPA then divides again by the target, producing far too
few replicas. Fix: `GetMetrics` returns the raw aggregate; the HPA owns the
division.

### Forgetting the activation threshold

Wrong: leaving activation unconsidered, so either the workload flaps up on idle
noise, or with scale-to-zero it never wakes on a small but real backlog. Fix: set
`activationThreshold` deliberately — high enough to ignore noise, low enough that
a genuine backlog crosses it.

### Expecting activation to gate a min-1 workload

Wrong: setting `minReplicaCount: 1` and also relying on `activationThreshold` to
suppress work. With a minimum of one replica the scaler is always active and the
activation threshold is ignored. Fix: only `minReplicaCount: 0` makes activation
meaningful.

### Confusing cooldownPeriod with downscale stabilization

Wrong: tuning `cooldownPeriod` to slow ordinary 1..N downscaling. It governs only
scale-to-zero. Fix: 1..N downscale speed is the HPA's stabilization window;
adjust the HPA behavior, not `cooldownPeriod`.

### Not embedding UnimplementedExternalScalerServer

Wrong: implementing `ExternalScalerServer` without embedding
`UnimplementedExternalScalerServer` by value. When KEDA's proto adds an RPC your
server does not implement, registration fails to compile or the method panics.
Fix: embed `pb.UnimplementedExternalScalerServer` by value in your server struct
for forward compatibility.

### Ignoring stream.Context() in StreamIsActive

Wrong: looping forever in `StreamIsActive` without selecting on
`stream.Context().Done()`. Every reconcile or reconnect starts a new stream and
leaks the old goroutine. Fix: return `ctx.Err()` when the stream context is
cancelled.

### No graceful shutdown

Wrong: ignoring `SIGTERM`, so the pod is `SIGKILL`ed after the grace period with
messages in flight — lost or duplicated with no clean abandon. Fix: use
`signal.NotifyContext`, stop pulling on cancellation, drain within a deadline,
and negatively acknowledge whatever remains.

### Treating scaling as instantaneous

Wrong: setting a one-second `pollingInterval` to "make it fast", which fights the
HPA's stabilization window and thrashes. Fix: account for the layered timing and
tune conservatively.

### PromQL that returns an empty or multi-element vector

Wrong: a Prometheus query that returns no series (so KEDA errors or, with
`ignoreNullValues`, silently treats it as a value) or multiple series (ambiguous
scale decision). Fix: aggregate to a single scalar with `sum(...)` and set
`ignoreNullValues` deliberately.

### Using the deprecated int64 fields

Wrong: setting `targetSize`/`metricValue` for a fractional target, silently
truncating. Fix: use `targetSizeFloat`/`metricValueFloat`.

### Omitting maxReplicaCount

Wrong: no `maxReplicaCount`, so a burst against a small target detonates into a
huge, expensive replica count. Fix: always cap `maxReplicaCount` to bound the
blast radius.

Next: [01-external-scaler-grpc.md](01-external-scaler-grpc.md)
