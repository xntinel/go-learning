# Building a Kubernetes Operator with kubebuilder — Concepts

An operator is how a platform team ships an SRE runbook as running code. You take
the procedure a human used to follow — "provision a Redis cluster, wait for it to
become healthy, scale it, replace failed members, garbage-collect it on delete" —
and encode it as a control loop that the API server drives on your behalf. The
custom resource definition (CRD) is the public API of that runbook: the moment a
user runs `kubectl apply -f cachecluster.yaml`, your `spec` shape is a contract
you can never silently break, exactly like a REST or gRPC schema. This lesson is
deliberately about that API surface and the operator bootstrap, not the reconcile
logic (that is the next lesson). Getting spec/status separation, the status
subresource, generation/observedGeneration, conditions, scheme registration, and
DeepCopy right is what makes every downstream reconciler safe. Two correctness
traps dominate at senior level, and both are addressed head-on below: a wrong
DeepCopy corrupts the shared informer cache for every other consumer in the
process, and a status that is not an independent subresource races spec writes and
destroys the one signal — observedGeneration — that tells the world whether the
system has converged.

## Concepts

### CRD plus controller equals operator

A CRD extends the Kubernetes API server with a new resource kind. Once installed,
the API server treats your `CacheCluster` like a first-class object: it stores it
in etcd, validates it, versions it, and serves it through the same REST endpoints
as a `Pod` or `Deployment`. That is only half of an operator. The other half is
the controller — a long-running process that watches those objects and does the
operational work to make reality match the declared `spec`. The CRD is the noun;
the controller is the verb. This lesson defines the noun (the Go types, the
scheme, the validation markers) and stands up the process that will host the verb
(the manager, its cache, leader election, health probes). The verb itself — the
reconcile loop — is the next lesson.

### Spec versus status: two owners, two lifecycles

The single most important design rule in the whole model is who owns what. `spec`
is desired state; it is owned by the user (or a GitOps controller like Argo CD or
Flux). The controller reads `spec` and must treat it as read-only: a controller
that mutates `spec` is fighting the user and will produce an infinite write loop
against GitOps, which reapplies the manifest and reverts the change on every sync.
`status` is observed state; it is owned by the controller. Users never write
`status`, and the controller never expresses intent there. Keeping these separate
is not bookkeeping — it is what lets a human, a dashboard, and three other
controllers all read the same object and agree on "what was asked for" versus
"what is currently true."

### The status subresource is mandatory, and here is the mechanics

Marking the type with `+kubebuilder:subresource:status` splits the object into two
independent update endpoints: `/status` and the main resource. This is not a
nicety; without it the model breaks in three concrete ways. First, every write to
`status` bumps `metadata.generation`, which is supposed to track only `spec`
changes — so `observedGeneration` becomes meaningless. Second, status writes and
spec writes share the same `resourceVersion` for optimistic concurrency, so a
controller updating `status` collides with a user editing `spec`, and one of them
loses with a conflict error and a retry storm. Third, a client doing a full update
of the object can silently clobber `status`. With the subresource enabled, spec
and status have separate endpoints, separate `resourceVersion` streams for
optimistic concurrency, and separate RBAC verbs, so the controller can be granted
`patch` on `.../status` without being able to rewrite user intent.

### generation versus observedGeneration

When status is a subresource, `metadata.generation` increments only when `spec`
changes. The canonical way for a human or another controller to know whether the
operator has caught up is for the controller to copy `metadata.generation` into
`status.observedGeneration` once it has finished acting on that spec. Then the
rule "`observedGeneration < generation` means the controller is behind" holds
exactly, and "`observedGeneration == generation` and `Ready=True`" means
converged. Never stamping `observedGeneration` leaves no way to distinguish
"converged" from "hasn't looked yet," which is the difference between a green
dashboard you can trust and one you cannot. Crucially, `observedGeneration` must
be stamped only after convergence, not at the start of a reconcile.

### Conditions are the standard status vocabulary

Kubernetes standardized how controllers report multi-dimensional status: a list of
`metav1.Condition`, each with a `Type` (e.g. `Ready`, `Progressing`, `Degraded`),
a `Status` of `True`/`False`/`Unknown`, a machine-readable `Reason`, a
human-readable `Message`, a `LastTransitionTime`, and its own
`ObservedGeneration`. The apimachinery helper `meta.SetStatusCondition` is the
correct way to mutate that list, and its behavior is subtle and important: it
moves `LastTransitionTime` only when the condition's `Status` actually flips, and
it returns whether anything changed. Hand-rolling this with `time.Now()` on every
reconcile makes `LastTransitionTime` advance on every pass even when nothing
changed, which is "flapping" — it defeats alerting that keys off transition age
and makes the object churn in etcd. Use the helper; do not reinvent it.

