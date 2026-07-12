# 7. Kubernetes Controller

A Kubernetes controller is a control loop that watches the cluster state and makes changes to drive actual state toward desired state. Writing one with `sigs.k8s.io/controller-runtime` requires understanding how the reconcile loop is scheduled, how to make reconciliation idempotent, how owner references drive garbage collection, and how to test the logic with a fake client rather than a real cluster.

The hard parts are not the happy path. They are: returning the right `Result` so the controller does not busy-loop, preventing infinite reconcile storms from status updates you just wrote, and writing tests that exercise real invariants rather than happy-path echo chambers.

```text
configsync/
  go.mod
  reconciler.go
  reconciler_test.go
  cmd/demo/main.go
```

The package is `package configsync`. Tests live in the same package so they can reach unexported helpers.

## Concepts

### The Reconcile Loop

Every controller-runtime controller runs one goroutine per reconcile. The entry point is `Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error)`.

`reconcile.Request` carries only the `types.NamespacedName` of the object that changed. The controller does not receive the old or new object, only the name; it is the reconciler's job to `Get` the current state from the API server. This is intentional: it prevents the controller from accumulating stale state in memory.

The return value controls requeueing:

| Return value | Meaning |
|---|---|
| `Result{}, nil` | Success; do not requeue unless a watch fires |
| `Result{Requeue: true}, nil` | Requeue immediately with backoff |
| `Result{RequeueAfter: d}, nil` | Requeue after duration `d` |
| `Result{}, err` (non-nil error) | Requeue with exponential backoff |

### Idempotency

The reconcile function is called whenever any watch fires. It may be called dozens of times for a single change: once for the object update, once for each owned resource update, once for a resync. Every call must produce the same cluster state. The way to achieve this is to always read current state from the API server, compute the desired state, and reconcile the diff, instead of assuming the previous call's result is still in place.

`controllerutil.CreateOrUpdate` encapsulates the pattern: it fetches the existing object, calls your mutate function, and either creates or updates. The mutate function receives the live object and must write only the fields it owns. This keeps the function idempotent across calls.

### Owner References and Garbage Collection

When a ConfigMap owns a Secret, the Kubernetes garbage collector deletes the Secret when the ConfigMap is deleted. `controllerutil.SetControllerReference(owner, controlled, scheme)` writes the `ownerReferences` field on the controlled object. Only one owner reference may have `controller: true`; SetControllerReference returns an error if another controller reference already exists.

Owner references work only within the same namespace. Cross-namespace ownership is not supported by the Kubernetes garbage collector.

### Predicate Filters

Without filters, every ConfigMap in the cluster triggers reconciliation. A predicate is a set of four boolean functions — `Create`, `Update`, `Delete`, `Generic` — that return true if the event should be processed.

`predicate.NewPredicateFuncs(func(obj client.Object) bool { ... })` builds a predicate from a single function applied to all event types. For update events the function receives the new object. Use it for simple label or annotation filters.

For update events that should only fire when the spec changes (not when the controller itself writes status), `predicate.GenerationChangedPredicate{}` compares `metadata.generation`, which the API server increments only on spec changes, not on status or metadata-only writes.

### Status and Annotations

The standard way to expose controller status is the `.status` sub-resource with a typed status struct. For lightweight metadata, annotations are acceptable. Writing an annotation back to the same object triggers an update event; combine `predicate.GenerationChangedPredicate{}` with `predicate.AnnotationChangedPredicate{}` using `predicate.Or(...)` if you need both to cause reconciliation but do not want the status write to loop.

### Testing With Fake Clients

`fake.NewClientBuilder().WithScheme(scheme).WithObjects(...).Build()` returns a `client.WithWatch` backed by an in-memory store. It supports Get, List, Create, Update, Delete, Patch, and sub-resource operations. The fake client does not run admission webhooks, does not increment ResourceVersion predictably, and does not enforce RBAC, but it is sufficient for unit-testing reconciler logic.

For integration tests that need a real API server, `sigs.k8s.io/controller-runtime/pkg/envtest` stands up a local etcd and kube-apiserver binary. That is out of scope for offline testing.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/31-cloud-native-go/07-kubernetes-controller/07-kubernetes-controller/cmd/demo
cd go-solutions/31-cloud-native-go/07-kubernetes-controller/07-kubernetes-controller
go get sigs.k8s.io/controller-runtime@v0.19.0
```

This is a library, not a program. Verification is `go test`.

### Exercise 1: The Reconciler Struct and Sentinel Errors

Create `reconciler.go`:

```go
package configsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ManagedByLabel is the label value that marks a ConfigMap for sync.
const ManagedByLabel = "configsync"

