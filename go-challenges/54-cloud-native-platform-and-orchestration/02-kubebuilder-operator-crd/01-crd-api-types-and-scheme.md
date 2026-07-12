# Exercise 1: Define CRD API Types and Register the Scheme

The CRD is your operator's public API, so we build its Go representation with the
same care as a wire contract: a spec/status-separated root type, a List type, the
validation and subresource markers that become the server-side schema, the
group-version wiring that maps Go types to a `GroupVersionKind`, and a hand-written
DeepCopy that guarantees the shared informer cache can never be corrupted.

This module is fully self-contained. It defines the whole `api/v1` package, its
scheme registration, its deep-copy methods, a demo, and its tests. Because it
depends on `k8s.io/apimachinery` and `sigs.k8s.io/controller-runtime`, it is a
bar-mode module: it is built and tested where those modules and a Go toolchain are
available, not in the offline gate.

## What you'll build

```text
cacheoperator/                     module: example.com/cacheoperator
  go.mod                           go 1.26; k8s.io/apimachinery + controller-runtime
  api/
    v1/
      groupversion_info.go         GroupVersion, SchemeBuilder, AddToScheme
      cachecluster_types.go        CacheCluster{Spec,Status}, CacheClusterList, markers
      zz_generated.deepcopy.go     hand-written DeepCopyInto/DeepCopy/DeepCopyObject
      cachecluster_test.go         scheme GVK, JSON round-trip, deep-copy independence
  cmd/
    demo/
      main.go                      marshal to JSON, read back, prove deep-copy isolation
```

Files: `api/v1/groupversion_info.go`, `api/v1/cachecluster_types.go`, `api/v1/zz_generated.deepcopy.go`, `cmd/demo/main.go`, `api/v1/cachecluster_test.go`.
Implement: a `CacheCluster` root type with separated `Spec`/`Status`, a `CacheClusterList`, the scheme `Builder`, and correct deep copies for every reference-typed field.
Test: register into a fresh `runtime.Scheme` and assert `ObjectKinds` returns the expected GVK; JSON round-trip a populated CR; mutate a deep copy and assert the original is untouched.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/02-kubebuilder-operator-crd/01-crd-api-types-and-scheme/api/v1 go-solutions/54-cloud-native-platform-and-orchestration/02-kubebuilder-operator-crd/01-crd-api-types-and-scheme/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/02-kubebuilder-operator-crd/01-crd-api-types-and-scheme
go mod edit -go=1.26
go get k8s.io/apimachinery@v0.32.0
go get sigs.k8s.io/controller-runtime@v0.20.0
```

### The group-version wiring

A Go type only becomes an API object once it is registered in a scheme under a
`GroupVersion`. The idiomatic kubebuilder layout puts that wiring in its own file.
`GroupVersion` names the API group and version; `SchemeBuilder` is a
`sigs.k8s.io/controller-runtime/pkg/scheme.Builder` bound to that group-version;
`AddToScheme` is the exported entry point the operator's bootstrap calls to teach a
`runtime.Scheme` about your types. The `Builder.Register` call in the types file's
`init` records which Go types belong to this group-version, and `AddToScheme` runs
those registrations against a target scheme.

Create `api/v1/groupversion_info.go`:

```go
// api/v1/groupversion_info.go
// Package v1 contains the v1 API types for the cache.platform.example.com group.
// +kubebuilder:object:generate=true
// +groupName=cache.platform.example.com
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group and version that identifies every type in this
// package. It is the "apiVersion" users write in their manifests.
var GroupVersion = schema.GroupVersion{Group: "cache.platform.example.com", Version: "v1"}