### Scheme and GVK: types become API objects only after registration

A Go struct is just a struct until it is registered in a `runtime.Scheme`. The
scheme is the bidirectional map between Go types and `GroupVersionKind` (GVK)
identities like `cache.platform.example.com/v1, Kind=CacheCluster`. It powers
(de)serialization, the typed and unstructured clients, and the cache. The
`runtime.Object` interface a scheme requires has two methods: `GetObjectKind`
(satisfied for free by embedding `metav1.TypeMeta`) and `DeepCopyObject`. That
second method is why every API type needs a deep copy. In a real project
`controller-gen` generates `zz_generated.deepcopy.go` from the
`+kubebuilder:object:root=true` marker; in these exercises you write it by hand so
you understand exactly what it must guarantee, and so the package compiles without
running the generator.

### DeepCopy correctness and the shared informer cache

Here is the trap that separates a working operator from one that corrupts other
controllers. The manager holds a single shared informer cache, and reads through
that cache return pointers to shared, read-only objects — the same pointer is
handed to every consumer in the process. If your reconciler mutates a cached
object in place, or if `DeepCopy` is shallow (it copies the struct header but
leaves slices, maps, and pointer fields aliasing the original), then a mutation in
one controller silently changes the object another controller is reading. The
symptoms are maddening: nondeterministic behavior, fields that change without a
write, cross-controller interference. The discipline is absolute: always
`DeepCopy` a cached object before mutating it, and make sure `DeepCopyInto`
allocates fresh backing arrays for every slice, fresh maps for every map, and
fresh pointees for every pointer. A correct deep copy, when you mutate the copy,
leaves the original completely unchanged.

### controller-gen markers are the source of truth

The comment markers above your types are not documentation — they are the input to
code generation. `+kubebuilder:object:root=true` drives deepcopy generation.
`+kubebuilder:validation:*` and `+kubebuilder:default` drive the OpenAPI v3
structural schema baked into the CRD, which is what lets the API server prune
unknown fields, validate on admission, and apply defaults. `+kubebuilder:rbac`
markers drive the generated Role. `+kubebuilder:printcolumn` shapes
`kubectl get` output. Getting markers right means the cluster enforces your
contract for free, on every write, with no controller code running.

### Declarative validation versus admission webhooks

There are two places to validate. Structural-schema validation from markers runs
in the API server, is always on, adds no latency you own, and cannot be bypassed —
prefer it for everything it can express (enums, ranges, required fields, string
patterns, defaults). Admission webhooks handle what the schema cannot:
cross-field invariants, stateful checks against other objects, complex defaulting.
But a webhook is a network hop on the write path, so it adds latency, a new
availability dependency (if the webhook is down, writes fail), and TLS certificate
management. The senior default is: validate declaratively with markers, and reach
for a webhook only when a rule genuinely cannot be expressed as a schema.

### API versioning and stability

The CRD is a public contract, so it follows the same alpha/beta/GA discipline as
any API: `v1alpha1` may change or vanish, `v1beta1` is more stable, `v1` is a
commitment. A kind can serve multiple versions simultaneously, but exactly one is
the storage version (what etcd persists), and a conversion webhook (or trivial
identity conversion) bridges the others. Once a version ships and users have
applied manifests against it, only additive, backward-compatible changes are
allowed on that version; a breaking change requires a new version and a conversion
path. Treating the CRD as "internal" and mutating the spec shape after release
breaks existing manifests and every GitOps repo that references them.

### List semantics and server-side apply

Fields that are slices or maps need list markers — `+listType` and, for lists
keyed by a field, `+listMapKey` — so that server-side apply (SSA) and strategic
merge know whether to merge element-by-element or replace wholesale. Omitting them
makes a list `atomic` by default: any partial apply replaces the entire list,
which produces spurious diffs and lets two appliers stomp each other's entries.
For unbounded, independently-owned lists (endpoints, members, conditions), declare
`+listType=map` with a key so SSA merges correctly.

