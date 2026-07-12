# Exercise 1: Render a Helm Chart Offline and Gate Values with values.schema.json

This is Helm-as-a-library: a Go package that loads a chart from disk, composes the
render context from chart defaults plus release metadata, renders every template to
Kubernetes manifests entirely offline, and rejects invalid overrides against the
chart's JSON schema. It is the same code path the `helm` CLI uses under the hood,
but embeddable in a CI check or an operator.

## What you'll build

```text
chartrender/                 independent module: example.com/chartrender
  go.mod                     requires helm.sh/helm/v3
  chartrender.go             LoadChart; Renderer{Strict}; Render; ValidateValues; ErrLoad/ErrRender/ErrInvalidValues
  cmd/
    demo/
      main.go                writes a tiny chart to a temp dir, renders it, prints derived facts
  chartrender_test.go        writeChart helper; render/strict/schema table tests; Example
```

Files: `chartrender.go`, `cmd/demo/main.go`, `chartrender_test.go`.
Implement: `LoadChart(dir)`, a `Renderer{Strict bool}` whose `Render` composes values with `chartutil.ToRenderValues` and renders with `engine.Engine`, and `ValidateValues` gating overrides against `values.schema.json`; sentinel errors `ErrLoad`, `ErrRender`, `ErrInvalidValues` wrapped with `%w`.
Test: a chart written to `t.TempDir()`; that a valid override is substituted into the rendered manifest; that strict rendering rejects a reference to an undefined value; that schema validation fails on a wrong type and passes on a good one.
Verify: `go test -race ./...` (needs the helm module fetched; `GOFLAGS=-mod=mod`).

Set up the module:

```bash
go get helm.sh/helm/v3@latest
```

### Why the render context is not just a values map

A Helm template reads far more than `.Values`. It reads `.Release.Name`,
`.Release.Namespace`, `.Chart.Version`, and `.Capabilities.KubeVersion`. Those keys
do not exist in your override map; they are assembled by
`chartutil.ToRenderValues(chrt, overrides, options, caps)`, which merges the
overrides over the chart's own `values.yaml` defaults and builds the full
top-level context the engine expects. The most common way to misuse the SDK is to
skip this step and hand `engine.Render` a bare `chartutil.Values{"replicaCount":
3}`; a template that references `.Release.Name` then renders empty. `Render` here
always goes through `ToRenderValues`, and passes `chartutil.DefaultCapabilities` so
`.Capabilities` is answered from a built-in Kubernetes version with no cluster.

### Why Strict matters

`engine.Engine{Strict: true}` fails the render when a template references a value
that was never provided. Without it, a misspelled `.Values.replicaCont` renders as
an empty string and produces a manifest that is syntactically valid and
semantically wrong — it will ship to the cluster and quietly do the wrong thing. In
a CI gate you always want `Strict`, so a typo is a build error, not a silent
production change. The test proves this: the same chart renders fine when the value
exists and fails under `Strict` when the template reaches for a key that does not.

### Schema validation is a separate guardrail

`ValidateValues` is not the same check as strict rendering. Strict rendering
catches references to missing keys; schema validation catches *wrong-typed* values
that are present. A `values.schema.json` declares that `replicaCount` is an
integer; if a PR sets it to the string `"three"`, `chartutil.ValidateAgainstSchema`
fails before anything renders. Note the order inside `ValidateValues`: coalesce the
overrides with the chart defaults first (so the schema sees the effective values),
then validate. Both checks wrap the sentinel `ErrInvalidValues` with `%w` so a
caller can classify the failure with `errors.Is`.

Create `chartrender.go`:

```go
package chartrender

import (
	"errors"
	"fmt"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

// Sentinel errors let a CI gate classify a failure: a chart that will not load, a
// values set that violates the schema, or a template that will not render.
var (
	ErrLoad          = errors.New("load chart")
	ErrInvalidValues = errors.New("invalid values")
	ErrRender        = errors.New("render chart")
)

// ReleaseInfo is the release metadata a template can read through .Release. It is
// a plain struct so callers need not import chartutil to describe a release.
type ReleaseInfo struct {
	Name      string
	Namespace string
	Revision  int
	IsInstall bool
	IsUpgrade bool
}

// Renderer renders a chart to manifests. Strict makes a reference to an undefined
// value a hard error instead of an empty substitution; use it in CI.
type Renderer struct {
	Strict bool
}

// LoadChart loads a chart directory from disk. No cluster or network is involved.
func LoadChart(dir string) (*chart.Chart, error) {
	chrt, err := loader.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrLoad, dir, err)
	}
	return chrt, nil
}

// ValidateValues coalesces overrides with the chart defaults and validates the
// result against the chart's values.schema.json. It returns nil when the chart
// has no schema and the values are well-formed.
func ValidateValues(chrt *chart.Chart, overrides map[string]any) error {
	coalesced, err := chartutil.CoalesceValues(chrt, overrides)
	if err != nil {
		return fmt.Errorf("%w: coalesce: %w", ErrInvalidValues, err)
	}
	if err := chartutil.ValidateAgainstSchema(chrt, coalesced); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidValues, err)
	}
	return nil
}

// Render composes the full template context from the chart defaults, the
// overrides, and rel, then renders every template. The returned map is keyed by
// template path (for example "web/templates/deployment.yaml").
func (r Renderer) Render(chrt *chart.Chart, rel ReleaseInfo, overrides map[string]any) (map[string]string, error) {
	relOpts := chartutil.ReleaseOptions{
		Name:      rel.Name,
		Namespace: rel.Namespace,
		Revision:  rel.Revision,
		IsInstall: rel.IsInstall,
		IsUpgrade: rel.IsUpgrade,
	}
	vals, err := chartutil.ToRenderValues(chrt, overrides, relOpts, chartutil.DefaultCapabilities)
	if err != nil {
		return nil, fmt.Errorf("%w: compose values: %w", ErrRender, err)
	}
	out, err := engine.Engine{Strict: r.Strict}.Render(chrt, vals)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRender, err)
	}
	return out, nil
}
```

### The demo

The demo writes a two-file chart to a temp directory, loads it, renders it with a
release name and an override, and prints derived facts rather than the raw manifest
so the output is stable regardless of incidental whitespace. It needs the helm
module available but no cluster and no network.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"example.com/chartrender"
)

