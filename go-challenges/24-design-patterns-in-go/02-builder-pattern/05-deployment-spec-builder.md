# Exercise 5: An Immutable Deployment Spec Builder

A Kubernetes Deployment manifest is a deep, partly-required object: metadata with a name and namespace, a replica count, labels, and a pod template carrying a container with an image, ports, and environment variables. Assembling one by hand is error-prone — a missing name or image is a manifest the API server rejects, and a half-built spec shared between callers is a subtle bug. This exercise builds a `deployment.Builder` that enforces the required fields at `Build` time and returns an *immutable* `Deployment` whose maps and slices are private copies, so the product can never change after it is handed out. A `Render` method emits a deterministic YAML-style manifest, and the test drives the whole thing end to end.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
deployment.go        Deployment (immutable product), Builder, New, setters, Build, Render
cmd/
  demo/
    main.go          build a full Deployment, render it, then print two error cases
deployment_test.go   full render, defaults, required-field enforcement, immutability, reuse
```

- Files: `deployment.go`, `cmd/demo/main.go`, `deployment_test.go`.
- Implement: `Builder` with fluent setters (`Name`, `Namespace`, `Replicas`, `Label`, `Image`, `Container`, `Port`, `Env`) and `Build() (Deployment, error)`, plus the immutable `Deployment` product with read accessors and a `Render` method.
- Test: `deployment_test.go` pins the full rendered manifest, the namespace/replica defaults, required-field enforcement via sentinels, error aggregation, that a built `Deployment` is immutable against later setter calls, and that a failed `Build` does not poison the builder.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p deployment-builder/cmd/demo && cd deployment-builder
go mod init example.com/deployment-builder
```

### Why required fields are enforced at Build, not in setters

A Deployment has fields with no sensible default — a name and a container image — and fields that do have one, like the namespace (`default`) and the replica count (`1`). The builder reflects that division. `New` seeds the defaultable fields, so a caller who never touches them still gets a valid namespace and one replica. The two fields that cannot be defaulted are *not* required by the setters — there is no `New(name, image)` constructor forcing them up front. Instead they are checked once, at `Build`, which appends `ErrNoName` or `ErrNoImage` if either is still empty. Enforcing at `Build` rather than at the setter is what lets the fluent chain stay order-free: a caller may set image before name, or interleave labels and ports however reads best, and the single completeness check at the end catches whatever is missing. The setters that *can* fail on their own argument — a negative replica count, a port outside 1..65535, an empty label — record a sentinel and keep going, so `Build` reports every problem together via `errors.Join`, exactly like the other builders in this lesson.

### Why Build deep-copies, and what "immutable" buys you

The product is the payoff. `Build` does not hand back a struct that shares memory with the builder; it deep-copies every reference-typed field — the labels map, the env map, the ports slice — into a fresh `Deployment`. This is the boundary condition the concepts file flagged: copying a struct copies a slice or map *header*, but the backing array stays shared, so a shallow copy would leave the returned `Deployment` aliasing the builder's maps. A later `b.Label("app", "changed")` would then mutate a `Deployment` someone already received and rendered. By copying the backing data, `Build` severs that link: the returned value owns its own maps and slice, and nothing the builder does afterward can reach it. `TestBuild_ProductIsImmutable` is the proof — it builds, snapshots the render, mutates the builder, and asserts the earlier product is byte-for-byte unchanged.

Immutability here is not decoration; it is what makes the product safe to pass around a service. A finished `Deployment` can be cached, handed to several goroutines, or used as a template without any caller worrying that another holder of the builder will change it underfoot. The `Deployment`'s fields are unexported and exposed through read-only accessors (`Name`, `Namespace`, `Replicas`, `Image`) and `Render`, so there is no exported handle through which the value could be mutated either. The product is immutable both against the builder that made it and against its own holders.

### Why Render is deterministic