### The manager, leader election, and health probes

The manager is the runtime that hosts everything: it builds the shared cache and
clients, wires the scheme, runs the metrics and health servers, and starts your
controllers. Leader election runs a single active controller across replicas using
a `Lease`, so two pods never both write and cause split-brain; the
`LeaderElectionID` must be unique per operator, or two different operators will
fight over one lease. The manager also exposes liveness and readiness probes via
`AddHealthzCheck`/`AddReadyzCheck`; Kubernetes uses liveness to restart a wedged
operator and readiness to gate rollout. Forgetting to register them means the
platform cannot tell whether your operator pod is healthy.

## Common Mistakes

### Omitting the status subresource

Wrong: defining the type without `+kubebuilder:subresource:status`. Then status
writes bump `metadata.generation`, race spec writes under one shared
`resourceVersion`, and can be silently overwritten by a full-object update. Fix:
add the marker so spec and status get independent update endpoints, RBAC, and
optimistic-concurrency streams.

### Never recording observedGeneration

Wrong: leaving `status.observedGeneration` unset, so nobody can tell "converged"
from "the controller hasn't caught up." Fix: after acting on a spec, copy
`metadata.generation` into `status.observedGeneration`, and only then, so
`observedGeneration < generation` reliably means "behind."

### Hand-writing LastTransitionTime with time.Now()

Wrong: stamping `LastTransitionTime = metav1.Now()` on every reconcile. The
timestamp then advances even when nothing changed, causing flapping and defeating
transition-age alerts. Fix: use `meta.SetStatusCondition`, which moves
`LastTransitionTime` only on an actual `Status` flip and reports whether anything
changed.

### Mutating spec from the controller

Wrong: writing a computed value back into `spec`, or pushing status through the
spec update path. Against GitOps this loops forever, and it violates ownership.
Fix: the controller reads `spec` read-only and writes only `status`, through the
status subresource endpoint.

### Shallow or wrong DeepCopy

Wrong: a `DeepCopyInto` that does `*out = *in` and stops — slices, maps, and
pointers now alias the original, so mutating the copy corrupts the shared cache
for every other consumer. Fix: allocate fresh backing arrays, fresh maps, and
fresh pointees for every reference-typed field, and deep-copy nested objects
(conditions) element by element.

### Forgetting to register types in the scheme

Wrong: constructing a client or (de)serializing before calling `AddToScheme`,
producing `no kind is registered for the type` panics at runtime. Fix: register
every API type (and the List type) in the scheme via `SchemeBuilder.Register` and
add it to the manager's scheme at startup, alongside the client-go core types.

### Non-pointer optional fields without omitempty

Wrong: an `int32` optional field with no `omitempty` and no pointer, so the zero
value serializes and is indistinguishable from "unset," creating endless spurious
diffs and defaulting confusion. Fix: make genuinely optional scalars pointers with
`omitempty` and `+optional`, so absence is representable.

### Unbounded lists without listType/listMapKey

Wrong: a large or independently-owned slice with no list markers, so it defaults
to atomic and server-side apply replaces the whole list on any partial write. Fix:
declare `+listType=map` with a `+listMapKey` (or `set`/`atomic` deliberately) so
strategic merge and SSA behave.

### Defaults only in Go, not in the schema

Wrong: applying defaults in controller code but not via `+kubebuilder:default`, so
`kubectl apply --dry-run=server` and the actual stored object diverge. Fix: put
defaults in the CRD schema so the API server applies them uniformly on every
write.

### Reusing a LeaderElectionID across operators

Wrong: copy-pasting the bootstrap and leaving the same `LeaderElectionID`, so two
different operators contend for one lease and each spends half its life idle. Fix:
give every operator a unique, stable `LeaderElectionID`.

### Treating the CRD as internal

Wrong: changing the spec shape after release because "it's just our CRD." Any user
manifest or GitOps repo pinned to the old shape breaks. Fix: version the API
(alpha/beta/GA), keep one storage version, and make only backward-compatible
changes to a shipped version.

### Skipping health checks

Wrong: not calling `AddHealthzCheck`/`AddReadyzCheck`, so Kubernetes cannot gate
liveness or readiness and a wedged operator is never restarted. Fix: register at
least a `healthz.Ping` liveness check and a readiness check at bootstrap.

Next: [01-crd-api-types-and-scheme.md](01-crd-api-types-and-scheme.md)