func writeChart(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: web\nversion: 0.1.0\n",
		"values.yaml": "replicaCount: 1\nimage:\n  repository: nginx\n  tag: \"1.25\"\n",
		"templates/deployment.yaml": "apiVersion: apps/v1\n" +
			"kind: Deployment\n" +
			"metadata:\n  name: {{ .Release.Name }}-web\n" +
			"spec:\n  replicas: {{ .Values.replicaCount }}\n" +
			"  template:\n    spec:\n      containers:\n        - name: web\n" +
			"          image: {{ .Values.image.repository }}:{{ .Values.image.tag }}\n",
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

	chrt, err := chartrender.LoadChart(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	overrides := map[string]any{
		"replicaCount": 3,
		"image":        map[string]any{"tag": "1.27"},
	}
	rel := chartrender.ReleaseInfo{Name: "prod", Namespace: "web", IsInstall: true}

	out, err := chartrender.Renderer{Strict: true}.Render(chrt, rel, overrides)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	manifest := out["web/templates/deployment.yaml"]
	fmt.Println("manifests rendered:", len(out))
	fmt.Println("release name applied:", strings.Contains(manifest, "name: prod-web"))
	fmt.Println("replica override applied:", strings.Contains(manifest, "replicas: 3"))
	fmt.Println("image override applied:", strings.Contains(manifest, "nginx:1.27"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
manifests rendered: 1
release name applied: true
replica override applied: true
image override applied: true
```

### The tests

`writeChart` writes a chart with a caller-supplied deployment template so one
helper serves both the happy-path render and the strict-failure case. `TestRender`
checks that an override lands in the manifest and that the release name from
`ReleaseInfo` is substituted — proof that the context was composed, not passed raw.
`TestStrict` renders a template that references `.Values.doesNotExist`: with
`Strict: false` it renders (the reference becomes empty), and with `Strict: true`
it returns an error wrapping `ErrRender`. `TestValidateValues` is table-driven: a
string in an integer field wraps `ErrInvalidValues`, and a well-typed override
returns nil.

Create `chartrender_test.go`:

```go
package chartrender

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	goodTemplate = "apiVersion: apps/v1\n" +
		"kind: Deployment\n" +
		"metadata:\n  name: {{ .Release.Name }}-web\n" +
		"spec:\n  replicas: {{ .Values.replicaCount }}\n" +
		"  template:\n    spec:\n      containers:\n        - name: web\n" +
		"          image: {{ .Values.image.repository }}:{{ .Values.image.tag }}\n"

	missingRefTemplate = "apiVersion: v1\n" +
		"kind: ConfigMap\n" +
		"metadata:\n  name: {{ .Release.Name }}-cm\n" +
		"data:\n  value: {{ .Values.doesNotExist }}\n"

	valuesYAML = "replicaCount: 1\nimage:\n  repository: nginx\n  tag: \"1.25\"\n"

	schemaJSON = `{
  "$schema": "https://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": {
    "replicaCount": { "type": "integer", "minimum": 1 },
    "image": {
      "type": "object",
      "properties": {
        "repository": { "type": "string" },
        "tag": { "type": "string" }
      }
    }
  }
}`
)

func writeChart(t *testing.T, template string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Chart.yaml":                "apiVersion: v2\nname: web\nversion: 0.1.0\n",
		"values.yaml":               valuesYAML,
		"values.schema.json":        schemaJSON,
		"templates/deployment.yaml": template,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRender(t *testing.T) {
	t.Parallel()
	chrt, err := LoadChart(writeChart(t, goodTemplate))
	if err != nil {
		t.Fatalf("LoadChart: %v", err)
	}

	overrides := map[string]any{
		"replicaCount": 3,
		"image":        map[string]any{"tag": "1.27"},
	}
	rel := ReleaseInfo{Name: "prod", Namespace: "web", IsInstall: true}

	out, err := Renderer{Strict: true}.Render(chrt, rel, overrides)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	manifest := out["web/templates/deployment.yaml"]
	for _, want := range []string{"name: prod-web", "replicas: 3", "nginx:1.27"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest missing %q; got:\n%s", want, manifest)
		}
	}
}

func TestStrict(t *testing.T) {
	t.Parallel()
	chrt, err := LoadChart(writeChart(t, missingRefTemplate))
	if err != nil {
		t.Fatalf("LoadChart: %v", err)
	}
	rel := ReleaseInfo{Name: "prod", IsInstall: true}

	if _, err := (Renderer{Strict: false}).Render(chrt, rel, nil); err != nil {
		t.Fatalf("non-strict render should tolerate a missing value, got %v", err)
	}

	_, err = Renderer{Strict: true}.Render(chrt, rel, nil)
	if !errors.Is(err, ErrRender) {
		t.Fatalf("strict render err = %v; want ErrRender", err)
	}
}

func TestValidateValues(t *testing.T) {
	t.Parallel()
	chrt, err := LoadChart(writeChart(t, goodTemplate))
	if err != nil {
		t.Fatalf("LoadChart: %v", err)
	}

	tests := []struct {
		name      string
		overrides map[string]any
		wantErr   bool
	}{
		{"valid integer", map[string]any{"replicaCount": 5}, false},
		{"wrong type", map[string]any{"replicaCount": "three"}, true},
		{"no overrides", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateValues(chrt, tc.overrides)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidValues) {
					t.Fatalf("err = %v; want ErrInvalidValues", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v; want nil", err)
			}
		})
	}
}

func ExampleRenderer_Render() {
	dir, _ := os.MkdirTemp("", "chart")
	defer os.RemoveAll(dir)
	_ = os.MkdirAll(filepath.Join(dir, "templates"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: web\nversion: 0.1.0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(valuesYAML), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "templates/deployment.yaml"), []byte(goodTemplate), 0o644)

	chrt, _ := LoadChart(dir)
	out, _ := Renderer{Strict: true}.Render(chrt, ReleaseInfo{Name: "prod"}, map[string]any{"replicaCount": 2})
	fmt.Println(strings.Contains(out["web/templates/deployment.yaml"], "replicas: 2"))
	// Output: true
}
```

## Review

The renderer is correct when a value present in the overrides appears in the
manifest and a value present only in the release metadata (`.Release.Name`) also
appears — that pair proves the context was composed by `ToRenderValues` rather than
passed raw. Confirm strictness by reading `TestStrict`: the identical chart renders
under `Strict: false` and fails under `Strict: true`, which is only possible if the
engine flag is actually threaded through. Schema validation is confirmed by the
table in `TestValidateValues`, where a string in an integer field is rejected and a
well-typed value passes.

The mistakes to avoid map directly to the concepts. Do not call `engine.Render`
with a bare values map; a template reading `.Release.Name` will render empty and no
test will catch it until the manifest is wrong on a cluster. Do not skip
`Strict` in a gate; a typo becomes an empty string that ships. Do not confuse the
two checks: strict rendering catches missing keys, schema validation catches
wrong-typed present keys, and you want both. Because this lesson depends on the
helm module, resolve it with `GOFLAGS=-mod=mod` where the module cache is cold.

## Resources

- [Helm Go SDK — Introduction](https://helm.sh/docs/v3/sdk/gosdk/) — how the SDK maps to the CLI and the v3 API-stability commitment.
- [`helm.sh/helm/v3/pkg/chartutil`](https://pkg.go.dev/helm.sh/helm/v3/pkg/chartutil) — `ToRenderValues`, `CoalesceValues`, `ValidateAgainstSchema`, `ReleaseOptions`, `DefaultCapabilities`.
- [`helm.sh/helm/v3/pkg/engine`](https://pkg.go.dev/helm.sh/helm/v3/pkg/engine) — `Render` and the `Engine` struct with `Strict` and `LintMode`.
- [`helm.sh/helm/v3/pkg/chart/loader`](https://pkg.go.dev/helm.sh/helm/v3/pkg/chart/loader) — `Load`, `LoadDir`, `LoadFiles`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-rendered-manifests-drift.md](02-rendered-manifests-drift.md)
