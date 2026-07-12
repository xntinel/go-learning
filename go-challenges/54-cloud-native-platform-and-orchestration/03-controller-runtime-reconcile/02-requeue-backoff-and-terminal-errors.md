# Exercise 2: Requeue Semantics — Transient Retry, Poll-Wait, and Terminal Errors

The single most consequential decision in a reconciler is what its `(ctrl.Result,
error)` return means. This exercise builds a reconciler that provisions against a
stubbed external dependency and encodes the three distinct outcomes precisely: a
non-nil error for transient failures (the workqueue applies rate-limited
exponential backoff), `ctrl.Result{RequeueAfter: d}` to poll a resource that is
still becoming ready, and `reconcile.TerminalError` for a permanent user error that
must not hot-loop. Because the reconciler takes its external dependency as an
interface, it is a pure function of its inputs and needs no cluster to test.

This module is self-contained: its own API type with deepcopy, its own reconciler,
demo, and tests. Nothing here imports another exercise.

## What you'll build

```text
provisioner-reconcile/            independent module: example.com/provisioner-reconcile
  go.mod                          go 1.24; requires controller-runtime + k8s.io/api
  cache_types.go                  Cache/CacheSpec/CacheStatus/CacheList + deepcopy + scheme
  reconciler.go                   Provisioner interface; Reconcile classifying the 3 outcomes
  cmd/
    demo/
      main.go                     drives all three outcomes against stub provisioners
  reconciler_test.go              table-driven: RequeueAfter, transient error, TerminalError
```

- Files: `cache_types.go`, `reconciler.go`, `cmd/demo/main.go`, `reconciler_test.go`.
- Implement: a `CacheReconciler` with an injected `Provisioner` that returns transient error → return the error; not-ready → `RequeueAfter`; ready → update status; invalid spec → `reconcile.TerminalError` wrapping a `%w` sentinel.
- Test: a table over an injected stub asserting the exact `(ctrl.Result, error)` pair, using `errors.Is` against the sentinel and against `reconcile.TerminalError(nil)` to classify.
- Verify: `go test -count=1 -race ./...` (needs the controller-runtime modules; offline this is validated by gofmt + review).

Set up the module:

```bash
go mod edit -go=1.24
go get sigs.k8s.io/controller-runtime@v0.20.4
go get k8s.io/api@v0.32.0 k8s.io/apimachinery@v0.32.0 k8s.io/client-go@v0.32.0
```

### Why the return value carries the whole control flow

controller-runtime does not expose retry timers to you. Instead it reads the pair
your `Reconcile` returns and drives the workqueue accordingly. A non-nil error is
interpreted as "this failed, retry it later" and the key is re-added with
rate-limited exponential backoff — ideal for a dependency that is briefly
unreachable, because you get progressively longer waits without writing a single
timer. `RequeueAfter` is interpreted as "no failure, but ask me again after this
duration" — ideal for polling an external resource that is genuinely still
provisioning; you choose the cadence. `reconcile.TerminalError` is interpreted as
"this failed and retrying cannot help" — it is logged and metered but not requeued
through the rate limiter, so an invalid spec does not spin the CPU forever. The
object will be reconciled again when its spec is edited, which is the only event
that can actually fix a permanent user error.

The trap is miscategorizing. Return a plain error for an invalid spec and the
controller hot-loops on a problem no retry fixes. Return `TerminalError` for a
transient blip and the object silently stalls until some unrelated event nudges
it. The classification is the design.

### The API type

The `Cache` here provisions an external resource sized by `spec.size`; a
non-positive size is a permanent user error. Status carries a `phase` string. As
in Exercise 1, deepcopy is hand-written and the spec/status are scalar, so struct
assignment is a valid deep copy.

Create `cache_types.go`:

```go
package provisionerreconcile

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var GroupVersion = schema.GroupVersion{Group: "platform.example.com", Version: "v1alpha1"}

var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(&Cache{}, &CacheList{})
}

// CacheSpec is user intent. Size must be positive; zero or negative is a
// permanent, user-caused error.
type CacheSpec struct {
	Size int `json:"size"`
}

// CacheStatus is controller-observed truth.
type CacheStatus struct {
	Ready bool   `json:"ready"`
	Phase string `json:"phase,omitempty"`
}

type Cache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CacheSpec   `json:"spec,omitempty"`
	Status CacheStatus `json:"status,omitempty"`
}

type CacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cache `json:"items"`
}

func (in *Cache) DeepCopyInto(out *Cache) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

func (in *Cache) DeepCopy() *Cache {
	if in == nil {
		return nil
	}
	out := new(Cache)
	in.DeepCopyInto(out)
	return out
}

func (in *Cache) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

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

func (in *CacheList) DeepCopy() *CacheList {
	if in == nil {
		return nil
	}
	out := new(CacheList)
	in.DeepCopyInto(out)
	return out
}

func (in *CacheList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
```

