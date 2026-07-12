# Exercise 3: Instrumenting the Workload for the KEDA Prometheus Scaler

The low-code alternative to a bespoke external scaler is to publish your demand
signal as a Prometheus metric and let KEDA's built-in `prometheus` trigger scrape
it. No gRPC service to run and operate; the cost is a dependency on a Prometheus
scrape pipeline and on the correctness of your PromQL aggregation. This exercise
instruments the consumer with a backlog gauge, exposes it on `/metrics`, and
authors the matching `ScaledObject`, with tests that check the exposition format
and validate the manifest statically.

This module depends on `github.com/prometheus/client_golang` and
`gopkg.in/yaml.v3`, neither vendored here, so offline it cannot build — a bar-mode
lesson judged on gofmt-clean, API-accurate, correctly shaped code.

## What you'll build

```text
backlog-exporter/                independent module: example.com/backlog-exporter
  go.mod                         go 1.24; requires prometheus/client_golang + yaml.v3
  metrics.go                     Metrics: NewMetrics, SetBacklog, Handler (promhttp)
  scaledobject.go                ScaledObjectYAML + ValidateScaledObject (parse/validate)
  cmd/
    demo/
      main.go                    scrapes its own /metrics through httptest, prints the gauge
    server/
      main.go                    //go:build serverbin — binds :8080 /metrics
  metrics_test.go                testutil.ToFloat64 / CollectAndCompare; scrape test; YAML validation
```

- Files: `metrics.go`, `scaledobject.go`, `cmd/demo/main.go`, `cmd/server/main.go`, `metrics_test.go`.
- Implement: a `Metrics` type wrapping a `consumer_backlog_messages` gauge (labelled by queue) registered on a private `*prometheus.Registry`, a `Handler` serving `promhttp` exposition, and a `ValidateScaledObject` that parses the manifest and checks the prometheus-trigger invariants.
- Test: assert the gauge value with `testutil.ToFloat64`, compare the exposition against a golden snippet with `testutil.CollectAndCompare`, scrape `/metrics` through `httptest`, and validate the `ScaledObject` YAML (accepts the good manifest, rejects an unquoted threshold and a missing `maxReplicaCount`).
- Verify: `go test -count=1 -race ./...` where the prometheus and yaml modules are available; offline this is validated by gofmt and review.

Set up the module:

```bash
go mod edit -go=1.24
go get github.com/prometheus/client_golang@v1.20.5 gopkg.in/yaml.v3@v3.0.1
```

### Why a private registry, and what the gauge must mean

The instinct is to register on `prometheus.DefaultRegisterer` and be done. Resist
it: a package-global registry makes tests order-dependent (a second `NewMetrics`
panics with a duplicate-registration error) and couples unrelated code through
shared global state. `promauto.With(reg)` builds and registers a collector on an
*explicit* registry you own, so each test gets an isolated `prometheus.
NewRegistry()` and the production path gets one registry per process. That single
decision is what makes the exposition tests below deterministic.

The gauge itself must publish the *aggregate* backlog per queue — the same
quantity the external scaler returned from `GetMetrics` — because KEDA's
prometheus trigger runs a PromQL query whose scalar result the HPA divides by the
`threshold`. The query must reduce to exactly one series; `sum(consumer_backlog_
messages{queue="orders"})` collapses across pods to a single scalar. A query that
returns an empty vector makes KEDA error (or, with `ignoreNullValues: true`,
silently treat it as no data), and a query that returns multiple series is an
ambiguous scale decision. The metric is a *gauge*, not a counter: backlog goes up
and down, so `Set` to the current depth on each poll rather than incrementing.

Create `metrics.go`:

```go
package backlogexporter

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics publishes the demand signal KEDA's prometheus scaler scrapes: the
// per-queue consumer backlog, as a gauge.
type Metrics struct {
	backlog *prometheus.GaugeVec
}

// NewMetrics registers the backlog gauge on reg. Passing an explicit registry
// (not the global default) keeps tests isolated and avoids duplicate-registration
// panics when more than one instance exists.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	return &Metrics{
		backlog: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Name: "consumer_backlog_messages",
			Help: "Number of unprocessed messages waiting in the queue.",
		}, []string{"queue"}),
	}
}

// SetBacklog publishes the current aggregate backlog for a queue. It is a gauge:
// set the absolute depth each poll, since backlog rises and falls.
func (m *Metrics) SetBacklog(queue string, depth float64) {
	m.backlog.WithLabelValues(queue).Set(depth)
}

// Handler serves the Prometheus exposition for reg (mount it at /metrics).
func Handler(reg prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
```

### The ScaledObject, and why threshold must be a quoted string

