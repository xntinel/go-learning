# 6. Kubernetes client-go

`client-go` is the official Go client library for Kubernetes. Every controller, operator, and CLI tool written in Go uses it. The hard parts are not the API calls themselves but understanding the three abstraction levels (raw REST, typed clientsets, and informers), why informers exist and what the cache-sync guarantee means, and how to test all of this hermetically with the fake clientset — without a running cluster.

```text
podwatcher/
  go.mod
  watcher.go
  watcher_test.go
  cmd/demo/main.go
```

The package exposes a `Watcher` type that wraps a `kubernetes.Interface`. Tests use `fake.NewSimpleClientset`; no cluster is required.

## Concepts

### Three Abstraction Levels

`client-go` offers three layers, each building on the one below.

**Raw REST (`rest.Config` + `rest.Interface`)** is the lowest level. You deal with HTTP verbs, URL paths, and raw JSON yourself. You almost never use this directly.

**Typed clientsets** (`kubernetes.Clientset`) give you strongly-typed methods for every Kubernetes resource. `cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})` returns a `*corev1.PodList` with full type safety. This is the right level for one-shot operations: listing, getting, creating, deleting.

**Informers** (`informers.SharedInformerFactory`) are a caching layer on top of the clientset. An informer opens a long-lived Watch connection, stores the current state in a thread-safe `Indexer` (the local cache), and fires `AddFunc`/`UpdateFunc`/`DeleteFunc` callbacks when objects change. Reading from the cache instead of calling the API server on every request is the primary reason informers exist: a controller with a thousand pods does not issue a thousand List calls.

### In-Cluster vs Out-of-Cluster Config

Two config sources are standard.

`rest.InClusterConfig()` works inside a running pod. It reads the service account token and CA cert that Kubernetes mounts automatically at well-known paths. It returns `rest.ErrNotInCluster` when the required environment variables (`KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT`) are absent.

`clientcmd.BuildConfigFromFlags("", kubeconfigPath)` works on a developer machine or CI server that has a `~/.kube/config`. It reads the kubeconfig file, resolves the current context, and returns a `*rest.Config`. When both arguments are empty strings and in-cluster variables are present, it falls back to `InClusterConfig` automatically.

The idiomatic pattern tries in-cluster first and falls back to kubeconfig:

```go
func buildConfig(kubeconfigPath string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
}
```

### The Cache-Sync Guarantee

After calling `factory.Start(ctx.Done())`, the informer lists all existing objects and populates the local cache before it begins processing live Watch events. `factory.WaitForCacheSync(ctx.Done())` blocks until that initial list-and-populate step finishes. Reading from the lister before `WaitForCacheSync` returns can yield stale or empty results — a common and subtle bug.

### The AddFunc Signature Change

In `client-go` v0.28+, a new type `ResourceEventHandlerDetailedFuncs` was added. Its `AddFunc` carries a second argument:

```go
type ResourceEventHandlerDetailedFuncs struct {
	AddFunc    func(obj interface{}, isInInitialList bool)
	UpdateFunc func(oldObj, newObj interface{})
	DeleteFunc func(obj interface{})
}
```

`isInInitialList` is `true` when the event came from the initial List (cache warmup), and `false` for subsequent Watch events. The original `ResourceEventHandlerFuncs.AddFunc` remains `func(obj interface{})` (single argument) in all versions. Use `ResourceEventHandlerDetailedFuncs` when you need the `isInInitialList` boolean; use `ResourceEventHandlerFuncs` when you do not. Both implement `ResourceEventHandler` and are accepted by `AddEventHandler`.

### Why the Fake Clientset?

`fake.NewSimpleClientset(objects ...runtime.Object)` returns a `*fake.Clientset` that implements `kubernetes.Interface` using an in-memory object tracker. No network. No cluster. It supports List, Get, Create, Update, Delete, and Watch. Because your types accept `kubernetes.Interface`, switching from real to fake requires no code change in the package under test.

