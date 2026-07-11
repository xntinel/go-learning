# Exercise 3: Wiring Watches — Owns, Watches, and Mapping Secondary Resources to Owners

A reconciler is only as good as the events that wake it up. This exercise builds
the `SetupWithManager` wiring that feeds the workqueue three ways — the primary
watch, the owned-child watch, and a fan-out `Watches` on a shared resource — and
isolates the one piece worth unit-testing without a cluster: the pure map function
that turns a changed secondary object into the set of primaries that must
reconcile.

This module is self-contained: its own API type with deepcopy, its reconciler and
wiring, a demo that exercises the map function against a fake client, and its
tests. Nothing here imports another exercise.

## What you'll build

```text
watch-mapping/                    independent module: example.com/watch-mapping
  go.mod                          go 1.24; requires controller-runtime + k8s.io/api
  cache_types.go                  Cache/CacheSpec/CacheStatus/CacheList + deepcopy + scheme
  reconciler.go                   Reconcile, cachesForSecret (the pure map func), SetupWithManager
  cmd/
    demo/
      main.go                     seeds Caches, runs the map func for a shared Secret
  reconciler_test.go              unit tests for cachesForSecret; compile assertions for wiring
```

- Files: `cache_types.go`, `reconciler.go`, `cmd/demo/main.go`, `reconciler_test.go`.
- Implement: a `CacheReconciler` whose `SetupWithManager` uses `For` (with `GenerationChangedPredicate`), `Owns`, and `Watches(&Secret{}, EnqueueRequestsFromMapFunc(r.cachesForSecret), WithPredicates(...))`, plus the standalone `cachesForSecret` map function.
- Test: feed `cachesForSecret` a `Secret` with known name and namespace against seeded `Cache` objects; assert the exact set of `reconcile.Request`s, including the empty result for an unreferenced Secret.
- Verify: `go test -count=1 -race ./...` (needs the controller-runtime modules; offline this is validated by gofmt + review).

Set up the module:

```bash
mkdir -p ~/operators/watch-mapping/cmd/demo
cd ~/operators/watch-mapping
go mod init example.com/watch-mapping
go mod edit -go=1.24
go get sigs.k8s.io/controller-runtime@v0.20.4
go get k8s.io/api@v0.32.0 k8s.io/apimachinery@v0.32.0 k8s.io/client-go@v0.32.0
```

### Three ways to feed the workqueue

The controller builder wires event sources into a single workqueue keyed by
namespaced name. There are three distinct wiring calls, and picking the wrong one
is a common cause of "my reconciler never fires" bugs.

`For(&Cache{})` establishes the *primary* watch: a change to a `Cache` enqueues
that same `Cache`. This is the one mandatory call. Attaching
`predicate.GenerationChangedPredicate{}` here drops update events where
`metadata.generation` did not change — which is exactly the status-only churn your
own `Status().Update` produces, so the predicate stops the controller from
re-triggering itself on its own writes. `GenerationChangedPredicate` only works on
types that maintain `metadata.generation` (a CR with a status subresource does);
it must not be used on `ConfigMap` or `Secret`, which never bump generation.

`Owns(&corev1.ConfigMap{})` watches children and enqueues their *owner*. It uses
the controller owner reference stamped by `SetControllerReference` to find the
owner, so `Owns` and the owner reference are two halves of one mechanism: without
the reference, `Owns` has nothing to map back to.

`Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.cachesForSecret))`
handles the general many-to-one case. A single shared `Secret` (say, a backend
credential) may be referenced by many `Cache` objects; when that `Secret` changes,
every dependent `Cache` must reconcile. The `handler.MapFunc` receives the changed
secondary object and returns the `[]reconcile.Request` of affected primaries. This
is the interesting, testable part.

### Why the map function must stay cheap and pure

