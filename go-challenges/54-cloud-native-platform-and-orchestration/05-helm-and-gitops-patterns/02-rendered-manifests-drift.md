# Exercise 2: The Rendered-Manifests GitOps Pipeline with Drift Detection

This is the reconciliation core of GitOps, decoupled from any cluster: render a
chart to individual, deterministically-named Kubernetes manifests, write them as
the desired-state tree, and diff a candidate tree against a recorded live snapshot
to detect drift. Determinism is the whole point — the same inputs must produce
byte-identical output, or a GitOps controller would report perpetual drift.

## What you'll build

```text
renderpipe/                  independent module: example.com/renderpipe
  go.mod                     requires helm.sh/helm/v3, sigs.k8s.io/yaml, github.com/google/go-cmp
  renderpipe.go              LoadChart; Render; SplitToManifests; BuildDesiredState; WriteTree; Diff; DriftReport
  cmd/
    demo/
      main.go                writes a chart, builds a tree, mutates a value, prints the drift report
  renderpipe_test.go         determinism, stable filenames, and precise-drift table tests; Example
```

Files: `renderpipe.go`, `cmd/demo/main.go`, `renderpipe_test.go`.
Implement: `Render` (strict), `SplitToManifests` (split a render into individual manifests with stable filenames derived from kind and name), `BuildDesiredState` (filename to content), `WriteTree`, and `Diff` returning a `DriftReport{Added, Removed, Changed}`.
Test: two identical builds produce equal trees; filenames are sorted and independent of Go map order; mutating one value marks exactly that manifest changed with no false positives; `go-cmp` for readable `DriftReport` diffs.
Verify: `go test -race ./...` (`GOFLAGS=-mod=mod` to fetch the modules).

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/05-helm-and-gitops-patterns/02-rendered-manifests-drift/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/05-helm-and-gitops-patterns/02-rendered-manifests-drift
go get helm.sh/helm/v3@latest sigs.k8s.io/yaml@latest github.com/google/go-cmp@latest
```

### From a render to a desired-state tree

`engine.Render` returns a `map[string]string` keyed by template *path*, and each
value may contain several YAML documents separated by `---`. That shape is wrong
for GitOps: you want one file per Kubernetes object, named by the object's stable
identity, so a reviewer reading a git diff sees exactly which object changed.
`SplitToManifests` does that conversion. It walks the render in *sorted* path order
(never map order), splits each file into documents with
`releaseutil.SplitManifests`, parses each document's head — `apiVersion`, `kind`,
and `metadata.name` — with `sigs.k8s.io/yaml`, and builds a filename from the kind
and name. The filename is derived from identity, not from iteration order, which is
what makes the tree reproducible.

The reason determinism is not optional bears repeating: a GitOps controller diffs
the desired state in git against the live cluster on every reconcile. If your
render can emit the same objects under different filenames or in a different order
on two runs with identical inputs, the controller sees a change that is not really
a change and never settles. Every place a Go map is iterated here is followed by a
sort, and no timestamp is ever embedded.

### Drift as a set diff

Once desired state is a `map[filename]content`, drift detection is a set
comparison. `Diff(live, desired)` reports three disjoint categories: filenames in
desired but not live (`Added`), filenames in live but not desired (`Removed`), and
filenames in both whose content differs (`Changed`). Each list is sorted so the
report itself is deterministic and diffable. The test's key assertion is *precision*:
mutating a value that only the Deployment reads must mark the Deployment changed and
leave the Service untouched — no false positives, because a controller that
mis-reports drift will fight the cluster forever.

Create `renderpipe.go`:

```go
package renderpipe

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/releaseutil"
	"sigs.k8s.io/yaml"
)

// Manifest is one Kubernetes object extracted from a render, with a filename
// derived from its stable identity (kind and name).
type Manifest struct {
	APIVersion string
	Kind       string
	Name       string
	Filename   string
	Content    string
}