## Exercises

Set up the module. This is a library package, not `package main`, so you verify it with `go test`, not `go run`.

```bash
mkdir -p go-solutions/31-cloud-native-go/06-kubernetes-client-go/06-kubernetes-client-go/cmd/demo
cd go-solutions/31-cloud-native-go/06-kubernetes-client-go/06-kubernetes-client-go
go get k8s.io/client-go@v0.33.0
go get k8s.io/api@v0.33.0
go get k8s.io/apimachinery@v0.33.0
```

### Exercise 1: The Watcher Type

Create `watcher.go`:

```go
package podwatcher

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ErrCacheSyncTimeout is returned when the informer cache does not sync
// within the context deadline.
var ErrCacheSyncTimeout = errors.New("podwatcher: cache sync timed out")

// PodSummary is a read-only snapshot of a pod.
type PodSummary struct {
	Name      string
	Namespace string
	Phase     corev1.PodPhase
	Restarts  int32
}

// EventKind describes how a pod changed.
type EventKind string

const (
	EventAdded   EventKind = "ADDED"
	EventUpdated EventKind = "UPDATED"
	EventDeleted EventKind = "DELETED"
)

// PodEvent is delivered to the handler registered via OnChange.
type PodEvent struct {
	Kind EventKind
	Pod  PodSummary
}

// Handler receives pod events from the informer.
type Handler func(PodEvent)

// Watcher uses a shared informer to watch pods in a given namespace.
// It caches pod state locally so it does not issue repeated List calls
// to the API server.
type Watcher struct {
	cs        kubernetes.Interface
	namespace string
	handler   Handler
}

// New creates a Watcher that reports events for pods in namespace.
// If namespace is empty, it watches all namespaces.
func New(cs kubernetes.Interface, namespace string, h Handler) *Watcher {
	return &Watcher{cs: cs, namespace: namespace, handler: h}
}

// ListPods returns a snapshot of all pods in the watcher's namespace
// using a direct API call (not the cache).
func (w *Watcher) ListPods(ctx context.Context) ([]PodSummary, error) {
	list, err := w.cs.CoreV1().Pods(w.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("podwatcher: list pods: %w", err)
	}
	out := make([]PodSummary, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, summarize(&list.Items[i]))
	}
	return out, nil
}

// Run starts the shared informer and blocks until ctx is cancelled.
// It returns ErrCacheSyncTimeout if the cache does not sync before ctx expires.
func (w *Watcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.cs,
		0, // resync disabled; set to a positive duration to re-trigger UpdateFunc periodically
		informers.WithNamespace(w.namespace),
	)

	podInformer := factory.Core().V1().Pods().Informer()

	if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj interface{}, isInInitialList bool) {
			if pod, ok := obj.(*corev1.Pod); ok && !isInInitialList {
				w.handler(PodEvent{Kind: EventAdded, Pod: summarize(pod)})
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if pod, ok := newObj.(*corev1.Pod); ok {
				w.handler(PodEvent{Kind: EventUpdated, Pod: summarize(pod)})
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				// Tombstone: the informer deleted the object from the cache
				// but the type assertion failed because it's wrapped in a
				// DeletedFinalStateUnknown.
				if d, dok := obj.(cache.DeletedFinalStateUnknown); dok {
					pod, ok = d.Obj.(*corev1.Pod)
				}
			}
			if ok {
				w.handler(PodEvent{Kind: EventDeleted, Pod: summarize(pod)})
			}
		},
	}); err != nil {
		return fmt.Errorf("podwatcher: add event handler: %w", err)
	}

	factory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced) {
		return ErrCacheSyncTimeout
	}

	<-ctx.Done()
	return nil
}

// summarize converts a Pod into a PodSummary.
func summarize(pod *corev1.Pod) PodSummary {
	var restarts int32
	for _, cs := range pod.Status.ContainerStatuses {
		restarts += cs.RestartCount
	}
	return PodSummary{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Phase:     pod.Status.Phase,
		Restarts:  restarts,
	}
}
```