// LabelKey is the full label key used to select managed ConfigMaps.
const LabelKey = "app.kubernetes.io/managed-by"

// LastSyncAnnotation is the annotation written by the controller on each
// successful reconcile.
const LastSyncAnnotation = "configsync.example.com/last-sync"

var (
	// ErrMissingScheme is returned when the reconciler is constructed without a scheme.
	ErrMissingScheme = errors.New("configsync: scheme is required")

	// ErrOwnerRefFailed is returned when SetControllerReference fails.
	ErrOwnerRefFailed = errors.New("configsync: failed to set controller reference")
)

// ConfigSyncReconciler watches ConfigMaps labelled with
//
//	app.kubernetes.io/managed-by=configsync
//
// and ensures a corresponding Secret exists in the same namespace with the
// same data (string values base64-encoded as []byte). When the ConfigMap is
// deleted, the Secret is garbage-collected via owner references.
type ConfigSyncReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile implements reconcile.Reconciler.
func (r *ConfigSyncReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("configmap", req.NamespacedName)

	var cm corev1.ConfigMap
	if err := r.Get(ctx, req.NamespacedName, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			// Object deleted before we reconciled; nothing to do.
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("configsync: get configmap: %w", err)
	}

	// Only reconcile ConfigMaps carrying the managed-by label.
	if cm.Labels[LabelKey] != ManagedByLabel {
		return reconcile.Result{}, nil
	}

	if err := r.reconcileSecret(ctx, &cm); err != nil {
		return reconcile.Result{}, err
	}

	if err := r.annotateLastSync(ctx, &cm); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("reconciled", "secret", cm.Name)
	return reconcile.Result{}, nil
}

// reconcileSecret ensures a Secret exists in the same namespace as cm, with
// the same name and data. It sets cm as the owner so deletion cascades.
func (r *ConfigSyncReconciler) reconcileSecret(ctx context.Context, cm *corev1.ConfigMap) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cm.Name,
			Namespace: cm.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		// Copy ConfigMap string data as binary Secret data.
		secret.Data = make(map[string][]byte, len(cm.Data))
		for k, v := range cm.Data {
			secret.Data[k] = []byte(v)
		}
		// Set the owner reference so the Secret is GC'd with the ConfigMap.
		if err := controllerutil.SetControllerReference(cm, secret, r.Scheme); err != nil {
			return fmt.Errorf("%w: %s/%s: %v", ErrOwnerRefFailed, cm.Namespace, cm.Name, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("configsync: create-or-update secret: %w", err)
	}
	return nil
}

// annotateLastSync writes the current UTC time as an annotation on the
// ConfigMap. It uses a strategic update on the object returned by Get, so the
// reconcile loop does not accidentally overwrite concurrent changes.
func (r *ConfigSyncReconciler) annotateLastSync(ctx context.Context, cm *corev1.ConfigMap) error {
	patch := client.MergeFrom(cm.DeepCopy())
	if cm.Annotations == nil {
		cm.Annotations = make(map[string]string)
	}
	cm.Annotations[LastSyncAnnotation] = time.Now().UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, cm, patch); err != nil {
		return fmt.Errorf("configsync: patch annotation: %w", err)
	}
	return nil
}

// SetupWithManager registers the controller for ConfigMaps and the Secrets it
// owns. Filtering to managed ConfigMaps is done by the label check inside
// Reconcile, not by a predicate.
func (r *ConfigSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Scheme == nil {
		return ErrMissingScheme
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("configsync").
		For(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
```

The `Reconcile` method returns `nil` for not-found objects; returning an error would cause unnecessary requeueing for objects that no longer exist. The label check inside `Reconcile` (rather than only in a predicate) makes the function safe to call directly from tests without going through the manager.

### Exercise 2: The Test Suite

Create `reconciler_test.go`. This is the primary verification — there is no main program to eyeball:

```go
package configsync

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// newScheme builds a runtime.Scheme with the core Kubernetes types registered.
func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return s
}

// managedCM returns a ConfigMap that carries the managed-by label.
func managedCM(name, namespace string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{LabelKey: ManagedByLabel},
		},
		Data: data,
	}
}

// reconcileOnce builds a fake client seeded with objs, creates a reconciler,
// and calls Reconcile once for the named ConfigMap.
func reconcileOnce(t *testing.T, name, namespace string, objs ...runtime.Object) (reconcile.Result, error) {
	t.Helper()
	scheme := newScheme()
	clientObjs := make([]runtime.Object, len(objs))
	copy(clientObjs, objs)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(clientObjs...).
		Build()
	r := &ConfigSyncReconciler{Client: fc, Scheme: scheme}
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
}

