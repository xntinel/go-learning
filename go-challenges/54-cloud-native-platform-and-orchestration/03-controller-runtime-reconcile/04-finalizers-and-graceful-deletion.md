# Exercise 4: Finalizers — Deterministic External Cleanup on Deletion

An operator that provisions something *outside* the cluster — a cloud bucket, a
queue, a DNS record — cannot let the API server hard-delete its custom resource,
because a deleted object never reconciles and the external resource leaks forever.
A finalizer turns deletion into a reconcile: the API server marks the object and
waits until your controller tears down the external side effect and removes the
finalizer. This exercise builds that deletion half of the loop and proves the two
properties that matter — cleanup runs, and a failed cleanup blocks deletion instead
of leaking.

This module is self-contained: its own API type with deepcopy, its reconciler, a
demo, and its tests. Nothing here imports another exercise.

## What you'll build

```text
finalizer-reconcile/              independent module: example.com/finalizer-reconcile
  go.mod                          go 1.24; requires controller-runtime + k8s.io/api
  cache_types.go                  Cache/CacheSpec/CacheStatus/CacheList + deepcopy + scheme
  reconciler.go                   ExternalStore interface; Reconcile with add/remove finalizer
  cmd/
    demo/
      main.go                     add finalizer, delete, watch cleanup fire and finalizer clear
  reconciler_test.go              finalizer added; cleanup once + removed; error blocks deletion
```

- Files: `cache_types.go`, `reconciler.go`, `cmd/demo/main.go`, `reconciler_test.go`.
- Implement: a `CacheReconciler` with an injected `ExternalStore` that, on a live object, ensures a finalizer via `controllerutil.AddFinalizer`; and on an object with a non-zero `DeletionTimestamp`, runs idempotent cleanup then `controllerutil.RemoveFinalizer`.
- Test: reconcile a live object and assert the finalizer is present; delete it through the fake client (which honors finalizers by setting `DeletionTimestamp`), reconcile again, assert cleanup fired exactly once and the finalizer is gone; a case where cleanup errors keeps the finalizer.
- Verify: `go test -count=1 -race ./...` (needs the controller-runtime modules; offline this is validated by gofmt + review).

Set up the module:

```bash
go mod edit -go=1.24
go get sigs.k8s.io/controller-runtime@v0.20.4
go get k8s.io/api@v0.32.0 k8s.io/apimachinery@v0.32.0 k8s.io/client-go@v0.32.0
```

### Deletion is a reconcile, not an event

Without a finalizer, a user's `kubectl delete` removes the object from etcd and it
is gone; your `Reconcile` is never invoked for it (the `Get` returns `NotFound`).
That is fine for objects whose entire state lives in the cluster — owner references
and cascading garbage collection clean up children. It is a disaster for objects
that own *external* state, because nothing tears that state down.

A finalizer changes the deletion protocol. When `metadata.finalizers` is non-empty,
`kubectl delete` does not remove the object; instead the API server sets
`metadata.deletionTimestamp` and leaves the object in place. Your reconciler now
observes a *live object that is being deleted*: `cache.DeletionTimestamp.IsZero()`
is false. That is the signal to run external cleanup and then call
`controllerutil.RemoveFinalizer`. Only when the last finalizer is removed does the
API server complete the delete. So the object's lifecycle has two phases handled by
the same `Reconcile`:

- `DeletionTimestamp` is zero (live): ensure the finalizer is present so that a
  future delete is intercepted. `controllerutil.AddFinalizer` returns whether it
  actually added it; only persist with `Update` when it did, to avoid a needless
  write.
- `DeletionTimestamp` is set (being deleted): if the finalizer is still present,
  run cleanup; on success remove the finalizer and `Update`; on failure return the
  error so the object stays (finalizer intact) and cleanup is retried.

### Idempotent cleanup is not optional