The key design choices: `Watcher` accepts `kubernetes.Interface` (not `*kubernetes.Clientset`) so tests can inject `fake.NewSimpleClientset`; `ResourceEventHandlerDetailedFuncs` is used (v0.28+) so `AddFunc` receives `isInInitialList bool` to distinguish cache-warmup events from live Watch events; tombstone handling in `DeleteFunc` guards against a real production edge case.

### Exercise 2: Test With the Fake Clientset

Create `watcher_test.go`:

```go
package podwatcher

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func makePod(name, ns string, phase corev1.PodPhase, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase: phase,
			ContainerStatuses: []corev1.ContainerStatus{
				{RestartCount: restarts},
			},
		},
	}
}

func TestListPodsReturnsSummaries(t *testing.T) {
	t.Parallel()

	cs := fake.NewSimpleClientset(
		makePod("alpha", "default", corev1.PodRunning, 0),
		makePod("beta", "default", corev1.PodPending, 3),
	)

	w := New(cs, "default", func(PodEvent) {})
	pods, err := w.ListPods(context.Background())
	if err != nil {
		t.Fatalf("ListPods error: %v", err)
	}
	if len(pods) != 2 {
		t.Fatalf("want 2 pods, got %d", len(pods))
	}
}

func TestListPodsEmptyNamespace(t *testing.T) {
	t.Parallel()

	cs := fake.NewSimpleClientset(
		makePod("alpha", "default", corev1.PodRunning, 0),
	)

	w := New(cs, "other", func(PodEvent) {})
	pods, err := w.ListPods(context.Background())
	if err != nil {
		t.Fatalf("ListPods error: %v", err)
	}
	if len(pods) != 0 {
		t.Fatalf("want 0 pods in namespace 'other', got %d", len(pods))
	}
}

func TestListPodsSummarizesRestarts(t *testing.T) {
	t.Parallel()

	cs := fake.NewSimpleClientset(makePod("crasher", "default", corev1.PodRunning, 5))
	w := New(cs, "default", func(PodEvent) {})
	pods, err := w.ListPods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 {
		t.Fatalf("want 1 pod, got %d", len(pods))
	}
	if pods[0].Restarts != 5 {
		t.Fatalf("Restarts = %d, want 5", pods[0].Restarts)
	}
}

func TestListPodsPhaseIsPreserved(t *testing.T) {
	t.Parallel()

	cases := []struct {
		phase corev1.PodPhase
	}{
		{corev1.PodRunning},
		{corev1.PodPending},
		{corev1.PodSucceeded},
		{corev1.PodFailed},
	}

	for _, tc := range cases {
		t.Run(string(tc.phase), func(t *testing.T) {
			t.Parallel()
			cs := fake.NewSimpleClientset(makePod("p", "default", tc.phase, 0))
			w := New(cs, "default", func(PodEvent) {})
			pods, err := w.ListPods(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(pods) != 1 || pods[0].Phase != tc.phase {
				t.Fatalf("Phase = %q, want %q", pods[0].Phase, tc.phase)
			}
		})
	}
}

func TestRunCancelsCleanly(t *testing.T) {
	t.Parallel()

	cs := fake.NewSimpleClientset()
	w := New(cs, "default", func(PodEvent) {})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := w.Run(ctx)
	// Cancellation via ctx expiry is not an error from the watcher's perspective.
	// ErrCacheSyncTimeout would be returned only if the sync didn't finish.
	if err != nil && !errors.Is(err, ErrCacheSyncTimeout) {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
}

func TestSummarize(t *testing.T) {
	t.Parallel()

	pod := makePod("myapp", "prod", corev1.PodRunning, 7)
	s := summarize(pod)

	if s.Name != "myapp" {
		t.Errorf("Name = %q, want myapp", s.Name)
	}
	if s.Namespace != "prod" {
		t.Errorf("Namespace = %q, want prod", s.Namespace)
	}
	if s.Phase != corev1.PodRunning {
		t.Errorf("Phase = %q, want Running", s.Phase)
	}
	if s.Restarts != 7 {
		t.Errorf("Restarts = %d, want 7", s.Restarts)
	}
}

func ExampleNew() {
	cs := fake.NewSimpleClientset(
		makePod("web", "default", corev1.PodRunning, 0),
	)
	w := New(cs, "default", func(PodEvent) {})
	pods, _ := w.ListPods(context.Background())
	fmt.Printf("found %d pod(s)\n", len(pods))
	// Output: found 1 pod(s)
}
```