A manifest builder that emitted labels and env vars in map-iteration order would produce different text on every run, which makes it untestable and produces noisy diffs in a GitOps repository. `Render` sorts every map's keys before emitting them, so the same spec always renders the same bytes. That determinism is what lets `TestBuild_FullSpecRendersManifest` pin the exact manifest string; without it, the test could only check fragments. Ports render in insertion order, because a slice already has a defined order the caller chose.

Create `deployment.go`:

```go
package deployment

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var (
	ErrNoName       = errors.New("metadata.name is required")
	ErrNoImage      = errors.New("spec.template.container.image is required")
	ErrBadReplicas  = errors.New("spec.replicas must not be negative")
	ErrBadPort      = errors.New("containerPort must be between 1 and 65535")
	ErrEmptyLabel   = errors.New("label key and value must not be empty")
	ErrEmptyEnvName = errors.New("env name must not be empty")
)

// Deployment is the immutable product. Its maps and slices are private copies,
// owned by this value alone: nothing the builder holds aliases them, so a built
// Deployment cannot change after Build returns.
type Deployment struct {
	name      string
	namespace string
	replicas  int
	labels    map[string]string
	image     string
	container string
	ports     []int
	env       map[string]string
}

func (d Deployment) Name() string      { return d.name }
func (d Deployment) Namespace() string { return d.namespace }
func (d Deployment) Replicas() int     { return d.replicas }
func (d Deployment) Image() string     { return d.image }

// Builder accumulates a deployment spec. Setters record problems instead of
// failing fast; Build enforces required fields and returns an immutable
// Deployment. It is mutable and not safe for concurrent use.
type Builder struct {
	name      string
	namespace string
	replicas  int
	labels    map[string]string
	image     string
	container string
	ports     []int
	env       map[string]string
	errs      []error
}

// New returns a builder with the Kubernetes defaults: the "default" namespace
// and a single replica. Name and image have no safe default, so Build refuses a
// spec that omits either.
func New() *Builder {
	return &Builder{
		namespace: "default",
		replicas:  1,
		labels:    make(map[string]string),
		env:       make(map[string]string),
	}
}

func (b *Builder) Name(n string) *Builder      { b.name = n; return b }
func (b *Builder) Namespace(n string) *Builder { b.namespace = n; return b }
func (b *Builder) Image(i string) *Builder     { b.image = i; return b }
func (b *Builder) Container(n string) *Builder { b.container = n; return b }

func (b *Builder) Replicas(n int) *Builder {
	if n < 0 {
		b.errs = append(b.errs, fmt.Errorf("%w: %d", ErrBadReplicas, n))
		return b
	}
	b.replicas = n
	return b
}

func (b *Builder) Label(key, value string) *Builder {
	if key == "" || value == "" {
		b.errs = append(b.errs, fmt.Errorf("%w: %q=%q", ErrEmptyLabel, key, value))
		return b
	}
	b.labels[key] = value
	return b
}

func (b *Builder) Port(p int) *Builder {
	if p < 1 || p > 65535 {
		b.errs = append(b.errs, fmt.Errorf("%w: %d", ErrBadPort, p))
		return b
	}
	b.ports = append(b.ports, p)
	return b
}

func (b *Builder) Env(name, value string) *Builder {
	if name == "" {
		b.errs = append(b.errs, ErrEmptyEnvName)
		return b
	}
	b.env[name] = value
	return b
}

// Build enforces the required fields and returns an immutable Deployment. Every
// map and slice is deep-copied into the product, so later setter calls on the
// builder, or a second Build, cannot reach back and mutate a value already
// handed out. The setter errors are copied into a fresh local slice so a failed
// Build never sticks to the builder.
func (b *Builder) Build() (Deployment, error) {
	errs := append([]error(nil), b.errs...)
	if b.name == "" {
		errs = append(errs, ErrNoName)
	}
	if b.image == "" {
		errs = append(errs, ErrNoImage)
	}
	if len(errs) > 0 {
		return Deployment{}, fmt.Errorf("deployment: %w", errors.Join(errs...))
	}

	container := b.container
	if container == "" {
		container = b.name
	}

	return Deployment{
		name:      b.name,
		namespace: b.namespace,
		replicas:  b.replicas,
		labels:    copyMap(b.labels),
		image:     b.image,
		container: container,
		ports:     append([]int(nil), b.ports...),
		env:       copyMap(b.env),
	}, nil
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Render emits a deterministic YAML-style manifest. Map keys are sorted so the
// output is stable regardless of insertion order.
func (d Deployment) Render() string {
	var sb strings.Builder
	sb.WriteString("apiVersion: apps/v1\n")
	sb.WriteString("kind: Deployment\n")
	sb.WriteString("metadata:\n")
	sb.WriteString("  name: " + d.name + "\n")
	sb.WriteString("  namespace: " + d.namespace + "\n")
	if len(d.labels) > 0 {
		sb.WriteString("  labels:\n")
		for _, k := range sortedKeys(d.labels) {
			sb.WriteString("    " + k + ": " + d.labels[k] + "\n")
		}
	}
	sb.WriteString("spec:\n")
	sb.WriteString("  replicas: " + strconv.Itoa(d.replicas) + "\n")
	sb.WriteString("  template:\n")
	sb.WriteString("    spec:\n")
	sb.WriteString("      containers:\n")
	sb.WriteString("        - name: " + d.container + "\n")
	sb.WriteString("          image: " + d.image + "\n")
	if len(d.ports) > 0 {
		sb.WriteString("          ports:\n")
		for _, p := range d.ports {
			sb.WriteString("            - containerPort: " + strconv.Itoa(p) + "\n")
		}
	}
	if len(d.env) > 0 {
		sb.WriteString("          env:\n")
		for _, k := range sortedKeys(d.env) {
			sb.WriteString("            - name: " + k + "\n")
			sb.WriteString("              value: " + d.env[k] + "\n")
		}
	}
	return sb.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
```