`handler.MapFunc` has type `func(context.Context, client.Object) []reconcile.Request`.
It runs on *every* secondary event, before anything is enqueued, so it sits on the
hot path of the whole controller. It must be a cheap, side-effect-free lookup:
never do API mutations in it, never do expensive work. In production you would back
the lookup with a field index (`mgr.GetFieldIndexer().IndexField(...)`) so it is an
indexed read instead of a full `List`; here the map function does a namespaced
`List` and filters in Go, which keeps the module free of index-registration
plumbing while preserving the property that matters for testing — it is a pure
function of `(seeded Caches, changed Secret)`.

Because the signature is exactly `func(context.Context, client.Object) []reconcile.Request`,
you can hold it as a method on the reconciler, pass it to
`handler.EnqueueRequestsFromMapFunc`, and unit-test it directly with a fake client
seeded with `Cache` objects — no manager, no real apiserver. Full watch *delivery*
(that a real edit to a Secret actually flows through the predicate, the mapper, and
into the workqueue) needs `envtest`, which spins up a real apiserver and etcd and
downloads binaries via `setup-envtest`; that belongs behind a build tag and is
unsuitable for the offline gate. The pure map function is where the interesting
logic lives, and it is fully unit-testable.

### The API type

`CacheSpec.BackendSecret` names the shared `Secret` this `Cache` depends on. That
reference is what the map function keys on.

Create `cache_types.go`:

```go
package watchmapping

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

// CacheSpec is user intent. BackendSecret names a shared Secret this Cache
// depends on; a change to that Secret must re-enqueue this Cache.
type CacheSpec struct {
	BackendSecret string `json:"backendSecret"`
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

### The reconciler, the map function, and the wiring

`cachesForSecret` is the map function. It casts the changed object to a `*Secret`
(returning `nil` for anything else, so an unrelated type produces no requests),
lists `Cache` objects in the Secret's namespace, and returns a request for each one
whose `spec.backendSecret` matches the Secret's name. It sorts the result so the
output is deterministic — handy for tests and demos, and harmless because the
workqueue deduplicates keys anyway.

`SetupWithManager` shows all three feeds. Note the Secret watch uses a
`predicate.Funcs` (`secretDataChanged`) rather than `GenerationChangedPredicate`:
Secrets do not maintain `metadata.generation`, so generation-based filtering would
either drop everything or nothing. `secretDataChanged` compares the old and new
`Data` maps and enqueues only on a real data change, dropping metadata-only churn.

Create `reconciler.go`:

```go
package watchmapping

import (
	"context"
	"reflect"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// CacheReconciler reconciles Cache objects that reference a shared Secret.
type CacheReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Compile-time assertions: the reconciler satisfies reconcile.Reconciler, and
// SetupWithManager type-checks against a manager.Manager (ctrl.Manager aliases it).
var (
	_ reconcile.Reconciler        = &CacheReconciler{}
	_ func(manager.Manager) error = (&CacheReconciler{}).SetupWithManager
)

// Reconcile re-reads the Cache and records whether its backend Secret exists.
func (r *CacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var cache Cache
	if err := r.Get(ctx, req.NamespacedName, &cache); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: cache.Spec.BackendSecret, Namespace: cache.Namespace}, &secret)
	switch {
	case err == nil:
		cache.Status.Ready = true
	case client.IgnoreNotFound(err) == nil:
		cache.Status.Ready = false
	default:
		return ctrl.Result{}, err
	}
	l.Info("reconciled cache", "cache", cache.Name, "ready", cache.Status.Ready)

	if err := r.Status().Update(ctx, &cache); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// cachesForSecret is the fan-out map function. Given a changed Secret it returns
// the reconcile.Requests of every Cache that references it. It is pure and cheap:
// no mutations, a single namespaced List and an in-Go filter. It runs on every
// Secret event, so it must stay side-effect-free.
func (r *CacheReconciler) cachesForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	var list CacheList
	if err := r.List(ctx, &list, client.InNamespace(secret.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		c := &list.Items[i]
		if c.Spec.BackendSecret == secret.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: c.Name, Namespace: c.Namespace},
			})
		}
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Name < reqs[j].Name })
	return reqs
}

