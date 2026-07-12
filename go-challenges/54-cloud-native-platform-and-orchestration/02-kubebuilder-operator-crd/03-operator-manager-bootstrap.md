# Exercise 3: Bootstrap the Operator Manager and Scheme Wiring

The operator process is a `manager` that owns the shared cache, the clients, the
metrics and health servers, and leader election. This exercise builds the
bootstrap the testable way: the scheme composition, the `manager.Options` builder,
and the health checkers are exercised in-process, while the one call that needs a
live API server — `mgr.Start` — sits behind a build tag so the module builds and
tests without cluster credentials.

This module is self-contained. It bundles a minimal `api/v1` (so it does not
import Exercise 1) and depends on `k8s.io/client-go` and
`sigs.k8s.io/controller-runtime`, making it bar-mode: built and tested where those
modules are available, not in the offline gate.

## What you'll build

```text
operator/                      module: example.com/operator
  go.mod                       go 1.26; client-go + controller-runtime
  api/
    v1/
      groupversion_info.go     GroupVersion, SchemeBuilder, AddToScheme
      cachecluster_types.go    minimal CacheCluster + List + DeepCopyObject
  setup.go                     BuildScheme, Config, BuildOptions
  run.go                       //go:build integration: NewManager + Start
  cmd/
    demo/
      main.go                  compose scheme, build options, run healthz.Ping
  setup_test.go                scheme GVKs, options fields, healthz.Ping
```

Files: `api/v1/groupversion_info.go`, `api/v1/cachecluster_types.go`, `setup.go`, `run.go`, `cmd/demo/main.go`, `setup_test.go`.
Implement: `BuildScheme` (client-go core types + your CR), `BuildOptions` (metrics/probe addresses, leader election), and a build-tagged `Run` that wires health checks and starts the manager.
Test: assert the scheme recognizes both `corev1.Pod` and `CacheCluster`; assert `BuildOptions` returns the configured `LeaderElectionID`, `HealthProbeBindAddress`, and `Metrics.BindAddress`; invoke `healthz.Ping` with a stub request and assert nil.
Verify: `go test -count=1 -race ./...` (the cluster path builds only under `-tags integration`).

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/02-kubebuilder-operator-crd/03-operator-manager-bootstrap/api/v1 go-solutions/54-cloud-native-platform-and-orchestration/02-kubebuilder-operator-crd/03-operator-manager-bootstrap/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/02-kubebuilder-operator-crd/03-operator-manager-bootstrap
go mod edit -go=1.26
go get k8s.io/api@v0.32.0 k8s.io/apimachinery@v0.32.0 k8s.io/client-go@v0.32.0
go get sigs.k8s.io/controller-runtime@v0.20.0
```

### Why the cluster call is the only thing behind a build tag

Almost everything a bootstrap does is pure, in-process assembly: constructing a
`runtime.Scheme`, registering types, filling a `manager.Options` struct, defining
health checkers. None of that needs a cluster, and all of it is where the bugs
live — a forgotten `AddToScheme`, a wrong `LeaderElectionID`, a probe bound to the
wrong address. The single call that genuinely requires an API server is
`ctrl.NewManager` followed by `mgr.Start`, because the manager immediately builds
a cache backed by a real client and blocks serving. So the design splits the file:
the pure builders live in `setup.go` and are unit-tested directly, and the
cluster-bound `Run` lives in `run.go` under `//go:build integration`. Ordinary
`go build` and `go test` compile everything except `run.go`; a CI job with kube
credentials runs `go test -tags integration` to exercise the real start path. This
is the standard way to keep an operator's bootstrap testable without a Kind cluster
in every unit run.

### The bundled API package

To stay independent of Exercise 1, this module carries its own slim `api/v1`. It
is the same wiring — `GroupVersion`, `SchemeBuilder`, `AddToScheme`, and a
`CacheCluster` with hand-written `DeepCopyObject` — trimmed to the minimum the
bootstrap needs.