The container name defaults to the deployment name when `Container` is never called, which mirrors a common convention and keeps the simple case short. The `append([]int(nil), b.ports...)` idiom is the slice equivalent of `copyMap`: it allocates a fresh backing array and copies the elements, so the product's `ports` cannot be grown or overwritten through the builder.

### The runnable demo

The demo builds a complete deployment — name, namespace, three replicas, two labels, an image, two ports, and two env vars — and prints its rendered manifest. Then it shows two failures: a spec missing both required fields, and a chain whose several independent faults aggregate into one error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/deployment-builder"
)

func main() {
	dep, err := deployment.New().
		Name("checkout").
		Namespace("payments").
		Replicas(3).
		Label("app", "checkout").
		Label("tier", "backend").
		Image("registry.example.com/checkout:1.4.2").
		Container("checkout").
		Port(8080).
		Port(9090).
		Env("LOG_LEVEL", "info").
		Env("REGION", "eu-west-1").
		Build()
	if err != nil {
		log.Fatalf("build: %v", err)
	}
	fmt.Print(dep.Render())

	fmt.Println("--- error cases ---")

	// Required fields omitted: no name, no image.
	if _, err := deployment.New().Replicas(2).Build(); err != nil {
		fmt.Printf("missing required: %v\n", err)
	}

	// Several independent faults aggregate into one error.
	if _, err := deployment.New().
		Name("api").
		Image("api:1").
		Replicas(-1).
		Port(70000).
		Label("", "x").
		Build(); err != nil {
		fmt.Printf("aggregated: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
  namespace: payments
  labels:
    app: checkout
    tier: backend
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: checkout
          image: registry.example.com/checkout:1.4.2
          ports:
            - containerPort: 8080
            - containerPort: 9090
          env:
            - name: LOG_LEVEL
              value: info
            - name: REGION
              value: eu-west-1
--- error cases ---
missing required: deployment: metadata.name is required
spec.template.container.image is required
aggregated: deployment: spec.replicas must not be negative: -1
containerPort must be between 1 and 65535: 70000
label key and value must not be empty: ""="x"
```

The labels render alphabetically (`app` before `tier`) because `Render` sorts map keys, while the two ports keep their insertion order. The missing-required case prints both `ErrNoName` and `ErrNoImage` as the two lines of a joined error, and the aggregated case shows the three setter-level faults reported together.

### Tests

The suite drives the builder end to end. `TestBuild_FullSpecRendersManifest` pins the exact rendered manifest for a fully-populated spec, so a regression in field order, indentation, or key sorting fails loudly. `TestBuild_DefaultsNamespaceAndReplicas` confirms the `New` defaults, and `TestBuild_ContainerNameDefaultsToDeploymentName` pins the container-name fallback. One test per required field asserts its sentinel, and one per validating setter does the same; the aggregation test confirms five independent faults stay individually reachable. `TestBuild_ProductIsImmutable` is the decisive one: it builds, mutates the builder afterward, and asserts the earlier product did not change — the guarantee the deep copy exists to provide. The reuse test confirms a failed `Build` does not stick to the builder.

Create `deployment_test.go`:

```go
package deployment

import (
	"errors"
	"strings"
	"testing"
)

func TestBuild_FullSpecRendersManifest(t *testing.T) {
	t.Parallel()

	dep, err := New().
		Name("checkout").
		Namespace("payments").
		Replicas(3).
		Label("app", "checkout").
		Label("tier", "backend").
		Image("registry.example.com/checkout:1.4.2").
		Port(8080).
		Env("LOG_LEVEL", "info").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if dep.Name() != "checkout" || dep.Namespace() != "payments" || dep.Replicas() != 3 {
		t.Errorf("metadata = %s/%s replicas=%d", dep.Namespace(), dep.Name(), dep.Replicas())
	}
	want := strings.Join([]string{
		"apiVersion: apps/v1",
		"kind: Deployment",
		"metadata:",
		"  name: checkout",
		"  namespace: payments",
		"  labels:",
		"    app: checkout",
		"    tier: backend",
		"spec:",
		"  replicas: 3",
		"  template:",
		"    spec:",
		"      containers:",
		"        - name: checkout",
		"          image: registry.example.com/checkout:1.4.2",
		"          ports:",
		"            - containerPort: 8080",
		"          env:",
		"            - name: LOG_LEVEL",
		"              value: info",
		"",
	}, "\n")
	if got := dep.Render(); got != want {
		t.Errorf("Render() =\n%q\nwant\n%q", got, want)
	}
}

func TestBuild_DefaultsNamespaceAndReplicas(t *testing.T) {
	t.Parallel()

	dep, err := New().Name("web").Image("nginx:1.27").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if dep.Namespace() != "default" {
		t.Errorf("namespace = %q, want default", dep.Namespace())
	}
	if dep.Replicas() != 1 {
		t.Errorf("replicas = %d, want 1", dep.Replicas())
	}
}

func TestBuild_ContainerNameDefaultsToDeploymentName(t *testing.T) {
	t.Parallel()

	dep, err := New().Name("worker").Image("worker:2").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(dep.Render(), "- name: worker") {
		t.Errorf("container name should default to deployment name:\n%s", dep.Render())
	}
}

func TestBuild_RequiresName(t *testing.T) {
	t.Parallel()

	if _, err := New().Image("nginx").Build(); !errors.Is(err, ErrNoName) {
		t.Fatalf("err = %v, want ErrNoName", err)
	}
}

func TestBuild_RequiresImage(t *testing.T) {
	t.Parallel()

	if _, err := New().Name("web").Build(); !errors.Is(err, ErrNoImage) {
		t.Fatalf("err = %v, want ErrNoImage", err)
	}
}

func TestBuild_RejectsNegativeReplicas(t *testing.T) {
	t.Parallel()

	_, err := New().Name("web").Image("nginx").Replicas(-2).Build()
	if !errors.Is(err, ErrBadReplicas) {
		t.Fatalf("err = %v, want ErrBadReplicas", err)
	}
}

func TestBuild_RejectsBadPort(t *testing.T) {
	t.Parallel()

	_, err := New().Name("web").Image("nginx").Port(70000).Build()
	if !errors.Is(err, ErrBadPort) {
		t.Fatalf("err = %v, want ErrBadPort", err)
	}
}

func TestBuild_AggregatesMultipleErrors(t *testing.T) {
	t.Parallel()

	_, err := New().Replicas(-1).Port(0).Label("", "x").Build()
	if err == nil {
		t.Fatal("expected joined error, got nil")
	}
	for _, want := range []error{ErrBadReplicas, ErrBadPort, ErrEmptyLabel, ErrNoName, ErrNoImage} {
		if !errors.Is(err, want) {
			t.Errorf("joined error missing %v: got %v", want, err)
		}
	}
}

func TestBuild_ProductIsImmutable(t *testing.T) {
	t.Parallel()

	b := New().Name("web").Image("nginx").Label("app", "web").Port(80).Env("K", "v1")
	dep, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	before := dep.Render()

	// Mutating the builder after Build must not change the already-built spec,
	// because Build deep-copied every map and slice.
	b.Label("app", "changed").Port(9999).Env("K", "v2")

	if after := dep.Render(); after != before {
		t.Errorf("built deployment changed after later setters:\nbefore=%q\nafter =%q", before, after)
	}
}

func TestBuild_IsReusableAndDoesNotLeakBuildErrors(t *testing.T) {
	t.Parallel()

	b := New().Image("nginx") // no name yet
	if _, err := b.Build(); !errors.Is(err, ErrNoName) {
		t.Fatalf("first Build: want ErrNoName, got %v", err)
	}
	// Setting the missing field and building again must succeed; the build-time
	// ErrNoName must not stick to the builder.
	dep, err := b.Name("web").Build()
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if dep.Name() != "web" {
		t.Errorf("name = %q, want web", dep.Name())
	}
}
```

## Review

The builder is correct when the product is genuinely independent of the builder that made it. The decisive check is `TestBuild_ProductIsImmutable`: build a spec, mutate the builder, and confirm the earlier product is unchanged. That holds only because `Build` deep-copies the labels map, the env map, and the ports slice; replace either `copyMap` or the `append([]int(nil), ...)` with a shallow assignment and the test fails, because the returned `Deployment` would alias the builder's backing storage. Confirm too that required-field enforcement lives in `Build`, not in the setters, so the fluent chain stays order-free, and that `Render` sorts its map keys so the manifest is deterministic.

Common mistakes for this builder. The first is shallow-copying the product — assigning `b.labels` straight into the returned struct — which silently shares a map and breaks immutability the moment the builder is touched again; copy the backing data. The second is requiring name and image up front in a constructor, which forces an awkward call order and loses the single-aggregated-error behavior; check completeness once at `Build`. The third is rendering maps in iteration order, which makes the output nondeterministic and untestable and produces noisy diffs; sort the keys. Run `go test -race -count=1 ./...` to confirm the full render, the defaults, the required-field enforcement, and above all the immutability guarantee; the builder is not safe to share across goroutines, but the `Deployment` it produces is.

## Resources

- [Kubernetes: Deployments](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/) — the spec this builder models: metadata, replicas, selector, and the pod template with its containers.
- [Go Slices: usage and internals](https://go.dev/blog/slices-intro) — why copying a slice header shares the backing array, which is why Build copies the data, not just the header.
- [Effective Go: Constructors and composite literals](https://go.dev/doc/effective_go#composite_literals) — when a constructor with defaults earns its place over a bare struct literal.
- [Builder in Go (Refactoring Guru)](https://refactoring.guru/design-patterns/builder/go/example) — the Builder pattern with a worked Go example.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-query-report-builder.md](04-query-report-builder.md) | Next: [Strategy Pattern](../03-strategy-pattern-via-interfaces/00-concepts.md)