// SchemeBuilder collects the Go types belonging to this GroupVersion so they can
// be added to a runtime.Scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme registers this package's types into the given scheme. The operator
// bootstrap calls it alongside the client-go core types.
var AddToScheme = SchemeBuilder.AddToScheme
```

### The spec and status types

The root type inlines `metav1.TypeMeta` (which supplies `apiVersion`/`kind` and,
for free, the `GetObjectKind` half of `runtime.Object`) and `metav1.ObjectMeta`
(name, namespace, labels, `generation`). Then it has exactly two payload fields:
`Spec` (desired, user-owned) and `Status` (observed, controller-owned). The
markers matter as much as the fields. `+kubebuilder:object:root=true` says "this
is a top-level kind, generate deepcopy for it." `+kubebuilder:subresource:status`
is the non-negotiable one from the concepts: it gives status its own update
endpoint so status writes never bump `metadata.generation` or race spec writes.
The `+kubebuilder:validation:*` markers become the server-side OpenAPI schema, so
the API server rejects a bad `engine` or an out-of-range `replicas` before your
controller ever sees it. `Replicas` is a pointer with `+optional` and a
`+kubebuilder:default` so "unset" is representable and the server fills the
default uniformly. `Endpoints` carries `+listType=atomic` deliberately, and
`Conditions` uses the standard `+listType=map` keyed by `type` so server-side
apply merges conditions by type instead of replacing the whole list.

Create `api/v1/cachecluster_types.go`:

```go
// api/v1/cachecluster_types.go
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaintenanceWindow is an optional nested struct; it exercises pointer-field deep
// copy in zz_generated.deepcopy.go.
type MaintenanceWindow struct {
	// Weekday is 0 (Sunday) through 6 (Saturday).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=6
	Weekday int32 `json:"weekday"`
	// StartHourUTC is the window start hour in UTC.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=23
	StartHourUTC int32 `json:"startHourUTC"`
}

// CacheClusterSpec is the desired state; it is owned by the user and read-only to
// the controller.
type CacheClusterSpec struct {
	// Engine is the cache engine to provision.
	// +kubebuilder:validation:Enum=redis;memcached
	// +required
	Engine string `json:"engine"`

	// Version is the engine version, e.g. "7.2".
	// +kubebuilder:validation:MinLength=1
	// +required
	Version string `json:"version"`

	// Replicas is the desired number of cache members. Pointer + default so an
	// unset value is distinct from an explicit zero.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// AllowedCIDRs restricts client access. Slice field: deep-copied.
	// +listType=atomic
	// +optional
	AllowedCIDRs []string `json:"allowedCIDRs,omitempty"`

	// Tags are propagated to created resources. Map field: deep-copied.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// Maintenance is an optional maintenance window. Pointer field: deep-copied.
	// +optional
	Maintenance *MaintenanceWindow `json:"maintenance,omitempty"`
}

