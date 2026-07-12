# Exercise 1: An Idempotent Reconciler that Owns a Child Resource

This exercise builds the core of an operator: a `Reconcile` method for a custom
resource that materializes a managed child object, stamps a controller owner
reference on it for garbage collection and fan-in, and records observed state on
the `/status` subresource. The deliverable that proves you wrote a convergence
engine and not fire-once create logic is this: the same reconcile run twice against
unchanged desired state returns `OperationResultNone` — a genuine no-op.

This module is self-contained. It defines its own API type with hand-written
deepcopy methods (the kind `controller-gen` normally generates), its reconciler,
a runnable demo backed by the in-memory fake client, and its tests. Nothing here
imports another exercise.

## What you'll build

```text
cache-reconciler/                 independent module: example.com/cache-reconciler
  go.mod                          go 1.24; requires controller-runtime + k8s.io/api
  cache_types.go                  Cache/CacheSpec/CacheStatus/CacheList + deepcopy + scheme
  reconciler.go                   CacheReconciler.Reconcile, syncConfigMap, SetupWithManager
  cmd/
    demo/
      main.go                     seeds a Cache in a fake client, reconciles, prints child + status
  reconciler_test.go              fake-client tests: child + owner ref + status; idempotency (None)
```

- Files: `cache_types.go`, `reconciler.go`, `cmd/demo/main.go`, `reconciler_test.go`.
- Implement: a `CacheReconciler` whose `Reconcile` uses `controllerutil.CreateOrUpdate` to materialize a child `ConfigMap`, sets a controller owner reference, and writes `.status` via `Status().Update`.
- Test: build a fake client with the status subresource registered; assert the child exists with a controller owner reference and status is populated; call `syncConfigMap` twice and assert `OperationResultCreated` then `OperationResultNone`.
- Verify: `go test -count=1 -race ./...` (needs the controller-runtime modules; offline this is validated by gofmt + review).

Set up the module and pull the dependencies:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/03-controller-runtime-reconcile/01-idempotent-reconcile-child-resource/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/03-controller-runtime-reconcile/01-idempotent-reconcile-child-resource
go mod edit -go=1.24
go get sigs.k8s.io/controller-runtime@v0.20.4
go get k8s.io/api@v0.32.0 k8s.io/apimachinery@v0.32.0 k8s.io/client-go@v0.32.0
```

### The API type and why it needs deepcopy

A custom resource is a Go struct that satisfies `runtime.Object`: it embeds
`metav1.TypeMeta` and `metav1.ObjectMeta`, and it must provide a
`DeepCopyObject() runtime.Object` method. The scheme and every client copy objects
defensively before handing them out, so deepcopy is not optional plumbing — a
missing or shallow deepcopy causes two callers to alias the same map and corrupt
each other. In a real project `controller-gen` writes these into
`zz_generated.deepcopy.go`; here they are written by hand so the module is
complete and you can see exactly what the machinery requires. Because `CacheSpec`
and `CacheStatus` hold only scalar fields, a struct assignment (`*out = *in`) is a
correct deep copy for them; the moment a spec grows a slice or map, deepcopy must
copy it element by element.

`GroupVersion`, the `scheme.Builder`, and `AddToScheme` register the type under a
group/version so the client knows how to (de)serialize it. `init` calls
`SchemeBuilder.Register(&Cache{}, &CacheList{})`; a test or `main` then calls
`AddToScheme(scheme)` to install it.

Create `cache_types.go`:

```go
package cachereconciler

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version this API type is registered under.
var GroupVersion = schema.GroupVersion{Group: "platform.example.com", Version: "v1alpha1"}

// SchemeBuilder wires the API types into a runtime.Scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme registers the Cache types with a scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(&Cache{}, &CacheList{})
}

// CacheSpec is user intent: how the managed cache should be configured.
type CacheSpec struct {
	// MaxEntries is the desired capacity rendered into the child ConfigMap.
	MaxEntries int `json:"maxEntries"`
	// EvictionPolicy is the desired policy rendered into the child ConfigMap.
	EvictionPolicy string `json:"evictionPolicy"`
}