func TestReconcileCreatesSecret(t *testing.T) {
	t.Parallel()

	scheme := newScheme()
	cm := managedCM("myapp", "default", map[string]string{"db-pass": "hunter2"})
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(cm).
		Build()

	r := &ConfigSyncReconciler{Client: fc, Scheme: scheme}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "myapp", Namespace: "default"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("expected empty Result, got %+v", result)
	}

	var secret corev1.Secret
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "myapp", Namespace: "default"}, &secret); err != nil {
		t.Fatalf("Secret not found after reconcile: %v", err)
	}
	if string(secret.Data["db-pass"]) != "hunter2" {
		t.Fatalf("Secret.Data[db-pass] = %q, want %q", secret.Data["db-pass"], "hunter2")
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	t.Parallel()

	scheme := newScheme()
	cm := managedCM("myapp", "default", map[string]string{"key": "value"})
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(cm).
		Build()

	r := &ConfigSyncReconciler{Client: fc, Scheme: scheme}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "myapp", Namespace: "default"}}

	// Call twice; neither call should return an error.
	for i := range 2 {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile call %d error = %v", i+1, err)
		}
	}

	// Exactly one Secret should exist.
	var list corev1.SecretList
	if err := fc.List(context.Background(), &list); err != nil {
		t.Fatalf("List secrets: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 Secret, got %d", len(list.Items))
	}
}

func TestReconcileUpdatesSecretData(t *testing.T) {
	t.Parallel()

	scheme := newScheme()
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(
			managedCM("myapp", "default", map[string]string{"key": "old"}),
		).
		Build()

	r := &ConfigSyncReconciler{Client: fc, Scheme: scheme}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "myapp", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	// Simulate the ConfigMap being updated by patching the in-memory object.
	var cm corev1.ConfigMap
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "myapp", Namespace: "default"}, &cm); err != nil {
		t.Fatalf("Get configmap: %v", err)
	}
	cm.Data["key"] = "new"
	if err := fc.Update(context.Background(), &cm); err != nil {
		t.Fatalf("Update configmap: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	var secret corev1.Secret
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "myapp", Namespace: "default"}, &secret); err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if string(secret.Data["key"]) != "new" {
		t.Fatalf("Secret.Data[key] = %q, want %q", secret.Data["key"], "new")
	}
}

func TestReconcileIgnoresNonManagedConfigMap(t *testing.T) {
	t.Parallel()

	scheme := newScheme()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"},
		Data:       map[string]string{"k": "v"},
	}
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(cm).
		Build()

	r := &ConfigSyncReconciler{Client: fc, Scheme: scheme}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "other", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var list corev1.SecretList
	if err := fc.List(context.Background(), &list); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("expected 0 secrets for unmanaged ConfigMap, got %d", len(list.Items))
	}
}

func TestReconcileNotFoundIsNotAnError(t *testing.T) {
	t.Parallel()

	_, err := reconcileOnce(t, "gone", "default")
	if err != nil {
		t.Fatalf("Reconcile of missing object returned error: %v", err)
	}
}

func TestSetupWithManagerRejectsNilScheme(t *testing.T) {
	t.Parallel()

	r := &ConfigSyncReconciler{}
	if err := r.SetupWithManager(nil); !errors.Is(err, ErrMissingScheme) {
		t.Fatalf("err = %v, want ErrMissingScheme", err)
	}
}
```

Your turn: add `TestReconcileSecretHasOwnerReference` — after reconciling a managed ConfigMap, fetch the Secret and assert that `secret.OwnerReferences` has exactly one entry whose `Name` equals the ConfigMap name and `Controller` is `true`.

### Exercise 3: The Demo Program

Create `cmd/demo/main.go`. Because `cmd/demo` is `package main`, it can only touch exported API:

```go
package main

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"example.com/configsync"
)