The manifest wires the built-in `prometheus` trigger to the gauge. The fields that
matter: `serverAddress` (the Prometheus query endpoint), `query` (the PromQL that
must reduce to one scalar), `threshold` (the per-replica target the HPA divides
by), and `activationThreshold` (the scale-from-zero gate). Every value in a
trigger's `metadata` block is a *string* in KEDA's schema — `threshold: "50"`,
not `threshold: 50`. That is not cosmetic: an unquoted `50` is a YAML integer, and
KEDA rejects a non-string metadata value. To catch it before it reaches a cluster,
the validator decodes each metadata value as a `yaml.Node` and checks its resolved
tag: a quoted `"50"` carries the `!!str` tag, while an unquoted `50` carries
`!!int`. The validator also insists on `maxReplicaCount`: omitting it lets a burst
against a small target detonate into an unbounded, expensive replica count.

Create `scaledobject.go`:

```go
package backlogexporter

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ScaledObjectYAML wires the built-in prometheus scaler to the
// consumer_backlog_messages gauge. threshold is the per-replica target;
// activationThreshold gates scale-from-zero. All trigger metadata values are
// quoted strings, as KEDA's schema requires.
const ScaledObjectYAML = `apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: orders-consumer
  namespace: default
spec:
  scaleTargetRef:
    name: orders-consumer
  minReplicaCount: 0
  maxReplicaCount: 20
  cooldownPeriod: 300
  pollingInterval: 15
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus.monitoring.svc:9090
        query: sum(consumer_backlog_messages{queue="orders"})
        threshold: "50"
        activationThreshold: "5"
        ignoreNullValues: "false"
`

type scaledObject struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		ScaleTargetRef struct {
			Name string `yaml:"name"`
		} `yaml:"scaleTargetRef"`
		MaxReplicaCount *int `yaml:"maxReplicaCount"`
		Triggers        []struct {
			Type     string               `yaml:"type"`
			Metadata map[string]yaml.Node `yaml:"metadata"`
		} `yaml:"triggers"`
	} `yaml:"spec"`
}

// ValidateScaledObject parses a ScaledObject manifest and checks the invariants
// that make the prometheus trigger scale correctly. Each metadata value is a
// yaml.Node so the validator can reject a non-string value (an unquoted number
// resolves to the !!int tag rather than !!str).
func ValidateScaledObject(data []byte) error {
	var so scaledObject
	if err := yaml.Unmarshal(data, &so); err != nil {
		return fmt.Errorf("parse ScaledObject: %w", err)
	}
	if so.APIVersion != "keda.sh/v1alpha1" {
		return fmt.Errorf("apiVersion = %q, want keda.sh/v1alpha1", so.APIVersion)
	}
	if so.Kind != "ScaledObject" {
		return fmt.Errorf("kind = %q, want ScaledObject", so.Kind)
	}
	if so.Spec.ScaleTargetRef.Name == "" {
		return fmt.Errorf("spec.scaleTargetRef.name is required")
	}
	if so.Spec.MaxReplicaCount == nil {
		return fmt.Errorf("spec.maxReplicaCount is required to bound the blast radius")
	}
	if len(so.Spec.Triggers) == 0 {
		return fmt.Errorf("at least one trigger is required")
	}
	for i, tr := range so.Spec.Triggers {
		if tr.Type != "prometheus" {
			continue
		}
		for _, key := range []string{"serverAddress", "query", "threshold"} {
			node, ok := tr.Metadata[key]
			if !ok || node.Value == "" {
				return fmt.Errorf("trigger[%d]: prometheus metadata %q is required", i, key)
			}
			if node.Tag != "!!str" {
				return fmt.Errorf("trigger[%d]: metadata %q must be a quoted string, got tag %s", i, key, node.Tag)
			}
		}
	}
	return nil
}
```

### The runnable demo

The demo publishes two queues' backlogs, serves the exposition on an in-process
`httptest` server, scrapes it back, and prints the metric lines — exactly what
Prometheus would collect. Sorting the lines makes the output deterministic
regardless of gauge iteration order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"

	exporter "example.com/backlog-exporter"
	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	reg := prometheus.NewRegistry()
	m := exporter.NewMetrics(reg)
	m.SetBacklog("orders", 42)
	m.SetBacklog("shipments", 7)

	srv := httptest.NewServer(exporter.Handler(reg))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "consumer_backlog_messages{") {
			lines = append(lines, line)
		}
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
}
```

Run it (with the prometheus module present):

```bash
go run ./cmd/demo
```

Expected output:

```
consumer_backlog_messages{queue="orders"} 42
consumer_backlog_messages{queue="shipments"} 7
```

### The real server, behind a build tag

The production server binds a port and updates the gauge from the broker on a
loop. It lives behind a build tag so offline `go build ./...` never binds a port.

Create `cmd/server/main.go`:

```go
//go:build serverbin

package main

import (
	"log"
	"net/http"
	"time"

	exporter "example.com/backlog-exporter"
	"github.com/prometheus/client_golang/prometheus"
)

// queueDepth queries the broker for the current backlog. Stubbed here.
func queueDepth(queue string) float64 { return 0 }