// CacheStatus is controller-observed truth, written on the /status subresource.
type CacheStatus struct {
	// Ready is true once the child ConfigMap has been materialized.
	Ready bool `json:"ready"`
	// ConfigMapName is the name of the child ConfigMap this controller owns.
	ConfigMapName string `json:"configMapName,omitempty"`
	// ObservedGeneration is the .metadata.generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Cache is the custom resource: desired cache config plus observed status.
type Cache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CacheSpec   `json:"spec,omitempty"`
	Status CacheStatus `json:"status,omitempty"`
}

// CacheList is the list type required for List operations.
type CacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cache `json:"items"`
}

// DeepCopyInto copies the receiver into out. Spec and Status are scalar-only,
// so a struct assignment deep-copies them.
func (in *Cache) DeepCopyInto(out *Cache) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

// DeepCopy returns a deep copy of the Cache.
func (in *Cache) DeepCopy() *Cache {
	if in == nil {
		return nil
	}
	out := new(Cache)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *Cache) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the list, deep-copying each item.
func (in *CacheList) DeepCopyInto(out *CacheList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Cache, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy returns a deep copy of the list.
func (in *CacheList) DeepCopy() *CacheList {
	if in == nil {
		return nil
	}
	out := new(CacheList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *CacheList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
```

### The reconciler: converge, then record

`Reconcile` re-reads the `Cache` every time — the request carries only a name. A
`NotFound` means the object is gone (already deleted); the correct response is
`client.IgnoreNotFound`, which turns that into `(ctrl.Result{}, nil)` so the
workqueue does not retry a phantom.

The materialization is factored into `syncConfigMap`, which is the idempotent
heart of the loop. `controllerutil.CreateOrUpdate` gets the child, runs the mutate
function to stamp the desired data and the controller owner reference, and then
creates or updates only if something changed. It returns the `OperationResult`.
Keeping this in its own method makes the idempotency property directly testable:
the same-package test calls `syncConfigMap` twice and asserts `Created` then
`None`. The mutate function must be deterministic — the same spec must render a
byte-identical `Data` map and the same owner reference — or the operation could
never settle on `None`.

Status is written last, and through `Status().Update`, not a plain `Update`. That
keeps the observed-state write off the spec generation and off the feedback loop
that a plain update would trigger.

Create `reconciler.go`:

```go
package cachereconciler

import (
	"context"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// CacheReconciler materializes a child ConfigMap for each Cache and records
// observed state on the Cache's status subresource.
type CacheReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Assert at compile time that the reconciler satisfies the interface.
var _ reconcile.Reconciler = &CacheReconciler{}

// Reconcile drives one Cache toward its desired state. It is level-triggered:
// it re-reads the object every call and converges, never assuming an event.
func (r *CacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var cache Cache
	if err := r.Get(ctx, req.NamespacedName, &cache); err != nil {
		// NotFound means the Cache is gone; nothing to converge.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	op, err := r.syncConfigMap(ctx, &cache)
	if err != nil {
		return ctrl.Result{}, err
	}
	l.Info("reconciled child configmap", "operation", op, "cache", cache.Name)

	cache.Status.Ready = true
	cache.Status.ConfigMapName = configMapName(&cache)
	cache.Status.ObservedGeneration = cache.Generation
	if err := r.Status().Update(ctx, &cache); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// configMapName is the deterministic name of the child owned by a Cache.
func configMapName(cache *Cache) string {
	return cache.Name + "-config"
}

// syncConfigMap upserts the child ConfigMap. It is idempotent: on unchanged
// desired state it returns controllerutil.OperationResultNone and writes nothing.
func (r *CacheReconciler) syncConfigMap(ctx context.Context, cache *Cache) (controllerutil.OperationResult, error) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(cache),
			Namespace: cache.Namespace,
		},
	}
	return controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{
			"maxEntries":     strconv.Itoa(cache.Spec.MaxEntries),
			"evictionPolicy": cache.Spec.EvictionPolicy,
		}
		// Owner reference: cascading GC plus Owns() fan-in to this Cache.
		return ctrl.SetControllerReference(cache, cm, r.Scheme)
	})
}

// SetupWithManager wires the primary watch on Cache and the owned-child watch on
// ConfigMap, so a change to a child re-enqueues its owning Cache.
func (r *CacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&Cache{}).
		Owns(&corev1.ConfigMap{}).
		Named("cache").
		Complete(r)
}
```

### The runnable demo

A demo cannot reach a real cluster, so it wires the reconciler to
controller-runtime's in-memory fake client — the same client the tests use. It
seeds a `Cache`, runs one reconcile, then reads back the child `ConfigMap` (with
its controller owner reference) and the populated status. This is exactly what a
real reconcile produces against a real API server.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	cachereconciler "example.com/cache-reconciler"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func main() {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := cachereconciler.AddToScheme(scheme); err != nil {
		panic(err)
	}

	cache := &cachereconciler.Cache{
		ObjectMeta: metav1.ObjectMeta{Name: "sessions", Namespace: "default"},
		Spec:       cachereconciler.CacheSpec{MaxEntries: 1000, EvictionPolicy: "lru"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cache).
		WithStatusSubresource(&cachereconciler.Cache{}).
		Build()

	r := &cachereconciler.CacheReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "sessions", Namespace: "default"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		panic(err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: "sessions-config", Namespace: "default"}, &cm); err != nil {
		panic(err)
	}
	fmt.Printf("child configmap: %s\n", cm.Name)
	fmt.Printf("controller owner: %s\n", cm.OwnerReferences[0].Name)
	fmt.Printf("data[maxEntries]: %s\n", cm.Data["maxEntries"])

	var got cachereconciler.Cache
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		panic(err)
	}
	fmt.Printf("status: ready=%v configMap=%s\n", got.Status.Ready, got.Status.ConfigMapName)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
child configmap: sessions-config
controller owner: sessions
data[maxEntries]: 1000
status: ready=true configMap=sessions-config
```