Create `api/v1/groupversion_info.go`:

```go
// api/v1/groupversion_info.go
// Package v1 contains the minimal v1 API types for the operator bootstrap.
// +groupName=cache.platform.example.com
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion identifies this package's group and version.
var GroupVersion = schema.GroupVersion{Group: "cache.platform.example.com", Version: "v1"}

// SchemeBuilder collects this package's types for registration.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme registers this package's types into a scheme.
var AddToScheme = SchemeBuilder.AddToScheme
```

Create `api/v1/cachecluster_types.go`:

```go
// api/v1/cachecluster_types.go
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// CacheClusterSpec is the desired state (minimal here).
type CacheClusterSpec struct {
	// +kubebuilder:validation:Enum=redis;memcached
	Engine string `json:"engine"`
}

// CacheClusterStatus is the observed state (minimal here).
type CacheClusterStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
}

// CacheCluster is the Schema for the cacheclusters API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type CacheCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CacheClusterSpec   `json:"spec,omitempty"`
	Status CacheClusterStatus `json:"status,omitempty"`
}

// CacheClusterList is the Schema for the cacheclusters list.
// +kubebuilder:object:root=true
type CacheClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CacheCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CacheCluster{}, &CacheClusterList{})
}

// DeepCopyInto copies a CacheCluster. Spec and Status hold only value fields, so
// a plain assignment is a correct deep copy for them.
func (in *CacheCluster) DeepCopyInto(out *CacheCluster) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

// DeepCopy returns a deep copy of the CacheCluster.
func (in *CacheCluster) DeepCopy() *CacheCluster {
	if in == nil {
		return nil
	}
	out := new(CacheCluster)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *CacheCluster) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies a CacheClusterList, deep-copying every item.
func (in *CacheClusterList) DeepCopyInto(out *CacheClusterList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		items := make([]CacheCluster, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&items[i])
		}
		out.Items = items
	}
}

// DeepCopy returns a deep copy of the list.
func (in *CacheClusterList) DeepCopy() *CacheClusterList {
	if in == nil {
		return nil
	}
	out := new(CacheClusterList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *CacheClusterList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
```

### The scheme and options builders

`BuildScheme` composes two registrations into a fresh scheme: `clientgoscheme.AddToScheme`
teaches it every built-in Kubernetes type (so the manager's cache can watch Pods,
Services, and the like), and your `cachev1.AddToScheme` adds the CR. A manager
whose scheme is missing the core types cannot build a working cache; a manager
missing your CR cannot serve your kind. `BuildOptions` is a pure function from a
small `Config` to `manager.Options`: it plugs the scheme in, sets the metrics bind
address via `metricsserver.Options{BindAddress: ...}`, sets the health probe
address, and turns on leader election with the operator's unique
`LeaderElectionID`. Keeping this a pure function is what makes it unit-testable
without a cluster.

Create `setup.go`:

```go
// Package operator wires the scheme and manager options for the CacheCluster
// operator. The cluster-bound start path lives in run.go behind a build tag.
package operator

import (
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cachev1 "example.com/operator/api/v1"
)

// Config holds the operator's runtime knobs.
type Config struct {
	MetricsAddr          string
	ProbeAddr            string
	LeaderElectionID     string
	EnableLeaderElection bool
}

// BuildScheme returns a scheme containing the built-in Kubernetes types and the
// CacheCluster CR. A missing registration is a startup bug, so both are checked.
func BuildScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := cachev1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// BuildOptions converts a Config into manager.Options. It is a pure function so it
// can be unit-tested without a cluster.
func BuildOptions(scheme *runtime.Scheme, cfg Config) manager.Options {
	return manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress: cfg.ProbeAddr,
		LeaderElection:         cfg.EnableLeaderElection,
		LeaderElectionID:       cfg.LeaderElectionID,
	}
}
```

### The cluster-bound start path