// secretDataChanged enqueues only when a Secret's Data actually changes, dropping
// metadata-only churn. GenerationChangedPredicate cannot be used on Secrets, which
// do not maintain metadata.generation.
func secretDataChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldS, ok1 := e.ObjectOld.(*corev1.Secret)
			newS, ok2 := e.ObjectNew.(*corev1.Secret)
			if !ok1 || !ok2 {
				return true
			}
			return !reflect.DeepEqual(oldS.Data, newS.Data)
		},
	}
}

// SetupWithManager wires all three event feeds: the primary watch (filtered to
// spec changes via GenerationChangedPredicate), the owned-child watch, and the
// shared-Secret fan-out via the map function.
func (r *CacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&Cache{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.ConfigMap{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.cachesForSecret),
			builder.WithPredicates(secretDataChanged()),
		).
		Named("cache-watch").
		Complete(r)
}
```

### The runnable demo

The demo seeds three `Cache` objects — two referencing the shared Secret
`backend-creds`, one referencing a different Secret — into a fake client, then runs
`cachesForSecret` for a change to `backend-creds` and prints the fan-out. It also
runs the map function for an unreferenced Secret to show the empty result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	watchmapping "example.com/watch-mapping"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func cache(name, secret string) *watchmapping.Cache {
	return &watchmapping.Cache{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       watchmapping.CacheSpec{BackendSecret: secret},
	}
}

func main() {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := watchmapping.AddToScheme(scheme); err != nil {
		panic(err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			cache("orders", "backend-creds"),
			cache("sessions", "backend-creds"),
			cache("audit", "other-creds"),
		).
		WithStatusSubresource(&watchmapping.Cache{}).
		Build()

	r := &watchmapping.CacheReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()

	changed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "backend-creds", Namespace: "default"}}
	for _, req := range r.CachesForSecret(ctx, changed) {
		fmt.Printf("backend-creds change enqueues: %s\n", req.Name)
	}

	unrelated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unused", Namespace: "default"}}
	fmt.Printf("unused change enqueues: %d caches\n", len(r.CachesForSecret(ctx, unrelated)))
}
```

The demo calls an exported wrapper `CachesForSecret` so `package main` can reach
the mapping logic. Append to `reconciler.go`:

```go
// CachesForSecret is the exported wrapper around the map function so external
// packages (the demo) can exercise it. The unexported cachesForSecret is what the
// builder wires, keeping the MapFunc signature exact.
func (r *CacheReconciler) CachesForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.cachesForSecret(ctx, obj)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
backend-creds change enqueues: orders
backend-creds change enqueues: sessions
unused change enqueues: 0 caches
```

### Tests

The tests exercise `cachesForSecret` directly against a fake client seeded with
`Cache` objects. The fan-out case asserts the exact sorted set of requests; the
unrelated-Secret case asserts the empty result; a wrong-type case (feeding a
`ConfigMap`) asserts `nil`, since the map function only maps Secrets. Because the
map function sorts, the assertion can compare slices directly rather than as sets.

Create `reconciler_test.go`:

```go
package watchmapping

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
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

func cache(name, secret string) *Cache {
	return &Cache{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       CacheSpec{BackendSecret: secret},
	}
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&Cache{}).
		Build()
}

func req(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}}
}

func TestCachesForSecret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		changed client.Object
		want    []reconcile.Request
	}{
		{
			name:    "fans out to every referencing cache, sorted",
			changed: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "backend-creds", Namespace: "default"}},
			want:    []reconcile.Request{req("orders"), req("sessions")},
		},
		{
			name:    "unreferenced secret enqueues nothing",
			changed: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unused", Namespace: "default"}},
			want:    nil,
		},
		{
			name:    "non-secret object maps to nothing",
			changed: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "backend-creds", Namespace: "default"}},
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newClient(t,
				cache("orders", "backend-creds"),
				cache("sessions", "backend-creds"),
				cache("audit", "other-creds"),
			)
			r := &CacheReconciler{Client: c, Scheme: testScheme(t)}

			got := r.cachesForSecret(context.Background(), tc.changed)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d requests %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("request[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSecretDataChanged(t *testing.T) {
	t.Parallel()
	p := secretDataChanged()

	oldS := &corev1.Secret{Data: map[string][]byte{"token": []byte("a")}}
	sameData := &corev1.Secret{Data: map[string][]byte{"token": []byte("a")}}
	newData := &corev1.Secret{Data: map[string][]byte{"token": []byte("b")}}

	if p.Update(event.UpdateEvent{ObjectOld: oldS, ObjectNew: sameData}) {
		t.Error("metadata-only update should be dropped")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: oldS, ObjectNew: newData}) {
		t.Error("data change should be enqueued")
	}
}

func TestCachesForSecretIsNamespaced(t *testing.T) {
	t.Parallel()
	// Two Caches in different namespaces reference a Secret of the same name.
	inA := &Cache{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "ns-a"},
		Spec:       CacheSpec{BackendSecret: "shared"},
	}
	inB := &Cache{
		ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "ns-b"},
		Spec:       CacheSpec{BackendSecret: "shared"},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(inA, inB).
		WithStatusSubresource(&Cache{}).
		Build()
	r := &CacheReconciler{Client: c, Scheme: testScheme(t)}

	// A change to the Secret in ns-a must enqueue only the ns-a Cache, because
	// the List inside cachesForSecret is scoped with client.InNamespace.
	changed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "ns-a"}}
	got := r.cachesForSecret(context.Background(), changed)

	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: "orders", Namespace: "ns-a"}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d requests %v, want %d %v", len(got), got, len(want), want)
	}
	if got[0] != want[0] {
		t.Errorf("request = %v, want %v (map function crossed namespaces)", got[0], want[0])
	}
}
```

## Review

The wiring is correct when each feed maps to the right trigger: `For` enqueues the
changed `Cache` (and `GenerationChangedPredicate` keeps your own status writes from
re-triggering it), `Owns` enqueues an owner when its child changes, and `Watches`
plus the map function enqueues every dependent `Cache` when the shared `Secret`
changes. The load-bearing test is `TestCachesForSecret`: it proves the map function
returns exactly the affected primaries and nothing for unrelated or wrong-typed
objects.

The mistakes to avoid: do not put API calls or heavy work inside the map function —
it runs on every secondary event and will dominate the controller's cost; back it
with a field index in production. Do not attach `GenerationChangedPredicate` to a
`Secret` or `ConfigMap` watch — those types never bump `metadata.generation`, so
use a data-comparing `predicate.Funcs` instead, as `secretDataChanged` does. And
remember the map function must be a pure `func(context.Context, client.Object) []reconcile.Request`;
keeping it that shape is what lets you unit-test it without a cluster. Offline this
module cannot build (the controller-runtime and k8s.io modules are not vendored);
it is validated by gofmt and review, and gates fully where those modules are
available. Full watch delivery is an `envtest` concern that belongs behind a build
tag.

## Resources

- [controller-runtime pkg/handler](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/handler) — `EnqueueRequestsFromMapFunc`, `MapFunc`.
- [controller-runtime pkg/builder](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/builder) — `For`, `Owns`, `Watches`, `WithPredicates`.
- [controller-runtime pkg/predicate](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/predicate) — `GenerationChangedPredicate`, `Funcs`.
- [Kubebuilder Book — Watching Resources](https://book.kubebuilder.io/reference/watching-resources) — Owns/Watches/predicates rationale.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-requeue-backoff-and-terminal-errors.md](02-requeue-backoff-and-terminal-errors.md) | Next: [04-finalizers-and-graceful-deletion.md](04-finalizers-and-graceful-deletion.md)