### The reconciler and its three outcomes

`Provisioner` is the external dependency, injected as an interface so the
reconciler is testable without a cluster: `Provision` reports whether the external
resource is ready yet, or a transient error. `ErrInvalidSpec` is a package-level
sentinel; the terminal path wraps it with `%w` so callers (and tests) can match it
with `errors.Is` while also detecting terminality via `reconcile.TerminalError`.

Note the ordering: the permanent-error check comes first, because there is no point
calling the external system for a spec that can never be valid. Then the transient
and poll outcomes, then the success path that records status.

Create `reconciler.go`:

```go
package provisionerreconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ErrInvalidSpec is a permanent, user-caused error. It is wrapped by a
// TerminalError so the workqueue does not retry it.
var ErrInvalidSpec = errors.New("invalid cache spec")

// Provisioner is the external dependency. Provision reports whether the external
// resource for id is ready, or a transient error if the call itself failed.
type Provisioner interface {
	Provision(ctx context.Context, id string) (ready bool, err error)
}

// CacheReconciler provisions an external resource per Cache.
type CacheReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Provisioner  Provisioner
	PollInterval time.Duration
}

var _ reconcile.Reconciler = &CacheReconciler{}

// Reconcile classifies each outcome: TerminalError for a permanent user error,
// a plain error for a transient failure (rate-limited backoff), RequeueAfter for
// a still-provisioning resource, and a status write on success.
func (r *CacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var cache Cache
	if err := r.Get(ctx, req.NamespacedName, &cache); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Permanent user error: do not hot-loop. Retrying cannot fix an invalid spec.
	if cache.Spec.Size <= 0 {
		return ctrl.Result{}, reconcile.TerminalError(
			fmt.Errorf("%w: spec.size must be > 0, got %d", ErrInvalidSpec, cache.Spec.Size),
		)
	}

	ready, err := r.Provisioner.Provision(ctx, cache.Name)
	if err != nil {
		// Transient/unknown: return the error for rate-limited backoff.
		return ctrl.Result{}, fmt.Errorf("provision %s: %w", cache.Name, err)
	}
	if !ready {
		// Still becoming ready: deterministic poll, not a failure.
		l.Info("external resource not ready; requeueing", "after", r.PollInterval)
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}

	cache.Status.Ready = true
	cache.Status.Phase = "Ready"
	if err := r.Status().Update(ctx, &cache); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the primary watch and bounds concurrency. The workqueue
// still guarantees a single key is never reconciled concurrently.
func (r *CacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&Cache{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Named("cache-provisioner").
		Complete(r)
}
```

### The runnable demo

The demo drives all three outcomes with tiny stub provisioners and prints the
classification. It uses the fake client so it runs without a cluster.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	provisionerreconcile "example.com/provisioner-reconcile"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type stub struct {
	ready bool
	err   error
}

func (s stub) Provision(ctx context.Context, id string) (bool, error) { return s.ready, s.err }

func mustScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := provisionerreconcile.AddToScheme(scheme); err != nil {
		panic(err)
	}
	return scheme
}

func newReconciler(cache *provisionerreconcile.Cache, p provisionerreconcile.Provisioner) *provisionerreconcile.CacheReconciler {
	scheme := mustScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cache).
		WithStatusSubresource(&provisionerreconcile.Cache{}).
		Build()
	return &provisionerreconcile.CacheReconciler{Client: c, Scheme: scheme, Provisioner: p, PollInterval: 30 * time.Second}
}

func req(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}}
}

func newCache(name string, size int) *provisionerreconcile.Cache {
	return &provisionerreconcile.Cache{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       provisionerreconcile.CacheSpec{Size: size},
	}
}