// CacheClusterStatus is the observed state; it is owned by the controller and
// written through the status subresource.
type CacheClusterStatus struct {
	// Phase is a coarse, human-facing summary.
	// +optional
	Phase string `json:"phase,omitempty"`

	// ObservedGeneration is the metadata.generation the controller last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ReadyReplicas is the number of members currently serving.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Endpoints are the reachable member addresses.
	// +listType=atomic
	// +optional
	Endpoints []string `json:"endpoints,omitempty"`

	// Conditions is the standard condition list, merged by type under SSA.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CacheCluster is the Schema for the cacheclusters API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
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
```

### DeepCopy: the file controller-gen would generate

In a real project you never write this file — `controller-gen object` reads the
`+kubebuilder:object:root=true` markers and emits `zz_generated.deepcopy.go`. We
write it by hand so the contract is explicit and the package compiles without the
generator. The contract is the one from the concepts section: the shared cache
hands out read-only pointers, so any mutation must happen on a deep copy, and a
deep copy is only correct if it allocates fresh backing storage for every
reference-typed field. Watch the pattern for each field kind. A pointer field
(`Replicas`, `Maintenance`) gets `new(T)` and a value copy of the pointee — never
a copied pointer. A slice field (`AllowedCIDRs`, `Endpoints`) gets a fresh
`make([]T, len)` and `copy`. A map field (`Tags`) gets a fresh `make(map...)` and
a per-entry copy. A slice of structs that themselves own references (`Conditions`)
is deep-copied element by element via each element's own `DeepCopyInto`. Embedded
`ObjectMeta` has its own generated `DeepCopyInto`, so we delegate to it; embedded
`TypeMeta` is all value fields, so a plain assignment suffices. `DeepCopyObject`
is the one `runtime.Object` requires; it just wraps `DeepCopy`.

Create `api/v1/zz_generated.deepcopy.go`:

```go
// api/v1/zz_generated.deepcopy.go
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out. Every reference-typed field gets
// fresh backing storage so out never aliases the receiver.
func (in *MaintenanceWindow) DeepCopyInto(out *MaintenanceWindow) {
	*out = *in
}

// DeepCopy returns a deep copy of the MaintenanceWindow.
func (in *MaintenanceWindow) DeepCopy() *MaintenanceWindow {
	if in == nil {
		return nil
	}
	out := new(MaintenanceWindow)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the spec, allocating fresh storage for pointer, slice, and
// map fields.
func (in *CacheClusterSpec) DeepCopyInto(out *CacheClusterSpec) {
	*out = *in
	if in.Replicas != nil {
		in, out := &in.Replicas, &out.Replicas
		*out = new(int32)
		**out = **in
	}
	if in.AllowedCIDRs != nil {
		in, out := &in.AllowedCIDRs, &out.AllowedCIDRs
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.Tags != nil {
		in, out := &in.Tags, &out.Tags
		*out = make(map[string]string, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
	if in.Maintenance != nil {
		in, out := &in.Maintenance, &out.Maintenance
		*out = new(MaintenanceWindow)
		**out = **in
	}
}

// DeepCopy returns a deep copy of the spec.
func (in *CacheClusterSpec) DeepCopy() *CacheClusterSpec {
	if in == nil {
		return nil
	}
	out := new(CacheClusterSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the status, deep-copying the endpoints slice and each
// condition element.
func (in *CacheClusterStatus) DeepCopyInto(out *CacheClusterStatus) {
	*out = *in
	if in.Endpoints != nil {
		in, out := &in.Endpoints, &out.Endpoints
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy returns a deep copy of the status.
func (in *CacheClusterStatus) DeepCopy() *CacheClusterStatus {
	if in == nil {
		return nil
	}
	out := new(CacheClusterStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies a CacheCluster, delegating to each member's DeepCopyInto.
func (in *CacheCluster) DeepCopyInto(out *CacheCluster) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
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
		in, out := &in.Items, &out.Items
		*out = make([]CacheCluster, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
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

Note the condition slice is deep-copied element by element through each
`metav1.Condition`'s own `DeepCopyInto`, because a condition can carry its own
allocated fields; a plain `copy` of the slice header would share that backing
array with the original.

### The runnable demo

The demo marshals a fully-populated `CacheCluster` to JSON, reads it back into a
fresh value, and proves both the round-trip and — more importantly — that a deep
copy is truly independent: it copies the object, bumps the copy's `Replicas` to 9,
and shows the original still reads 3. That is the exact property that keeps the
shared informer cache safe.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cachev1 "example.com/cacheoperator/api/v1"
)

func int32p(v int32) *int32 { return &v }

func main() {
	cc := &cachev1.CacheCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cache.platform.example.com/v1",
			Kind:       "CacheCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "sessions", Namespace: "prod", Generation: 3},
		Spec: cachev1.CacheClusterSpec{
			Engine:       "redis",
			Version:      "7.2",
			Replicas:     int32p(3),
			AllowedCIDRs: []string{"10.0.0.0/8"},
			Tags:         map[string]string{"team": "platform"},
		},
		Status: cachev1.CacheClusterStatus{Phase: "Ready", ObservedGeneration: 3, ReadyReplicas: 3},
	}

	data, err := json.Marshal(cc)
	if err != nil {
		panic(err)
	}

	var back cachev1.CacheCluster
	if err := json.Unmarshal(data, &back); err != nil {
		panic(err)
	}

	fmt.Printf("round-trip kind=%s apiVersion=%s\n", back.Kind, back.APIVersion)
	fmt.Printf("spec: engine=%s version=%s replicas=%d\n", back.Spec.Engine, back.Spec.Version, *back.Spec.Replicas)
	fmt.Printf("status: phase=%s observedGeneration=%d readyReplicas=%d\n", back.Status.Phase, back.Status.ObservedGeneration, back.Status.ReadyReplicas)

	cp := cc.DeepCopy()
	*cp.Spec.Replicas = 9
	cp.Spec.Tags["team"] = "sre"
	fmt.Printf("deepcopy isolated: original replicas=%d team=%s; copy replicas=%d team=%s\n",
		*cc.Spec.Replicas, cc.Spec.Tags["team"], *cp.Spec.Replicas, cp.Spec.Tags["team"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
round-trip kind=CacheCluster apiVersion=cache.platform.example.com/v1
spec: engine=redis version=7.2 replicas=3
status: phase=Ready observedGeneration=3 readyReplicas=3
deepcopy isolated: original replicas=3 team=platform; copy replicas=9 team=sre
```

### Tests

The tests assert the three properties that make the API type safe. `TestScheme`
registers the package into a fresh `runtime.NewScheme()` through `AddToScheme` and
asserts `Scheme.ObjectKinds` maps the Go value to the expected `GroupVersionKind`
— the proof that registration works and the type will (de)serialize. `TestJSONRoundTrip`
marshals a populated CR and unmarshals it, asserting `apiVersion`, `kind`, spec,
and status all survive. `TestDeepCopyIndependence` is the important one: it deep-
copies an object with a populated pointer, slice, map, nested pointer, and
condition list, mutates every one of those on the copy, and asserts the original
is byte-for-byte unchanged — a shallow copy would fail this and silently corrupt
the cache.

Create `api/v1/cachecluster_test.go`:

```go
package v1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func int32p(v int32) *int32 { return &v }

func TestScheme(t *testing.T) {
	t.Parallel()
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	want := schema.GroupVersionKind{
		Group:   "cache.platform.example.com",
		Version: "v1",
		Kind:    "CacheCluster",
	}
	gvks, _, err := s.ObjectKinds(&CacheCluster{})
	if err != nil {
		t.Fatalf("ObjectKinds: %v", err)
	}
	found := false
	for _, gvk := range gvks {
		if gvk == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("ObjectKinds = %v; want to contain %v", gvks, want)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := &CacheCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cache.platform.example.com/v1",
			Kind:       "CacheCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "sessions", Generation: 2},
		Spec: CacheClusterSpec{
			Engine:   "redis",
			Version:  "7.2",
			Replicas: int32p(3),
			Tags:     map[string]string{"team": "platform"},
		},
		Status: CacheClusterStatus{Phase: "Ready", ObservedGeneration: 2, ReadyReplicas: 3},
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out CacheCluster
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if out.Kind != "CacheCluster" || out.APIVersion != "cache.platform.example.com/v1" {
		t.Fatalf("TypeMeta lost: kind=%q apiVersion=%q", out.Kind, out.APIVersion)
	}
	if out.Spec.Engine != "redis" || out.Spec.Version != "7.2" || out.Spec.Replicas == nil || *out.Spec.Replicas != 3 {
		t.Fatalf("spec lost: %+v", out.Spec)
	}
	if out.Status.ObservedGeneration != 2 || out.Status.ReadyReplicas != 3 {
		t.Fatalf("status lost: %+v", out.Status)
	}
}

func TestDeepCopyIndependence(t *testing.T) {
	t.Parallel()
	orig := &CacheCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Labels: map[string]string{"k": "v"}},
		Spec: CacheClusterSpec{
			Engine:       "redis",
			Version:      "7.2",
			Replicas:     int32p(3),
			AllowedCIDRs: []string{"10.0.0.0/8"},
			Tags:         map[string]string{"team": "platform"},
			Maintenance:  &MaintenanceWindow{Weekday: 1, StartHourUTC: 2},
		},
		Status: CacheClusterStatus{
			Endpoints:  []string{"a:6379"},
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Converged"}},
		},
	}

	cp := orig.DeepCopy()
	// Mutate every reference-typed field on the copy.
	*cp.Spec.Replicas = 9
	cp.Spec.AllowedCIDRs[0] = "0.0.0.0/0"
	cp.Spec.Tags["team"] = "sre"
	cp.Spec.Maintenance.Weekday = 6
	cp.Status.Endpoints[0] = "b:6379"
	cp.Status.Conditions[0].Reason = "Failed"
	cp.ObjectMeta.Labels["k"] = "mutated"

	if *orig.Spec.Replicas != 3 {
		t.Errorf("Replicas aliased: got %d", *orig.Spec.Replicas)
	}
	if orig.Spec.AllowedCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("AllowedCIDRs aliased: got %q", orig.Spec.AllowedCIDRs[0])
	}
	if orig.Spec.Tags["team"] != "platform" {
		t.Errorf("Tags aliased: got %q", orig.Spec.Tags["team"])
	}
	if orig.Spec.Maintenance.Weekday != 1 {
		t.Errorf("Maintenance aliased: got %d", orig.Spec.Maintenance.Weekday)
	}
	if orig.Status.Endpoints[0] != "a:6379" {
		t.Errorf("Endpoints aliased: got %q", orig.Status.Endpoints[0])
	}
	if orig.Status.Conditions[0].Reason != "Converged" {
		t.Errorf("Conditions aliased: got %q", orig.Status.Conditions[0].Reason)
	}
	if orig.ObjectMeta.Labels["k"] != "v" {
		t.Errorf("ObjectMeta aliased: got %q", orig.ObjectMeta.Labels["k"])
	}
}

func TestDeepCopyObjectType(t *testing.T) {
	t.Parallel()
	var obj runtime.Object = (&CacheCluster{Spec: CacheClusterSpec{Engine: "redis"}}).DeepCopyObject()
	cc, ok := obj.(*CacheCluster)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T; want *CacheCluster", obj)
	}
	if cc.Spec.Engine != "redis" {
		t.Fatalf("DeepCopyObject dropped data: %+v", cc.Spec)
	}
}
```

## Review

The API type is correct when three things hold. First, registration: a fresh
scheme fed through `AddToScheme` maps `&CacheCluster{}` to
`cache.platform.example.com/v1, Kind=CacheCluster`; if `ObjectKinds` errors with
"no kind is registered," the `SchemeBuilder.Register` call in `init` is missing or
the group-version is wrong. Second, serialization: the inlined `TypeMeta` and the
`spec`/`status` fields survive a JSON round-trip, which they only do if the json
tags match the API convention (lowercase, `,inline` on `TypeMeta`, `metadata` on
`ObjectMeta`). Third, and most important, deep-copy independence: mutating a copy's
pointer, slice, map, nested-pointer, and condition fields must leave the original
untouched. The classic bug is a `DeepCopyInto` that stops at `*out = *in`; that
compiles and passes a naive test, then corrupts the shared informer cache in
production because every reference field still aliases the original. The
`TestDeepCopyIndependence` test exists specifically to catch that. Two more traps
worth internalizing from this code: making `Replicas` a bare `int32` instead of
`*int32` would make "unset" and "zero" indistinguishable, and dropping
`+kubebuilder:subresource:status` would let status writes bump
`metadata.generation` — neither is caught by a Go test, only by understanding the
contract.

## Resources

- [Kubebuilder Book — Designing an API](https://book.kubebuilder.io/cronjob-tutorial/api-design) — spec/status separation and the validation/subresource markers.
- [Kubebuilder Book — GroupVersion, SchemeBuilder, AddToScheme](https://book.kubebuilder.io/cronjob-tutorial/other-api-files) — the `groupversion_info.go` wiring this exercise mirrors.
- [`sigs.k8s.io/controller-runtime/pkg/scheme`](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/scheme) — the `scheme.Builder` type, `Register`, and `AddToScheme`.
- [`k8s.io/apimachinery/pkg/runtime`](https://pkg.go.dev/k8s.io/apimachinery/pkg/runtime#Object) — the `runtime.Object` interface and `Scheme.ObjectKinds`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-status-conditions-and-observed-state.md](02-status-conditions-and-observed-state.md)