func main() {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		os.Exit(1)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-config",
			Namespace: "default",
			Labels: map[string]string{
				configsync.LabelKey: configsync.ManagedByLabel,
			},
		},
		Data: map[string]string{
			"api-key":  "s3cr3t",
			"endpoint": "https://api.example.com",
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(cm).
		Build()

	r := &configsync.ConfigSyncReconciler{
		Client: fc,
		Scheme: scheme,
	}

	ctx := context.Background()
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "demo-config", Namespace: "default"},
	}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("reconcile result: requeue=%v requeue_after=%s\n", result.Requeue, result.RequeueAfter)

	var secret corev1.Secret
	if err := fc.Get(ctx, types.NamespacedName{Name: "demo-config", Namespace: "default"}, &secret); err != nil {
		fmt.Fprintf(os.Stderr, "get secret: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("secret/%s created in namespace %s\n", secret.Name, secret.Namespace)
	for k, v := range secret.Data {
		fmt.Printf("  %s = %s\n", k, string(v))
	}
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Returning Error on Not-Found

Wrong: returning the raw `Get` error when the object is missing.

```go
if err := r.Get(ctx, req.NamespacedName, &cm); err != nil {
    return reconcile.Result{}, err // loops forever after deletion
}
```

What happens: the controller requeues with exponential backoff until the object reappears, producing noisy log lines and wasted work. The object was deleted; that is not an error state.

Fix: use `apierrors.IsNotFound(err)` or `client.IgnoreNotFound(err)`:

```go
if err := r.Get(ctx, req.NamespacedName, &cm); err != nil {
    return reconcile.Result{}, client.IgnoreNotFound(err)
}
```

### Mutating the Object Passed to CreateOrUpdate Before Calling It

Wrong: setting fields on `secret` before passing it to `CreateOrUpdate`, then overwriting them again inside the mutate function.

What happens: on the first call (create path), the pre-set fields and the mutate-function fields are merged, which may be fine. On the second call (update path), `CreateOrUpdate` first calls `Get` and writes the live state back into `secret`, then calls the mutate function. Any fields set before `CreateOrUpdate` are silently discarded.

Fix: set all desired fields only inside the mutate function, where they run after the `Get`.

### Writing Status Back Through the Main Client

Wrong: calling `r.Update(ctx, cm)` to persist a status change on a resource that has a status sub-resource.

What happens: on clusters where the status sub-resource is enabled (most CRDs), a plain `Update` does not persist `.status` changes; only `r.Status().Update(ctx, cm)` does. The bug is invisible in fake-client tests because the fake client does not enforce sub-resource separation unless `WithStatusSubresource` is set.

Fix: use `r.Status().Update(ctx, obj)` for status, `r.Patch(ctx, obj, patch)` for metadata/annotations, and `controllerutil.CreateOrUpdate` for managed child resources.

### Forgetting to Set the Scheme on the Reconciler

Wrong: constructing `ConfigSyncReconciler{Client: fc}` without `Scheme`.

What happens: `controllerutil.SetControllerReference` requires the scheme to resolve the GVK of the owner object. It panics or returns an error, causing every reconcile to fail.

Fix: always pass `Scheme: mgr.GetScheme()` (in production) or `Scheme: newScheme()` (in tests).

### Predicate on For Does Not Apply to Owned Resources

Wrong: adding a label predicate to `For(&corev1.ConfigMap{})` and expecting it to also filter events from owned Secrets.

What happens: events from owned Secrets are triggered by the `Owns` watch, which has its own predicate chain separate from `For`. Unlabelled Secret updates still enqueue reconcile requests for their owner ConfigMaps.

Fix: use `WithEventFilter` at the builder level for predicates that must apply to all watches, or pass per-watch predicates via `builder.WithPredicates(...)` on the individual `For` or `Owns` call.

## Verification

From `~/go-exercises/configsync`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Running `go test -v -race ./...` shows each test case. There is no program output to eyeball; the test suite is the verification.

To run the demo:

```bash
go run ./cmd/demo
```

Expected output (key order may vary):

```
reconcile result: requeue=false requeue_after=0s
secret/demo-config created in namespace default
  api-key = s3cr3t
  endpoint = https://api.example.com
```

## Summary

- A controller reconcile loop receives only the object's NamespacedName; it must Get current state from the API server on every call.
- `controllerutil.CreateOrUpdate` encapsulates idempotent create-or-update. All field mutations belong inside the mutate function, not before the call.
- `apierrors.IsNotFound` (or `client.IgnoreNotFound`) prevents requeue storms after object deletion.
- Owner references set with `controllerutil.SetControllerReference` enable automatic garbage collection of child resources when the parent is deleted.
- `predicate.NewPredicateFuncs` filters which events reach the reconciler; `WithEventFilter` applies a predicate to all watches registered with the builder.
- Fake clients from `fake.NewClientBuilder()` are sufficient for unit-testing reconciler logic; `envtest` is required for integration tests that need a real API server.

## What's Next

Next: [Terraform Provider Skeleton](../08-terraform-provider-skeleton/08-terraform-provider-skeleton.md).

## Resources

- [controller-runtime pkg.go.dev reference](https://pkg.go.dev/sigs.k8s.io/controller-runtime)
- [controllerutil package: CreateOrUpdate, SetControllerReference](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/controller/controllerutil)
- [fake client package](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client/fake)
- [Kubernetes controller pattern](https://kubernetes.io/docs/concepts/architecture/controller/)
- [Kubebuilder book: controller implementation](https://book.kubebuilder.io/cronjob-tutorial/controller-implementation.html)