`Run` is the only code that touches a real cluster, so it is the only code behind
`//go:build integration`. It builds the scheme, constructs the manager against the
ambient kubeconfig (`ctrl.GetConfigOrDie`), registers a liveness and a readiness
check with `healthz.Ping`, and blocks in `mgr.Start` under a signal-aware context
from `ctrl.SetupSignalHandler` (which cancels on SIGINT/SIGTERM for graceful
shutdown). `ctrl.NewManager`, `ctrl.GetConfigOrDie`, and `ctrl.SetupSignalHandler`
are convenience aliases the controller-runtime root package exposes.

Create `run.go`:

```go
//go:build integration

// run.go holds the cluster-bound start path, compiled only with -tags integration
// so unit builds and tests need no kube credentials.
package operator

import (
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// Run builds the manager and starts it. It blocks until the process receives
// SIGINT or SIGTERM.
func Run(cfg Config) error {
	scheme, err := BuildScheme()
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), BuildOptions(scheme, cfg))
	if err != nil {
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	return mgr.Start(ctrl.SetupSignalHandler())
}
```

### The runnable demo

The demo runs the whole pure bootstrap without a cluster: it composes the scheme,
proves the scheme recognizes both a core type (`corev1.Pod`) and the CR, builds the
options and prints the configured addresses, and invokes the same `healthz.Ping`
checker the manager would register — showing it returns nil for a healthy probe.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"example.com/operator"
	cachev1 "example.com/operator/api/v1"
)