The reconcile can crash at any point, including between "external cleanup
succeeded" and "finalizer removed". After the crash the workqueue replays the same
object — still being deleted, finalizer still present — and cleanup runs *again*, on
an external resource that is already gone. If cleanup treats "already deleted" as an
error, deletion is blocked permanently: every retry fails on the missing resource
and the finalizer never comes off. Cleanup must therefore be idempotent: deleting
something that is already deleted is success. The `memStore` in the demo achieves
this trivially (deleting a missing map key is a no-op); a real client must map the
provider's "not found" to success.

`DeletionTimestamp` is a `*metav1.Time`, and `metav1.Time.IsZero` is nil-safe, so
`cache.DeletionTimestamp.IsZero()` is the idiomatic live-versus-deleting check even
before any deletion has occurred.

### The API type

`CacheSpec.BucketName` names the external resource this `Cache` owns. Deletion must
tear that bucket down.

Create `cache_types.go`:

```go
package finalizerreconcile

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

// CacheSpec is user intent. BucketName names an external resource this Cache
// owns; deletion of the Cache must tear that resource down.
type CacheSpec struct {
	BucketName string `json:"bucketName"`
}

// CacheStatus is controller-observed truth.
type CacheStatus struct {
	Ready bool `json:"ready"`
}

// Cache is the custom resource.
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

### The reconciler: two phases, one function

`ExternalStore` is the injected dependency: `Delete(ctx, id)` tears down the
external resource and must be idempotent. `cacheFinalizer` is the finalizer string,
conventionally `<resource>.<group>/finalizer`.

`Reconcile` branches on `DeletionTimestamp.IsZero()`. On the live path it ensures
the finalizer with `AddFinalizer` and persists only if it changed. On the deleting
path it checks `ContainsFinalizer`; if present, it runs cleanup, and only on cleanup
success removes the finalizer and updates. A cleanup error returns without touching
the finalizer, so the object remains in `Terminating` and the workqueue retries —
the external resource is never abandoned.

Create `reconciler.go`:

```go
package finalizerreconcile

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// cacheFinalizer is the finalizer key that intercepts deletion of a Cache.
const cacheFinalizer = "cache.platform.example.com/finalizer"

// ExternalStore is the external side effect a Cache owns. Delete must be
// idempotent: deleting an already-deleted resource is success, because a crash
// between cleanup and finalizer removal replays the cleanup.
type ExternalStore interface {
	Delete(ctx context.Context, id string) error
}

// CacheReconciler provisions and tears down an external resource per Cache.
type CacheReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Store  ExternalStore
}

var _ reconcile.Reconciler = &CacheReconciler{}