The `ExampleNew` function uses `// Output:` so `go test` verifies it automatically. Your turn: add `TestListPodsFiltersOtherNamespace` that pre-populates three pods across two namespaces and asserts the watcher only returns pods from its configured namespace.

### Exercise 3: The Demo Command

Create `cmd/demo/main.go`. Because `cmd/demo` is `package main`, it can only use the exported API of `podwatcher`.

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"example.com/podwatcher"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {
	ns := flag.String("namespace", "default", "namespace to watch")
	kubeconfig := flag.String("kubeconfig", defaultKubeconfig(), "path to kubeconfig")
	flag.Parse()

	cfg, err := buildConfig(*kubeconfig)
	if err != nil {
		log.Fatalf("build config: %v", err)
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("new clientset: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w := podwatcher.New(cs, *ns, func(ev podwatcher.PodEvent) {
		fmt.Printf("[%s] %s/%s phase=%s restarts=%d\n",
			ev.Kind, ev.Pod.Namespace, ev.Pod.Name, ev.Pod.Phase, ev.Pod.Restarts)
	})

	// One-shot list before starting the watch.
	pods, err := w.ListPods(ctx)
	if err != nil {
		log.Fatalf("list pods: %v", err)
	}
	fmt.Printf("--- existing pods in %q ---\n", *ns)
	for _, p := range pods {
		fmt.Printf("  %-40s %-12s restarts=%d\n", p.Name, p.Phase, p.Restarts)
	}

	fmt.Println("--- watching for changes (Ctrl-C to stop) ---")
	if err := w.Run(ctx); err != nil {
		if err.Error() != "context deadline exceeded" {
			log.Printf("watcher stopped: %v", err)
		}
	}
	fmt.Println("shutdown complete")
}

func buildConfig(kubeconfigPath string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
}