func main() {
	scheme, err := operator.BuildScheme()
	if err != nil {
		panic(err)
	}

	cfg := operator.Config{
		MetricsAddr:          ":8080",
		ProbeAddr:            ":8081",
		LeaderElectionID:     "cachecluster.platform.example.com",
		EnableLeaderElection: true,
	}
	opts := operator.BuildOptions(scheme, cfg)

	_, _, coreErr := scheme.ObjectKinds(&corev1.Pod{})
	fmt.Printf("scheme recognizes core/v1 Pod: %v\n", coreErr == nil)

	gvks, _, err := scheme.ObjectKinds(&cachev1.CacheCluster{})
	if err != nil {
		panic(err)
	}
	fmt.Printf("scheme recognizes CacheCluster: %s\n", gvks[0])

	fmt.Printf("metrics bind: %s\n", opts.Metrics.BindAddress)
	fmt.Printf("health probe bind: %s\n", opts.HealthProbeBindAddress)
	fmt.Printf("leader election: %v (id=%s)\n", opts.LeaderElection, opts.LeaderElectionID)

	req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	fmt.Printf("healthz ping error: %v\n", healthz.Ping(req))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scheme recognizes core/v1 Pod: true
scheme recognizes CacheCluster: cache.platform.example.com/v1, Kind=CacheCluster
metrics bind: :8080
health probe bind: :8081
leader election: true (id=cachecluster.platform.example.com)
healthz ping error: <nil>
```

### Tests

The tests cover the three pure pieces without a cluster. `TestBuildSchemeRecognizesTypes`
asserts the composed scheme maps both a built-in type and the CR to GVKs — the
proof that both registrations ran. `TestBuildOptions` asserts the builder threaded
every `Config` value into the right `manager.Options` field, including the
`Metrics.BindAddress` nested option. `TestHealthzPing` invokes the exact checker
the bootstrap registers, with a stub `*http.Request`, and asserts it reports
healthy.

Create `setup_test.go`:

```go
package operator

import (
	"fmt"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	cachev1 "example.com/operator/api/v1"
)

func TestBuildSchemeRecognizesTypes(t *testing.T) {
	t.Parallel()
	s, err := BuildScheme()
	if err != nil {
		t.Fatalf("BuildScheme: %v", err)
	}

	if _, _, err := s.ObjectKinds(&corev1.Pod{}); err != nil {
		t.Fatalf("core/v1 Pod not recognized: %v", err)
	}

	gvks, _, err := s.ObjectKinds(&cachev1.CacheCluster{})
	if err != nil {
		t.Fatalf("CacheCluster not recognized: %v", err)
	}
	const want = "cache.platform.example.com/v1, Kind=CacheCluster"
	if got := gvks[0].String(); got != want {
		t.Fatalf("CacheCluster GVK = %q; want %q", got, want)
	}
}

func TestBuildOptions(t *testing.T) {
	t.Parallel()
	s, err := BuildScheme()
	if err != nil {
		t.Fatalf("BuildScheme: %v", err)
	}
	cfg := Config{
		MetricsAddr:          ":9090",
		ProbeAddr:            ":9091",
		LeaderElectionID:     "op.example.com",
		EnableLeaderElection: true,
	}
	opts := BuildOptions(s, cfg)

	if opts.Scheme != s {
		t.Error("BuildOptions did not thread the scheme")
	}
	if opts.Metrics.BindAddress != ":9090" {
		t.Errorf("Metrics.BindAddress = %q; want :9090", opts.Metrics.BindAddress)
	}
	if opts.HealthProbeBindAddress != ":9091" {
		t.Errorf("HealthProbeBindAddress = %q; want :9091", opts.HealthProbeBindAddress)
	}
	if opts.LeaderElectionID != "op.example.com" {
		t.Errorf("LeaderElectionID = %q; want op.example.com", opts.LeaderElectionID)
	}
	if !opts.LeaderElection {
		t.Error("LeaderElection = false; want true")
	}
}

func TestHealthzPing(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequest(http.MethodGet, "/healthz", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := healthz.Ping(req); err != nil {
		t.Fatalf("healthz.Ping = %v; want nil", err)
	}
}

func ExampleBuildScheme() {
	s, _ := BuildScheme()
	gvks, _, _ := s.ObjectKinds(&cachev1.CacheCluster{})
	fmt.Println(gvks[0])
	// Output: cache.platform.example.com/v1, Kind=CacheCluster
}
```

## Review

The bootstrap is correct when the pure pieces do their job and the impure one is
isolated. The scheme composition is right when a fresh scheme fed through
`BuildScheme` recognizes both a built-in type and the CR; a missing
`clientgoscheme.AddToScheme` shows up as the manager being unable to watch core
resources, and a missing `cachev1.AddToScheme` as a `no kind is registered` panic
the first time your controller touches its own type. `BuildOptions` is right when
every `Config` field lands in the matching `manager.Options` field — note that the
metrics address is nested inside `Metrics metricsserver.Options`, a common place to
mis-set. Health is right when `healthz.Ping` returns nil for a stub request, which
is what Kubernetes needs to gate liveness and readiness. The structural point to
internalize: `mgr.Start` is behind `//go:build integration` not to hide it but
because it is the only call that needs an API server; keep the cluster boundary at
exactly that line so the rest stays unit-testable, and give every operator a
unique `LeaderElectionID` so two operators never contend for one lease.

## Resources

- [`sigs.k8s.io/controller-runtime/pkg/manager`](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/manager) — the `Options` struct and the `Manager` interface (`AddHealthzCheck`, `AddReadyzCheck`, `Start`).
- [`sigs.k8s.io/controller-runtime/pkg/healthz`](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/healthz) — the `Checker` type and the `Ping` checker.
- [`sigs.k8s.io/controller-runtime`](https://pkg.go.dev/sigs.k8s.io/controller-runtime) — the `NewManager`, `GetConfigOrDie`, and `SetupSignalHandler` aliases.
- [Kubebuilder Book — main.go and the manager](https://book.kubebuilder.io/cronjob-tutorial/empty-main) — how the generated bootstrap composes the scheme and starts the manager.

---

Back to [02-status-conditions-and-observed-state.md](02-status-conditions-and-observed-state.md) | Next: [../03-controller-runtime-reconcile/00-concepts.md](../03-controller-runtime-reconcile/00-concepts.md)