// Reconcile handles both lifecycle phases. On a live object it ensures the
// finalizer; on an object being deleted it runs idempotent cleanup and then
// removes the finalizer so the API server can complete the delete.
func (r *CacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var cache Cache
	if err := r.Get(ctx, req.NamespacedName, &cache); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Being deleted: run external cleanup, then drop the finalizer.
	if !cache.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&cache, cacheFinalizer) {
			if err := r.Store.Delete(ctx, cache.Spec.BucketName); err != nil {
				// Cleanup failed: keep the finalizer so deletion is blocked and
				// retried, rather than leaking the external resource.
				return ctrl.Result{}, fmt.Errorf("cleanup bucket %q: %w", cache.Spec.BucketName, err)
			}
			l.Info("external resource cleaned up", "bucket", cache.Spec.BucketName)
			controllerutil.RemoveFinalizer(&cache, cacheFinalizer)
			if err := r.Update(ctx, &cache); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Live: ensure the finalizer is present so a future delete is intercepted.
	if controllerutil.AddFinalizer(&cache, cacheFinalizer) {
		if err := r.Update(ctx, &cache); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the primary watch on Cache.
func (r *CacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&Cache{}).
		Named("cache-finalizer").
		Complete(r)
}
```

### The runnable demo

The demo backs the reconciler with an in-memory `memStore` whose `Delete` is
idempotent by construction. It reconciles a live `Cache` (adding the finalizer),
deletes it through the fake client (which, honoring the finalizer, sets
`DeletionTimestamp` instead of removing it), reconciles again (running cleanup and
removing the finalizer), and shows the object is finally gone and the bucket
deleted.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	finalizerreconcile "example.com/finalizer-reconcile"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type memStore struct{ buckets map[string]bool }

// Delete is idempotent: removing a missing bucket is a no-op success.
func (m *memStore) Delete(ctx context.Context, id string) error {
	delete(m.buckets, id)
	return nil
}

func main() {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := finalizerreconcile.AddToScheme(scheme); err != nil {
		panic(err)
	}

	cache := &finalizerreconcile.Cache{
		ObjectMeta: metav1.ObjectMeta{Name: "sessions", Namespace: "default"},
		Spec:       finalizerreconcile.CacheSpec{BucketName: "sessions-bucket"},
	}
	store := &memStore{buckets: map[string]bool{"sessions-bucket": true}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cache).Build()
	r := &finalizerreconcile.CacheReconciler{Client: c, Scheme: scheme, Store: store}
	ctx := context.Background()
	key := types.NamespacedName{Name: "sessions", Namespace: "default"}
	req := reconcile.Request{NamespacedName: key}

	// Live reconcile: adds the finalizer.
	if _, err := r.Reconcile(ctx, req); err != nil {
		panic(err)
	}
	var live finalizerreconcile.Cache
	if err := c.Get(ctx, key, &live); err != nil {
		panic(err)
	}
	fmt.Printf("finalizers after first reconcile: %d\n", len(live.Finalizers))

	// Delete: the fake client honors the finalizer by setting DeletionTimestamp.
	if err := c.Delete(ctx, &live); err != nil {
		panic(err)
	}

	// Deleting reconcile: runs cleanup, removes the finalizer.
	if _, err := r.Reconcile(ctx, req); err != nil {
		panic(err)
	}

	err := c.Get(ctx, key, &finalizerreconcile.Cache{})
	fmt.Printf("object gone after cleanup: %v\n", apierrors.IsNotFound(err))
	fmt.Printf("bucket still exists: %v\n", store.buckets["sessions-bucket"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
finalizers after first reconcile: 1
object gone after cleanup: true
bucket still exists: false
```

### Tests

`TestAddsFinalizer` reconciles a live object and asserts the finalizer is present.
`TestCleanupOnDeletion` deletes the object through the fake client (which sets
`DeletionTimestamp` because the finalizer is present), reconciles, and asserts the
spy's cleanup counter is exactly one and the object is finally gone.
`TestCleanupErrorBlocksDeletion` uses a spy whose `Delete` returns an error and
asserts the reconcile returns that error, the object still exists with its
finalizer, and cleanup was attempted — the safety property that keeps a failing
teardown from leaking the external resource. Errors are matched with `errors.Is`
against a wrapped sentinel.

Create `reconciler_test.go`:

```go
package finalizerreconcile

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var errStoreDown = errors.New("external store unreachable")

type spyStore struct {
	calls int
	err   error
}

func (s *spyStore) Delete(ctx context.Context, id string) error {
	s.calls++
	return s.err
}

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

func newCache() *Cache {
	return &Cache{
		ObjectMeta: metav1.ObjectMeta{Name: "sessions", Namespace: "default"},
		Spec:       CacheSpec{BucketName: "sessions-bucket"},
	}
}

func key() types.NamespacedName {
	return types.NamespacedName{Name: "sessions", Namespace: "default"}
}

func TestAddsFinalizer(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newCache()).Build()
	r := &CacheReconciler{Client: c, Scheme: scheme, Store: &spyStore{}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key()}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got Cache
	if err := c.Get(context.Background(), key(), &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, cacheFinalizer) {
		t.Errorf("finalizer %q not added; finalizers=%v", cacheFinalizer, got.Finalizers)
	}
}

func TestCleanupOnDeletion(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newCache()).Build()
	spy := &spyStore{}
	r := &CacheReconciler{Client: c, Scheme: scheme, Store: spy}
	ctx := context.Background()

	// Add the finalizer.
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key()}); err != nil {
		t.Fatalf("Reconcile (add): %v", err)
	}

	// Delete: fake client honors the finalizer by setting DeletionTimestamp.
	var live Cache
	if err := c.Get(ctx, key(), &live); err != nil {
		t.Fatalf("Get before delete: %v", err)
	}
	if err := c.Delete(ctx, &live); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Deleting reconcile: cleanup + finalizer removal.
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key()}); err != nil {
		t.Fatalf("Reconcile (delete): %v", err)
	}

	if spy.calls != 1 {
		t.Errorf("cleanup calls = %d, want 1", spy.calls)
	}
	err := c.Get(ctx, key(), &Cache{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("object still present after cleanup; Get err = %v", err)
	}
}

func TestCleanupErrorBlocksDeletion(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newCache()).Build()
	spy := &spyStore{err: errStoreDown}
	r := &CacheReconciler{Client: c, Scheme: scheme, Store: spy}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key()}); err != nil {
		t.Fatalf("Reconcile (add): %v", err)
	}
	var live Cache
	if err := c.Get(ctx, key(), &live); err != nil {
		t.Fatalf("Get before delete: %v", err)
	}
	if err := c.Delete(ctx, &live); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key()})
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("err = %v, want errors.Is(_, errStoreDown)", err)
	}
	if spy.calls != 1 {
		t.Errorf("cleanup calls = %d, want 1", spy.calls)
	}

	// The object must still exist with its finalizer: deletion is blocked, not leaked.
	var stuck Cache
	if err := c.Get(ctx, key(), &stuck); err != nil {
		t.Fatalf("object should still exist while cleanup fails: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&stuck, cacheFinalizer) {
		t.Errorf("finalizer removed despite failed cleanup; external resource would leak")
	}
	if stuck.DeletionTimestamp.IsZero() {
		t.Errorf("expected DeletionTimestamp set on a blocked deletion")
	}
}

func TestAddFinalizerIsIdempotent(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newCache()).Build()
	r := &CacheReconciler{Client: c, Scheme: scheme, Store: &spyStore{}}
	ctx := context.Background()

	// Two reconciles of the same live object: the first adds the finalizer, the
	// second must be a no-op because ContainsFinalizer already reports it present.
	for range 2 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key()}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	var got Cache
	if err := c.Get(ctx, key(), &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Finalizers) != 1 {
		t.Fatalf("finalizers = %v, want exactly one", got.Finalizers)
	}
	if !controllerutil.ContainsFinalizer(&got, cacheFinalizer) {
		t.Errorf("finalizer %q missing after repeated reconcile", cacheFinalizer)
	}
}
```

## Review

The reconciler is correct when deletion is a reconcile: a live object gains a
finalizer, and an object with a set `DeletionTimestamp` triggers cleanup followed by
finalizer removal, so the API server only completes the delete after the external
resource is torn down. `TestCleanupOnDeletion` proves cleanup fires exactly once and
the object then disappears; `TestCleanupErrorBlocksDeletion` proves a failed cleanup
keeps the finalizer so the resource is never leaked.

The mistakes to avoid: do not do external cleanup without a finalizer — the API
server would delete the object before your reconcile ran and the resource would
leak. Do not write cleanup that assumes it runs once; a crash between cleanup and
`RemoveFinalizer` replays it, so deleting an already-gone resource must be success or
deletion blocks forever. Persist `AddFinalizer`/`RemoveFinalizer` with `r.Update`
(they mutate `metadata.finalizers`, not status), and only write when the helper
reports a change. Offline this module cannot build (the controller-runtime and
k8s.io modules are not vendored); it is validated by gofmt and review, and gates
fully where those modules are available.

## Resources

- [controller-runtime pkg/controller/controllerutil](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/controller/controllerutil) — `AddFinalizer`, `RemoveFinalizer`, `ContainsFinalizer`.
- [controller-runtime pkg/client/fake](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client/fake) — the fake client's finalizer-honoring delete behavior.
- [Kubebuilder Book — Using Finalizers](https://book.kubebuilder.io/reference/using-finalizers) — the finalizer deletion protocol in context.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-watches-and-owner-mapping.md](03-watches-and-owner-mapping.md) | Next: [../04-keda-event-driven-autoscaling/00-concepts.md](../04-keda-event-driven-autoscaling/00-concepts.md)