### Tests

The tests use the fake client with the status subresource registered explicitly —
without `WithStatusSubresource`, `Status().Update` is silently dropped and the
status assertions would fail for the wrong reason. `TestReconcileMaterializesChild`
runs one reconcile and asserts the child exists, carries a controller owner
reference pointing at the `Cache`, and that status is populated.
`TestSyncConfigMapIsIdempotent` is the load-bearing test: it calls `syncConfigMap`
twice and asserts `OperationResultCreated` then `OperationResultNone`, proving the
loop converges instead of writing on every pass. Assertions are on object
*content*, never on `ResourceVersion` or `Generation`, which the fake client does
not model faithfully.

Create `reconciler_test.go`:

```go
package cachereconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return scheme
}

func newCache(name string) *Cache {
	return &Cache{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       CacheSpec{MaxEntries: 100, EvictionPolicy: "lru"},
	}
}

func TestReconcileMaterializesChild(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	cache := newCache("c1")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cache).
		WithStatusSubresource(&Cache{}).
		Build()
	r := &CacheReconciler{Client: c, Scheme: scheme}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "c1", Namespace: "default"}}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res != (reconcile.Result{}) {
		t.Errorf("Result = %+v, want empty", res)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Name: "c1-config", Namespace: "default"}, &cm); err != nil {
		t.Fatalf("child ConfigMap not found: %v", err)
	}
	if got := cm.Data["maxEntries"]; got != "100" {
		t.Errorf("data[maxEntries] = %q, want %q", got, "100")
	}
	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("owner references = %d, want 1", len(cm.OwnerReferences))
	}
	owner := cm.OwnerReferences[0]
	if owner.Name != "c1" || owner.Controller == nil || !*owner.Controller {
		t.Errorf("owner = %+v, want controller ref to c1", owner)
	}

	var got Cache
	if err := c.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get Cache: %v", err)
	}
	if !got.Status.Ready || got.Status.ConfigMapName != "c1-config" {
		t.Errorf("status = %+v, want ready=true configMap=c1-config", got.Status)
	}
}

func TestSyncConfigMapIsIdempotent(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	cache := newCache("c2")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cache).
		Build()
	r := &CacheReconciler{Client: c, Scheme: scheme}

	first, err := r.syncConfigMap(context.Background(), cache)
	if err != nil {
		t.Fatalf("first syncConfigMap: %v", err)
	}
	if first != controllerutil.OperationResultCreated {
		t.Errorf("first op = %q, want %q", first, controllerutil.OperationResultCreated)
	}

	second, err := r.syncConfigMap(context.Background(), cache)
	if err != nil {
		t.Fatalf("second syncConfigMap: %v", err)
	}
	if second != controllerutil.OperationResultNone {
		t.Errorf("second op = %q, want %q (loop did not converge)", second, controllerutil.OperationResultNone)
	}
}

func TestReconcileMissingObjectIsNoOp(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &CacheReconciler{Client: c, Scheme: scheme}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "gone", Namespace: "default"}}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile on missing object should not error, got %v", err)
	}
	if res != (reconcile.Result{}) {
		t.Errorf("Result = %+v, want empty", res)
	}
}

func TestSyncConfigMapUpdatesOnSpecChange(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	cache := newCache("c3")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cache).
		Build()
	r := &CacheReconciler{Client: c, Scheme: scheme}

	first, err := r.syncConfigMap(context.Background(), cache)
	if err != nil {
		t.Fatalf("first syncConfigMap: %v", err)
	}
	if first != controllerutil.OperationResultCreated {
		t.Errorf("first op = %q, want %q", first, controllerutil.OperationResultCreated)
	}

	// Change desired state: the mutate function must now diff and update.
	cache.Spec.MaxEntries = 200
	second, err := r.syncConfigMap(context.Background(), cache)
	if err != nil {
		t.Fatalf("second syncConfigMap: %v", err)
	}
	if second != controllerutil.OperationResultUpdated {
		t.Errorf("second op = %q, want %q (mutate did not diff desired vs observed)", second, controllerutil.OperationResultUpdated)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Name: "c3-config", Namespace: "default"}, &cm); err != nil {
		t.Fatalf("child ConfigMap not found: %v", err)
	}
	if got := cm.Data["maxEntries"]; got != "200" {
		t.Errorf("data[maxEntries] = %q, want %q", got, "200")
	}
}
```