func main() {
	reg := prometheus.NewRegistry()
	m := exporter.NewMetrics(reg)

	go func() {
		for range time.Tick(5 * time.Second) {
			m.SetBacklog("orders", queueDepth("orders"))
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", exporter.Handler(reg))
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("serving /metrics on %s", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}
```

### Tests

The tests use a private registry so they never collide. `TestBacklogGauge` reads
the gauge back with `testutil.ToFloat64`. `TestExposition` compares the full
exposition against a golden snippet with `testutil.CollectAndCompare`, which is
the strongest check of the metric name, HELP/TYPE lines, labels, and values.
`TestScrapeEndpoint` scrapes `/metrics` through `httptest` to confirm the HTTP
surface serves what Prometheus expects. `TestValidateScaledObject` accepts the
shipped manifest and rejects both an unquoted threshold (which resolves to a
non-string tag) and a manifest missing `maxReplicaCount`.

Create `metrics_test.go`:

```go
package backlogexporter

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBacklogGauge(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SetBacklog("orders", 42)

	if got := testutil.ToFloat64(m.backlog.WithLabelValues("orders")); got != 42 {
		t.Errorf("gauge = %v, want 42", got)
	}
}

func TestExposition(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SetBacklog("orders", 42)
	m.SetBacklog("shipments", 7)

	const golden = `# HELP consumer_backlog_messages Number of unprocessed messages waiting in the queue.
# TYPE consumer_backlog_messages gauge
consumer_backlog_messages{queue="orders"} 42
consumer_backlog_messages{queue="shipments"} 7
`
	if err := testutil.CollectAndCompare(m.backlog, strings.NewReader(golden), "consumer_backlog_messages"); err != nil {
		t.Fatalf("exposition mismatch: %v", err)
	}
}

func TestScrapeEndpoint(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SetBacklog("orders", 128)

	srv := httptest.NewServer(Handler(reg))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := `consumer_backlog_messages{queue="orders"} 128`
	if !strings.Contains(string(body), want) {
		t.Errorf("exposition missing %q\n---\n%s", want, body)
	}
}

func TestValidateScaledObject(t *testing.T) {
	t.Parallel()
	if err := ValidateScaledObject([]byte(ScaledObjectYAML)); err != nil {
		t.Fatalf("shipped manifest should be valid: %v", err)
	}

	const unquotedThreshold = `apiVersion: keda.sh/v1alpha1
kind: ScaledObject
spec:
  scaleTargetRef:
    name: orders-consumer
  maxReplicaCount: 20
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus:9090
        query: sum(consumer_backlog_messages)
        threshold: 50
`
	if err := ValidateScaledObject([]byte(unquotedThreshold)); err == nil {
		t.Error("unquoted threshold should fail (metadata values must be strings)")
	}

	const noMax = `apiVersion: keda.sh/v1alpha1
kind: ScaledObject
spec:
  scaleTargetRef:
    name: orders-consumer
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus:9090
        query: sum(consumer_backlog_messages)
        threshold: "50"
`
	if err := ValidateScaledObject([]byte(noMax)); err == nil {
		t.Error("missing maxReplicaCount should fail (unbounded blast radius)")
	}
}

func Example() {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SetBacklog("orders", 128)
	fmt.Printf("%.0f\n", testutil.ToFloat64(m.backlog.WithLabelValues("orders")))
	// Output:
	// 128
}
```

## Review

The exporter is correct when the gauge publishes the aggregate backlog per queue
and the exposition is byte-for-byte what Prometheus expects.
`TestExposition`'s `CollectAndCompare` against the golden snippet is the strongest
proof: it fails on a wrong metric name, a missing HELP/TYPE line, a mislabeled
series, or a wrong value. Keep the registry private (`prometheus.NewRegistry`) so
tests never collide and the process never double-registers.

The mistakes to avoid: do not use a counter for backlog — it is a gauge that rises
and falls, so `Set` the absolute depth each poll. Do not write a PromQL query that
returns zero or many series; wrap it in `sum(...)` so it reduces to one scalar,
and set `ignoreNullValues` deliberately rather than by accident. Do not leave
`threshold` unquoted in the manifest — KEDA metadata values are strings, and
`TestValidateScaledObject` proves an unquoted value is rejected because it resolves
to the `!!int` tag rather than `!!str`. And never ship
a `ScaledObject` without `maxReplicaCount`; the validator rejects it because an
unbounded target turns a burst into a runaway bill. Offline this module cannot
build (the prometheus and yaml modules are not vendored); it is validated by gofmt
and review, and gates fully where those are available.

## Resources

- [KEDA docs: Prometheus scaler](https://keda.sh/docs/2.18/scalers/prometheus/) — `serverAddress`, `query`, `threshold`, `activationThreshold`, `ignoreNullValues`.
- [`prometheus/client_golang/prometheus`](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus) — `NewGaugeVec`, `GaugeVec.WithLabelValues`, `Registry`.
- [`prometheus/client_golang/prometheus/testutil`](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus/testutil) — `ToFloat64`, `CollectAndCompare`.
- [`prometheus/client_golang/prometheus/promhttp`](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus/promhttp) — `HandlerFor` and `HandlerOpts`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-graceful-drain-worker.md](02-graceful-drain-worker.md) | Next: [../05-helm-and-gitops-patterns/00-concepts.md](../05-helm-and-gitops-patterns/00-concepts.md)