// DriftReport is the disjoint result of diffing a desired tree against a live one.
type DriftReport struct {
	Added   []string
	Removed []string
	Changed []string
}

// HasDrift reports whether the desired and live states diverge at all.
func (r DriftReport) HasDrift() bool {
	return len(r.Added)+len(r.Removed)+len(r.Changed) > 0
}

// head is the minimal slice of a manifest needed for stable naming and grouping.
type head struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// LoadChart loads a chart directory from disk.
func LoadChart(dir string) (*chart.Chart, error) {
	return loader.LoadDir(dir)
}

// Render composes the template context and renders every template strictly, so a
// reference to an undefined value fails instead of silently emitting empty text.
func Render(chrt *chart.Chart, releaseName string, overrides map[string]any) (map[string]string, error) {
	relOpts := chartutil.ReleaseOptions{Name: releaseName, IsInstall: true}
	vals, err := chartutil.ToRenderValues(chrt, overrides, relOpts, chartutil.DefaultCapabilities)
	if err != nil {
		return nil, fmt.Errorf("compose values: %w", err)
	}
	out, err := engine.Engine{Strict: true}.Render(chrt, vals)
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return out, nil
}

func filename(kind, name string) string {
	return fmt.Sprintf("%s-%s.yaml", strings.ToLower(kind), name)
}

// SplitToManifests turns a render (keyed by template path, possibly multi-document)
// into individual manifests sorted by filename. Ordering is derived from identity,
// never from Go map iteration order, so the result is reproducible.
func SplitToManifests(rendered map[string]string) ([]Manifest, error) {
	paths := make([]string, 0, len(rendered))
	for p := range rendered {
		paths = append(paths, p)
	}
	slices.Sort(paths)

	var manifests []Manifest
	for _, p := range paths {
		if strings.HasSuffix(p, "NOTES.txt") {
			continue
		}
		for _, doc := range releaseutil.SplitManifests(rendered[p]) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			var h head
			if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
				return nil, fmt.Errorf("parse manifest head in %s: %w", p, err)
			}
			if h.Kind == "" || h.Metadata.Name == "" {
				continue
			}
			manifests = append(manifests, Manifest{
				APIVersion: h.APIVersion,
				Kind:       h.Kind,
				Name:       h.Metadata.Name,
				Filename:   filename(h.Kind, h.Metadata.Name),
				Content:    strings.TrimSpace(doc) + "\n",
			})
		}
	}
	slices.SortFunc(manifests, func(a, b Manifest) int {
		return cmp.Compare(a.Filename, b.Filename)
	})
	return manifests, nil
}

// BuildDesiredState renders the chart and returns the desired-state tree as a map
// of filename to content.
func BuildDesiredState(chrt *chart.Chart, releaseName string, overrides map[string]any) (map[string]string, error) {
	rendered, err := Render(chrt, releaseName, overrides)
	if err != nil {
		return nil, err
	}
	manifests, err := SplitToManifests(rendered)
	if err != nil {
		return nil, err
	}
	tree := make(map[string]string, len(manifests))
	for _, m := range manifests {
		tree[m.Filename] = m.Content
	}
	return tree, nil
}