## Review

The reconciler is correct when it is a pure convergence step: re-read the `Cache`,
render the child deterministically, upsert it with `CreateOrUpdate`, and record
status through the subresource. The proof of convergence is
`TestSyncConfigMapIsIdempotent` — the second call returns
`OperationResultNone`. If it instead returns `OperationResultUpdated`, the mutate
function is non-deterministic (a map iteration order leaking into a value, a
timestamp, a re-computed field) and the loop will thrash in production.

The mistakes to avoid: do not return the raw error on a `NotFound` get — wrap it
with `client.IgnoreNotFound` so a deleted object is a clean no-op, which
`TestReconcileMissingObjectIsNoOp` checks. Do not write status with `r.Update`; use
`r.Status().Update` so the write stays off spec generation and off the reconcile
feedback loop. Do not forget `SetControllerReference` inside the mutate — without
it the child is orphaned on delete and `Owns()` can never fan a child change back
to the owner. And in tests, always register `WithStatusSubresource` and assert on
content, not on `ResourceVersion`/`Generation`, which the fake client leaves
unmodeled. Offline this module cannot build (the controller-runtime and k8s.io
modules are not vendored); it is validated by gofmt and review, and gates fully
where those modules are available.

## Resources

- [controller-runtime pkg/controller/controllerutil](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/controller/controllerutil) — `CreateOrUpdate`, `OperationResult`, `SetControllerReference`.
- [controller-runtime pkg/client/fake](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client/fake) — the fake client builder and `WithStatusSubresource`.
- [controller-runtime pkg/reconcile](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile) — `Reconciler`, `Request`, `Result`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-requeue-backoff-and-terminal-errors.md](02-requeue-backoff-and-terminal-errors.md)
