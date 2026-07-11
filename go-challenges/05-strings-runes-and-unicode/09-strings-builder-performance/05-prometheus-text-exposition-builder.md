# Exercise 5: Render a Prometheus Text-Exposition Metrics Body

A `/metrics` endpoint is scraped every 15 seconds, and each scrape rebuilds the whole
exposition body: `# HELP` and `# TYPE` lines plus samples like
`http_requests_total{code="200",method="POST"} 42`. This exercise builds that body
with a single `strings.Builder`, formats float values with `strconv.AppendFloat`,
sorts label keys for deterministic output, and escapes label values correctly.

This module is self-contained.

## What you'll build

```text
promexpo/                    independent module: example.com/promexpo
  go.mod
  promexpo.go                Metric, Sample; Render([]Metric) string; label/value escaping
  cmd/
    demo/
      main.go                renders a small metric set, prints the exposition body
  promexpo_test.go           exact-output golden, float edge cases, escaping
```

Files: `promexpo.go`, `cmd/demo/main.go`, `promexpo_test.go`.
Implement: `Render(metrics []Metric) string` producing HELP/TYPE lines and samples with sorted labels, `strconv.AppendFloat` values, and escaped label values.
Test: exact exposition for a fixed metric set; float edge cases (integers without spurious decimals, NaN/Inf); label escaping of quotes and backslashes.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p ~/go-exercises/promexpo/cmd/demo
cd ~/go-exercises/promexpo
go mod init example.com/promexpo
```

### Determinism and escaping are the whole job

The Prometheus text exposition format is line-oriented and picky, and two properties
make or break it. First, determinism: a scrape must produce the same bytes for the
same metric state, but Go map iteration order is randomized, so if you emit labels in
map order the output shuffles between scrapes. That breaks diffing, caching, and any
test. The fix is to collect the label keys, `slices.Sort` them, and emit in sorted
order — every time. Second, escaping: a label *value* may contain a double-quote, a
backslash, or a newline, and the format requires those be escaped (`\` becomes `\\`,
`"` becomes `\"`, newline becomes `\n`); a `# HELP` line escapes backslash and
newline but not quotes. Emit a raw quote inside a label value and the line no longer
parses. We use `strings.NewReplacer`, which scans the input once and is the right tool
for a fixed set of non-overlapping replacements.

Value formatting is where `strconv.AppendFloat` earns its place. Prometheus values are
float64, but a counter of `42` must render as `42`, not `42.000000`. `AppendFloat`
with format `'g'` and precision `-1` produces the shortest representation that round-
trips: `42` stays `42`, `3.14` stays `3.14`, and a large value uses exponent form,
all without a `fmt.Sprintf` per sample. The three special float values need explicit
handling to match the format's spelling: `NaN`, `+Inf`, `-Inf`. Everything is written
into one Builder that grows once and yields the body with a single copy-free `String()`
— appropriate for an endpoint hit on a fixed schedule where you do not want to allocate
a fresh string per line.

Create `promexpo.go`:

```go
package promexpo

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

// Sample is one measurement: a set of label key/value pairs and a value.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Metric is a named metric family with HELP/TYPE metadata and its samples.
type Metric struct {
	Name    string
	Help    string
	Type    string
	Samples []Sample
}

var labelValueEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

// Render builds a Prometheus text exposition body. Label keys are sorted so the
// output is deterministic across scrapes.
func Render(metrics []Metric) string {
	var b strings.Builder
	for _, m := range metrics {
		if m.Help != "" {
			b.WriteString("# HELP ")
			b.WriteString(m.Name)
			b.WriteByte(' ')
			b.WriteString(helpEscaper.Replace(m.Help))
			b.WriteByte('\n')
		}
		if m.Type != "" {
			b.WriteString("# TYPE ")
			b.WriteString(m.Name)
			b.WriteByte(' ')
			b.WriteString(m.Type)
			b.WriteByte('\n')
		}
		for _, s := range m.Samples {
			b.WriteString(m.Name)
			writeLabels(&b, s.Labels)
			b.WriteByte(' ')
			writeValue(&b, s.Value)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func writeLabels(b *strings.Builder, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(labelValueEscaper.Replace(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

func writeValue(b *strings.Builder, v float64) {
	switch {
	case math.IsNaN(v):
		b.WriteString("NaN")
	case math.IsInf(v, 1):
		b.WriteString("+Inf")
	case math.IsInf(v, -1):
		b.WriteString("-Inf")
	default:
		var buf [32]byte
		b.Write(strconv.AppendFloat(buf[:0], v, 'g', -1, 64))
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/promexpo"
)

func main() {
	metrics := []promexpo.Metric{
		{
			Name: "http_requests_total",
			Help: "Total HTTP requests.",
			Type: "counter",
			Samples: []promexpo.Sample{
				{Labels: map[string]string{"method": "POST", "code": "200"}, Value: 42},
				{Labels: map[string]string{"method": "GET", "code": "200"}, Value: 1024},
			},
		},
	}
	fmt.Print(promexpo.Render(metrics))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
# HELP http_requests_total Total HTTP requests.
# TYPE http_requests_total counter
http_requests_total{code="200",method="POST"} 42
http_requests_total{code="200",method="GET"} 1024
```