// WriteTree writes the desired-state tree to dir, one file per manifest, in sorted
// order so the write sequence is deterministic.
func WriteTree(dir string, tree map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(tree))
	for n := range tree {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(tree[n]), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Diff computes the drift of desired against live: files only in desired are
// Added, files only in live are Removed, and files in both with differing content
// are Changed. Each list is sorted for a deterministic, diffable report.
func Diff(live, desired map[string]string) DriftReport {
	var r DriftReport
	for name, want := range desired {
		switch got, ok := live[name]; {
		case !ok:
			r.Added = append(r.Added, name)
		case got != want:
			r.Changed = append(r.Changed, name)
		}
	}
	for name := range live {
		if _, ok := desired[name]; !ok {
			r.Removed = append(r.Removed, name)
		}
	}
	slices.Sort(r.Added)
	slices.Sort(r.Removed)
	slices.Sort(r.Changed)
	return r
}
```

### The demo

The demo renders a chart with a Deployment and a Service, builds the live tree,
then rebuilds with a changed replica count and diffs. It prints the manifest count,
the sorted filenames, and each drift category, all of which are deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"example.com/renderpipe"
)

func writeChart(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: web\nversion: 0.1.0\n",
		"values.yaml": "replicaCount: 2\nimage:\n  repository: nginx\n  tag: \"1.25\"\n",
		"templates/deployment.yaml": "apiVersion: apps/v1\nkind: Deployment\n" +
			"metadata:\n  name: {{ .Release.Name }}-web\n" +
			"spec:\n  replicas: {{ .Values.replicaCount }}\n",
		"templates/service.yaml": "apiVersion: v1\nkind: Service\n" +
			"metadata:\n  name: {{ .Release.Name }}-svc\n" +
			"spec:\n  ports:\n    - port: 80\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "chart")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	if err := writeChart(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	chrt, err := renderpipe.LoadChart(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	live, err := renderpipe.BuildDesiredState(chrt, "prod", map[string]any{"replicaCount": 2})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	desired, err := renderpipe.BuildDesiredState(chrt, "prod", map[string]any{"replicaCount": 5})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	names := make([]string, 0, len(live))
	for n := range live {
		names = append(names, n)
	}
	slices.Sort(names)

	report := renderpipe.Diff(live, desired)
	fmt.Println("manifests:", len(live))
	fmt.Println("files:", names)
	fmt.Println("drift added:", report.Added)
	fmt.Println("drift changed:", report.Changed)
	fmt.Println("drift removed:", report.Removed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
manifests: 2
files: [deployment-prod-web.yaml service-prod-svc.yaml]
drift added: []
drift changed: [deployment-prod-web.yaml]
drift removed: []
```

### The tests

`TestDeterministic` builds the tree twice with identical inputs and asserts the
maps are equal with `maps.Equal` — the reproducibility contract. `TestFilenames`
asserts the tree's keys are exactly the two identity-derived names, proving naming
does not depend on map order. `TestDrift` is the precision check: it changes only
the replica count and asserts, with `go-cmp`, that the report marks exactly the
Deployment changed and nothing else. `TestNoDrift` confirms an unchanged rebuild
reports no drift at all.

Create `renderpipe_test.go`:

```go
package renderpipe

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func writeChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: web\nversion: 0.1.0\n",
		"values.yaml": "replicaCount: 2\nimage:\n  repository: nginx\n  tag: \"1.25\"\n",
		"templates/deployment.yaml": "apiVersion: apps/v1\nkind: Deployment\n" +
			"metadata:\n  name: {{ .Release.Name }}-web\n" +
			"spec:\n  replicas: {{ .Values.replicaCount }}\n",
		"templates/service.yaml": "apiVersion: v1\nkind: Service\n" +
			"metadata:\n  name: {{ .Release.Name }}-svc\n" +
			"spec:\n  ports:\n    - port: 80\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDeterministic(t *testing.T) {
	t.Parallel()
	chrt, err := LoadChart(writeChart(t))
	if err != nil {
		t.Fatalf("LoadChart: %v", err)
	}
	a, err := BuildDesiredState(chrt, "prod", map[string]any{"replicaCount": 2})
	if err != nil {
		t.Fatalf("build a: %v", err)
	}
	b, err := BuildDesiredState(chrt, "prod", map[string]any{"replicaCount": 2})
	if err != nil {
		t.Fatalf("build b: %v", err)
	}
	if !maps.Equal(a, b) {
		t.Fatal("two identical builds produced different trees")
	}
}

func TestFilenames(t *testing.T) {
	t.Parallel()
	chrt, err := LoadChart(writeChart(t))
	if err != nil {
		t.Fatalf("LoadChart: %v", err)
	}
	tree, err := BuildDesiredState(chrt, "prod", map[string]any{"replicaCount": 2})
	if err != nil {
		t.Fatalf("BuildDesiredState: %v", err)
	}
	got := slices.Sorted(maps.Keys(tree))
	want := []string{"deployment-prod-web.yaml", "service-prod-svc.yaml"}
	if !slices.Equal(got, want) {
		t.Fatalf("filenames = %v; want %v", got, want)
	}
}

func TestDrift(t *testing.T) {
	t.Parallel()
	chrt, err := LoadChart(writeChart(t))
	if err != nil {
		t.Fatalf("LoadChart: %v", err)
	}
	live, err := BuildDesiredState(chrt, "prod", map[string]any{"replicaCount": 2})
	if err != nil {
		t.Fatalf("build live: %v", err)
	}
	desired, err := BuildDesiredState(chrt, "prod", map[string]any{"replicaCount": 5})
	if err != nil {
		t.Fatalf("build desired: %v", err)
	}

	got := Diff(live, desired)
	want := DriftReport{Changed: []string{"deployment-prod-web.yaml"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("drift report mismatch (-want +got):\n%s", diff)
	}
	if !got.HasDrift() {
		t.Fatal("HasDrift() = false; want true")
	}
}

func TestNoDrift(t *testing.T) {
	t.Parallel()
	chrt, err := LoadChart(writeChart(t))
	if err != nil {
		t.Fatalf("LoadChart: %v", err)
	}
	tree, err := BuildDesiredState(chrt, "prod", map[string]any{"replicaCount": 2})
	if err != nil {
		t.Fatalf("BuildDesiredState: %v", err)
	}
	if Diff(tree, tree).HasDrift() {
		t.Fatal("diff of a tree against itself reports drift")
	}
}

func ExampleDiff() {
	live := map[string]string{"deployment-prod-web.yaml": "replicas: 2\n"}
	desired := map[string]string{"deployment-prod-web.yaml": "replicas: 5\n"}
	fmt.Println(Diff(live, desired).Changed)
	// Output: [deployment-prod-web.yaml]
}
```

## Review

The pipeline is correct when it is both reproducible and precise. Reproducibility
is proven by `TestDeterministic` and `TestFilenames`: identical inputs yield equal
trees, and filenames come from object identity rather than map order. Precision is
proven by `TestDrift`, where changing a single value marks exactly one manifest
changed and the `go-cmp` diff would show any false positive immediately. A drift of
a tree against itself must be empty, which `TestNoDrift` guards.

The failure mode to internalize is spurious drift. Any Go map iterated without a
following sort, any embedded timestamp, any unstable list order will make the
controller believe the state changed when it did not, and it will churn forever
trying to reconcile a difference that is only noise. `SplitToManifests` walks the
render in sorted path order and sorts the final manifest slice by filename;
`WriteTree` writes in sorted key order; and `Diff` sorts each output list. So the
tree is deterministic regardless of the intermediate `SplitManifests` map iteration
order, because filenames are unique per object and every result is ordered by a
stable key. Fetch the external modules with `GOFLAGS=-mod=mod` when the cache is cold.

## Resources

- [`helm.sh/helm/v3/pkg/releaseutil`](https://pkg.go.dev/helm.sh/helm/v3/pkg/releaseutil) — `SplitManifests` and the manifest head types.
- [`helm.sh/helm/v3/pkg/engine`](https://pkg.go.dev/helm.sh/helm/v3/pkg/engine) — `Render`, keyed by template path.
- [`sigs.k8s.io/yaml`](https://pkg.go.dev/sigs.k8s.io/yaml) — `Unmarshal`, which reads YAML through the JSON struct tags used for `apiVersion`/`kind`/`metadata`.
- [`github.com/google/go-cmp/cmp`](https://pkg.go.dev/github.com/google/go-cmp/cmp) — `cmp.Diff` for readable struct comparisons in tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-render-and-validate-chart.md](01-render-and-validate-chart.md) | Next: [03-git-delivery-idempotent-commit.md](03-git-delivery-idempotent-commit.md)