func defaultKubeconfig() string {
	if home := homedir.HomeDir(); home != "" {
		return filepath.Join(home, ".kube", "config")
	}
	return ""
}
```

Run it against a real cluster (kind or minikube):

```bash
go run ./cmd/demo -namespace kube-system
```

## Common Mistakes

### Using `*kubernetes.Clientset` Instead of `kubernetes.Interface`

Wrong: a function or struct field accepts `*kubernetes.Clientset`. Tests cannot inject `fake.NewSimpleClientset` because `*fake.Clientset` and `*kubernetes.Clientset` are unrelated concrete types.

What happens: `cannot use fake.NewSimpleClientset(...) (value of type *fake.Clientset) as type *kubernetes.Clientset`.

Fix: accept `kubernetes.Interface`. Both `*kubernetes.Clientset` and `*fake.Clientset` implement it.

### Listing From the Cache Before `WaitForCacheSync`

Wrong: starting the informer with `factory.Start(ctx.Done())` and immediately listing from the lister returned by `factory.Core().V1().Pods().Lister()` in another goroutine.

What happens: the lister returns empty results or stale data because the initial List-and-populate step has not finished.

Fix: always call `cache.WaitForCacheSync(ctx.Done(), informer.HasSynced)` and check its return value before reading from any lister.

### Using `ResourceEventHandlerFuncs` When You Need `isInInitialList`

Wrong: using `cache.ResourceEventHandlerFuncs` with a two-argument `AddFunc` literal because you need `isInInitialList`:

```go
cache.ResourceEventHandlerFuncs{
	AddFunc: func(obj interface{}, isInInitialList bool) { /* ... */ },
}
```

What happens: compile error — `ResourceEventHandlerFuncs.AddFunc` is `func(obj interface{})` (single argument). The two-argument literal does not match.

Fix: switch to `cache.ResourceEventHandlerDetailedFuncs`, which is designed for exactly this case:

```go
cache.ResourceEventHandlerDetailedFuncs{
	AddFunc: func(obj interface{}, isInInitialList bool) { /* ... */ },
}
```

Both types implement `ResourceEventHandler` and are accepted by `AddEventHandler`. Use `ResourceEventHandlerFuncs` when you do not need `isInInitialList`, and `ResourceEventHandlerDetailedFuncs` when you do.

### Ignoring the Tombstone in `DeleteFunc`

Wrong: type-asserting directly to `*corev1.Pod` in `DeleteFunc` and dropping objects that fail.

What happens: when the informer's watch connection drops and the object is evicted from the cache before the delete event arrives, Kubernetes wraps the last known state in `cache.DeletedFinalStateUnknown`. The direct assertion returns `ok = false` and the event is silently dropped.

Fix: check for `DeletedFinalStateUnknown` as a fallback (shown in Exercise 1).

### Not Checking `factory.WaitForCacheSync` Return Value

Wrong:

```go
factory.WaitForCacheSync(ctx.Done())
// assume synced
```

What happens: if the context was cancelled before sync completed, `WaitForCacheSync` returns `false` but the code continues with an unsynced cache.

Fix: check the return value and handle the failure path, as shown in `Run`.

## Verification

From `~/go-exercises/podwatcher`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass with no cluster. The `cmd/demo` binary requires a reachable cluster or kubeconfig but the test suite does not.

## Summary

- `client-go` has three abstraction levels: raw REST, typed clientsets, and informers; most production code uses informers for efficiency.
- Accept `kubernetes.Interface`, not `*kubernetes.Clientset`, so tests can inject `fake.NewSimpleClientset`.
- `rest.InClusterConfig()` and `clientcmd.BuildConfigFromFlags` are the two config sources; the canonical pattern tries in-cluster first.
- Informers cache state locally and fire event callbacks; always call `cache.WaitForCacheSync` before reading from any lister.
- Use `cache.ResourceEventHandlerDetailedFuncs` (v0.28+) when you need `isInInitialList bool` in `AddFunc`; `ResourceEventHandlerFuncs.AddFunc` remains `func(obj interface{})`. The `DeleteFunc` must handle `cache.DeletedFinalStateUnknown`.
- `fake.NewSimpleClientset(objects ...runtime.Object)` enables hermetic tests with no cluster.

## What's Next

Next: [Kubernetes Controller](../07-kubernetes-controller/07-kubernetes-controller.md).

## Resources

- [k8s.io/client-go/kubernetes](https://pkg.go.dev/k8s.io/client-go/kubernetes) — Clientset and Interface definitions
- [k8s.io/client-go/informers](https://pkg.go.dev/k8s.io/client-go/informers) — SharedInformerFactory, WithNamespace, WaitForCacheSync
- [k8s.io/client-go/tools/cache](https://pkg.go.dev/k8s.io/client-go/tools/cache) — ResourceEventHandlerFuncs, DeletedFinalStateUnknown, WaitForCacheSync
- [k8s.io/client-go/kubernetes/fake](https://pkg.go.dev/k8s.io/client-go/kubernetes/fake) — NewSimpleClientset for hermetic tests
- [client-go examples](https://github.com/kubernetes/client-go/tree/master/examples) — official worked examples including out-of-cluster and informers