### Tests

The golden test fixes a metric set and asserts the exact bytes, including the sorted
label order. Separate tests pin float formatting (an integer value must not grow a
`.0`, and NaN/Inf render with their format spellings) and label-value escaping.

Create `promexpo_test.go`:

```go
package promexpo

import (
	"fmt"
	"math"
	"testing"
)

func TestRenderGolden(t *testing.T) {
	t.Parallel()

	metrics := []Metric{
		{
			Name: "http_requests_total",
			Help: "Total HTTP requests.",
			Type: "counter",
			Samples: []Sample{
				{Labels: map[string]string{"method": "POST", "code": "200"}, Value: 42},
				{Labels: map[string]string{"method": "GET", "code": "200"}, Value: 1024},
			},
		},
	}

	const want = "# HELP http_requests_total Total HTTP requests.\n" +
		"# TYPE http_requests_total counter\n" +
		`http_requests_total{code="200",method="POST"} 42` + "\n" +
		`http_requests_total{code="200",method="GET"} 1024` + "\n"

	if got := Render(metrics); got != want {
		t.Fatalf("Render mismatch:\n got  %q\n want %q", got, want)
	}
}

func TestValueFormatting(t *testing.T) {
	t.Parallel()

	cases := []struct {
		value float64
		want  string
	}{
		{42, "42"},
		{3.14, "3.14"},
		{0, "0"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "+Inf"},
		{math.Inf(-1), "-Inf"},
	}
	for _, tc := range cases {
		m := []Metric{{Name: "m", Samples: []Sample{{Value: tc.value}}}}
		want := "m " + tc.want + "\n"
		if got := Render(m); got != want {
			t.Fatalf("value %v: got %q, want %q", tc.value, got, want)
		}
	}
}

func TestLabelValueEscaping(t *testing.T) {
	t.Parallel()

	m := []Metric{{
		Name:    "m",
		Samples: []Sample{{Labels: map[string]string{"path": `a"b\c`}, Value: 1}},
	}}
	const want = `m{path="a\"b\\c"} 1` + "\n"
	if got := Render(m); got != want {
		t.Fatalf("escaping: got %q, want %q", got, want)
	}
}

func ExampleRender() {
	m := []Metric{{Name: "up", Samples: []Sample{{Value: 1}}}}
	fmt.Print(Render(m))
	// Output: up 1
}
```

## Review

The renderer is correct when it is deterministic and well-escaped: sorted label keys
make the same state produce the same bytes every scrape, and `strings.NewReplacer`
turns quotes, backslashes, and newlines in label values into their escaped forms so
each line still parses. `strconv.AppendFloat` with `'g'`/`-1` gives the shortest
value string — `42`, not `42.000000` — while NaN and the infinities get their format
spellings. The whole body is one Builder with a single `String()` finish, which is
what you want on a fixed-schedule scrape endpoint rather than allocating per line.

## Resources

- [Prometheus text exposition format](https://prometheus.io/docs/instrumenting/exposition_formats/#text-based-format) — HELP/TYPE lines, label and value escaping.
- [strconv.AppendFloat](https://pkg.go.dev/strconv#AppendFloat) — shortest float formatting with `'g'`/`-1`.
- [slices.Sort](https://pkg.go.dev/slices#Sort) — deterministic label ordering.

---

Prev: [04-csv-export-row-encoder.md](04-csv-export-row-encoder.md) | Back to [00-concepts.md](00-concepts.md) | Next: [06-buffer-pool-hot-path.md](06-buffer-pool-hot-path.md)