func main() {
	ctx := context.Background()

	// Still provisioning.
	r1 := newReconciler(newCache("c1", 3), stub{ready: false})
	res, _ := r1.Reconcile(ctx, req("c1"))
	fmt.Printf("still provisioning: requeue after %s\n", res.RequeueAfter)

	// Ready.
	r2 := newReconciler(newCache("c2", 3), stub{ready: true})
	if _, err := r2.Reconcile(ctx, req("c2")); err != nil {
		panic(err)
	}
	var got provisionerreconcile.Cache
	if err := r2.Get(ctx, req("c2").NamespacedName, &got); err != nil {
		panic(err)
	}
	fmt.Printf("provisioned: phase=%s ready=%v\n", got.Status.Phase, got.Status.Ready)

	// Invalid spec: terminal.
	r3 := newReconciler(newCache("c3", 0), stub{ready: true})
	_, err := r3.Reconcile(ctx, req("c3"))
	fmt.Printf("invalid spec: terminal=%v isInvalidSpec=%v\n",
		errors.Is(err, reconcile.TerminalError(nil)),
		errors.Is(err, provisionerreconcile.ErrInvalidSpec))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
still provisioning: requeue after 30s
provisioned: phase=Ready ready=true
invalid spec: terminal=true isInvalidSpec=true
```

### Tests

The table drives one stub provisioner per case and asserts the exact `(ctrl.Result,
error)` pair. Terminality is detected with `errors.Is(err,
reconcile.TerminalError(nil))`: the terminal wrapper's own `Is` method matches any
terminal error, and because the sentinel is wrapped with `%w`, `errors.Is(err,
ErrInvalidSpec)` still finds it through the chain. The transient case asserts the
error is present, matches the injected cause, and is *not* terminal — the property
that keeps it on the backoff path.

Create `reconciler_test.go`:

```go
package provisionerreconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var errBoom = errors.New("dependency unreachable")

type stubProvisioner struct {
	ready bool
	err   error
}

func (s stubProvisioner) Provision(ctx context.Context, id string) (bool, error) {
	return s.ready, s.err
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

func TestReconcileOutcomes(t *testing.T) {
	t.Parallel()
	const poll = 30 * time.Second

	tests := []struct {
		name             string
		size             int
		prov             stubProvisioner
		wantRequeueAfter time.Duration
		wantErrIs        error
		wantTerminal     bool
	}{
		{
			name:             "still provisioning polls with RequeueAfter",
			size:             3,
			prov:             stubProvisioner{ready: false},
			wantRequeueAfter: poll,
		},
		{
			name:         "transient failure returns retryable error",
			size:         3,
			prov:         stubProvisioner{err: errBoom},
			wantErrIs:    errBoom,
			wantTerminal: false,
		},
		{
			// The error path is checked before the not-ready path, so a failing
			// Provision yields a retryable error and no RequeueAfter poll.
			name:             "error wins over not-ready: retryable, no requeue",
			size:             3,
			prov:             stubProvisioner{ready: false, err: errBoom},
			wantRequeueAfter: 0,
			wantErrIs:        errBoom,
			wantTerminal:     false,
		},
		{
			name:         "invalid spec returns terminal error",
			size:         0,
			prov:         stubProvisioner{ready: true},
			wantErrIs:    ErrInvalidSpec,
			wantTerminal: true,
		},
		{
			name: "ready path succeeds with empty result",
			size: 3,
			prov: stubProvisioner{ready: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scheme := testScheme(t)
			cache := &Cache{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
				Spec:       CacheSpec{Size: tc.size},
			}
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(cache).
				WithStatusSubresource(&Cache{}).
				Build()
			r := &CacheReconciler{Client: c, Scheme: scheme, Provisioner: tc.prov, PollInterval: poll}

			res, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "c", Namespace: "default"},
			})

			if res.RequeueAfter != tc.wantRequeueAfter {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.wantRequeueAfter)
			}
			if tc.wantErrIs == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("err = %v, want errors.Is(_, %v)", err, tc.wantErrIs)
			}
			if got := errors.Is(err, reconcile.TerminalError(nil)); got != tc.wantTerminal {
				t.Errorf("terminal = %v, want %v", got, tc.wantTerminal)
			}
		})
	}
}
```

## Review

The reconciler is correct when each outcome maps to the right control-flow signal:
transient failure returns an error (rate-limited backoff), a still-provisioning
resource returns `RequeueAfter` with a nil error, a permanent user error returns
`reconcile.TerminalError`, and success writes status and returns an empty result.
`TestReconcileOutcomes` pins all four; the terminal-vs-transient distinction is the
one that matters most in production, so it is asserted with `errors.Is` against
both the sentinel and the terminal marker.

The mistakes to avoid: do not return `Result{Requeue: true}` (deprecated) or a
one-second `RequeueAfter` to poll — pick a realistic interval and prefer watching
the dependency where you can. Do not return a plain error for an invalid spec (it
hot-loops) or a `TerminalError` for a blip (it stalls). Do not treat an
`apierrors.IsConflict` from a status update as a hard failure — it is expected
under contention and should be returned so the workqueue re-reads and retries.
Offline this module cannot build (the controller-runtime and k8s.io modules are not
vendored); it is validated by gofmt and review, and gates fully where those modules
are available.

## Resources

- [controller-runtime pkg/reconcile](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile) — `Result`, `TerminalError`, and the requeue contract.
- [controller-runtime pkg/controller](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/controller) — `Options.MaxConcurrentReconciles`.
- [Kubebuilder Book — Watching Resources](https://book.kubebuilder.io/reference/watching-resources) — requeue and event-driven reconcile in context.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-idempotent-reconcile-child-resource.md](01-idempotent-reconcile-child-resource.md) | Next: [03-watches-and-owner-mapping.md](03-watches-and-owner-mapping.md)
