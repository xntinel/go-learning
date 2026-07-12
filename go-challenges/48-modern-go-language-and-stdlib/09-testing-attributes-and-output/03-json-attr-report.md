# Exercise 3: CI Attribute Report from go test -json

This is the on-the-job half of the pipeline: the consumer. The producer metadata from
exercises 1 and 2 is only useful if something downstream reads it back. Here you build
the tool a custom CI gate would run — parse the `go test -json` event stream, aggregate
each test's attributes, status, and elapsed time, and flag any test missing a required
attribute. This is the shape `gotestsum` and hand-rolled gates use in production.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
attrreport/                 independent module: example.com/attrreport
  go.mod                    go 1.25
  report.go                 event; TestResult; Report; BuildReport; ErrMalformedStream
  cmd/
    demo/
      main.go               reads a JSON event stream from stdin, prints the report
  report_test.go            canned multi-test stream; malformed-stream error; Example
```

- Files: `report.go`, `cmd/demo/main.go`, `report_test.go`.
- Implement: `BuildReport(r io.Reader) (Report, error)` that decodes newline-delimited `go test -json` events, groups per fully-qualified test its attributes / final status / elapsed seconds, and reports the sorted set of tests missing the required `owner` attribute.
- Test: a canned stream mixing `run`/`output`/`attr`/`pass`/`fail` across two tests, asserting grouping, status, elapsed, and the missing-owner set; a malformed line returning a wrapped sentinel; an `Example` with `// Output:`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/09-testing-attributes-and-output/03-json-attr-report/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/09-testing-attributes-and-output/03-json-attr-report
go mod edit -go=1.25
```

### Decoding a stream of JSON values

`go test -json` does not emit one JSON array; it emits a sequence of JSON objects, one
per line. `json.Decoder` is built for exactly this: calling `Decode` repeatedly reads
consecutive values from the stream until it returns `io.EOF`. The loop decodes into a
reused `event` value, dispatches on `Action`, and stops cleanly on EOF. Any other
decode error means a malformed line, which is wrapped in the `ErrMalformedStream`
sentinel so a caller can distinguish "bad input" from any future error with
`errors.Is`.

The `event` struct mirrors the `test2json` event, but only the fields this tool needs.
`Elapsed` is `*float64` — a pointer — because the stream sets it only on `pass` and
`fail` events; a `nil` `Elapsed` means "not a terminal event", which is different from
"zero seconds". Copying `*ev.Elapsed` only when non-nil keeps a `run` event from
zeroing an elapsed time recorded by the later `pass`.

### Aggregating per fully-qualified test

A test is identified across events by its package plus its name, so the report keys
each result by `Package + "." + Test`. Package-level events (a `start` or a package
`pass` with an empty `Test`) are skipped — they are not per-test facts. For each event
with a `Test`, the tool looks up or creates the `TestResult`, then folds the event in:
an `attr` event adds `Key -> Value` to the result's attribute map; a `pass`, `fail`,
or `skip` sets the terminal status and captures elapsed. Actions the tool does not
model (`run`, `output`, `pause`, `cont`) fall through the `switch` untouched — a robust
consumer ignores what it does not understand rather than failing on it.

The gate policy is the payoff. After folding every event, the tool walks the results
and collects the fully-qualified names of tests whose attribute map lacks the required
`owner` key. That slice is sorted so the output is deterministic. A real CI gate would
exit non-zero if it is non-empty; here `BuildReport` just reports it, keeping the tool
a pure function that is trivial to test.

Create `report.go`:

```go
package attrreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// requiredAttr is the attribute every test must declare; BuildReport flags any
// test missing it. A real gate would exit non-zero when that set is non-empty.
const requiredAttr = "owner"

// ErrMalformedStream wraps any decode failure on the event stream so callers can
// branch on it with errors.Is.
var ErrMalformedStream = errors.New("attrreport: malformed json event stream")

// event is the subset of the go test -json (test2json) event we consume. Elapsed
// is a pointer because it is set only on pass/fail events; nil means "not set".
type event struct {
	Action  string
	Package string
	Test    string
	Key     string
	Value   string
	Elapsed *float64
}

// TestResult is the aggregated view of one test across all its events.
type TestResult struct {
	Package string
	Test    string
	Status  string // "pass", "fail", "skip", or "" if no terminal event seen
	Elapsed float64
	Attrs   map[string]string
}

// Report is the whole run: results keyed by "Package.Test", plus the sorted set of
// fully-qualified tests missing the required attribute.
type Report struct {
	Tests        map[string]TestResult
	MissingOwner []string
}

// String renders a stable, human-readable report (tests in sorted order).
func (r Report) String() string {
	var b strings.Builder
	for _, fq := range slices.Sorted(maps.Keys(r.Tests)) {
		tr := r.Tests[fq]
		owner := tr.Attrs[requiredAttr]
		if owner == "" {
			owner = "<none>"
		}
		fmt.Fprintf(&b, "%s status=%s elapsed=%.2fs owner=%s\n", fq, tr.Status, tr.Elapsed, owner)
	}
	for _, fq := range r.MissingOwner {
		fmt.Fprintf(&b, "MISSING-OWNER %s\n", fq)
	}
	return b.String()
}

// BuildReport consumes a go test -json event stream and aggregates, per
// fully-qualified test, its attributes, terminal status, and elapsed seconds. A
// malformed line returns an error wrapping ErrMalformedStream and no report.
func BuildReport(r io.Reader) (Report, error) {
	dec := json.NewDecoder(r)
	results := map[string]TestResult{}
	for {
		var ev event
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Report{}, fmt.Errorf("%w: %v", ErrMalformedStream, err)
		}
		if ev.Test == "" {
			continue // package-level event; not a per-test fact
		}
		fq := ev.Package + "." + ev.Test
		tr, ok := results[fq]
		if !ok {
			tr = TestResult{Package: ev.Package, Test: ev.Test, Attrs: map[string]string{}}
		}
		switch ev.Action {
		case "attr":
			tr.Attrs[ev.Key] = ev.Value
		case "pass", "fail", "skip":
			tr.Status = ev.Action
			if ev.Elapsed != nil {
				tr.Elapsed = *ev.Elapsed
			}
		}
		results[fq] = tr
	}
	var missing []string
	for fq, tr := range results {
		if _, ok := tr.Attrs[requiredAttr]; !ok {
			missing = append(missing, fq)
		}
	}
	slices.Sort(missing)
	return Report{Tests: results, MissingOwner: missing}, nil
}
```

### The runnable demo

The demo reads the event stream from stdin, so in real use you pipe it directly from
the test runner: `go test -json ./... | go run ./cmd/demo`. Offline, feed it a canned
stream on stdin to see the same report.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/attrreport"
)

func main() {
	rep, err := attrreport.BuildReport(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report failed:", err)
		os.Exit(1)
	}
	fmt.Print(rep)
}
```

Run it with a canned stream on stdin:

```bash
go run ./cmd/demo <<'EOF'
{"Action":"run","Package":"example.com/pay","Test":"TestCharge"}
{"Action":"attr","Package":"example.com/pay","Test":"TestCharge","Key":"owner","Value":"payments"}
{"Action":"pass","Package":"example.com/pay","Test":"TestCharge","Elapsed":0.42}
{"Action":"run","Package":"example.com/pay","Test":"TestRefund"}
{"Action":"attr","Package":"example.com/pay","Test":"TestRefund","Key":"case_id","Value":"TC-2"}
{"Action":"fail","Package":"example.com/pay","Test":"TestRefund","Elapsed":1.5}
EOF
```

Expected output:

```
example.com/pay.TestCharge status=pass elapsed=0.42s owner=payments
example.com/pay.TestRefund status=fail elapsed=1.50s owner=<none>
MISSING-OWNER example.com/pay.TestRefund
```

### Tests

The main test feeds a realistic stream — two tests in one package, with `run`,
`output`, `attr`, and terminal events interleaved — and asserts the report grouped the
attributes under the right test, captured each status and elapsed time, and produced
exactly the expected missing-owner set. The negative test feeds a truncated JSON line
and asserts the error wraps `ErrMalformedStream` via `errors.Is`. The `Example` prints
a small report through the `Report.String` method with an `// Output:` block.

Create `report_test.go`:

```go
package attrreport

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const sampleStream = `{"Action":"run","Package":"example.com/pay","Test":"TestCharge"}
{"Action":"output","Package":"example.com/pay","Test":"TestCharge","Output":"=== RUN   TestCharge\n"}
{"Action":"attr","Package":"example.com/pay","Test":"TestCharge","Key":"owner","Value":"payments"}
{"Action":"attr","Package":"example.com/pay","Test":"TestCharge","Key":"case_id","Value":"TC-1"}
{"Action":"pass","Package":"example.com/pay","Test":"TestCharge","Elapsed":0.42}
{"Action":"run","Package":"example.com/pay","Test":"TestRefund"}
{"Action":"attr","Package":"example.com/pay","Test":"TestRefund","Key":"case_id","Value":"TC-2"}
{"Action":"fail","Package":"example.com/pay","Test":"TestRefund","Elapsed":1.5}`

func TestBuildReportAggregates(t *testing.T) {
	t.Parallel()
	rep, err := BuildReport(strings.NewReader(sampleStream))
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	charge, ok := rep.Tests["example.com/pay.TestCharge"]
	if !ok {
		t.Fatal("missing TestCharge result")
	}
	if charge.Status != "pass" {
		t.Errorf("TestCharge status = %q, want pass", charge.Status)
	}
	if charge.Elapsed != 0.42 {
		t.Errorf("TestCharge elapsed = %v, want 0.42", charge.Elapsed)
	}
	if charge.Attrs["owner"] != "payments" || charge.Attrs["case_id"] != "TC-1" {
		t.Errorf("TestCharge attrs = %v", charge.Attrs)
	}

	refund, ok := rep.Tests["example.com/pay.TestRefund"]
	if !ok {
		t.Fatal("missing TestRefund result")
	}
	if refund.Status != "fail" {
		t.Errorf("TestRefund status = %q, want fail", refund.Status)
	}
	if refund.Elapsed != 1.5 {
		t.Errorf("TestRefund elapsed = %v, want 1.5", refund.Elapsed)
	}

	wantMissing := []string{"example.com/pay.TestRefund"}
	if !slices.Equal(rep.MissingOwner, wantMissing) {
		t.Errorf("MissingOwner = %v, want %v", rep.MissingOwner, wantMissing)
	}
}

func TestBuildReportMalformed(t *testing.T) {
	t.Parallel()
	_, err := BuildReport(strings.NewReader(`{"Action":"run" BROKEN`))
	if !errors.Is(err, ErrMalformedStream) {
		t.Fatalf("error = %v, want errors.Is(_, ErrMalformedStream)", err)
	}
}

func ExampleBuildReport() {
	stream := `{"Action":"attr","Package":"p","Test":"TestA","Key":"owner","Value":"team-a"}
{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0.1}
{"Action":"fail","Package":"p","Test":"TestB","Elapsed":0.2}`
	rep, err := BuildReport(strings.NewReader(stream))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(rep)
	// Output:
	// p.TestA status=pass elapsed=0.10s owner=team-a
	// p.TestB status=fail elapsed=0.20s owner=<none>
	// MISSING-OWNER p.TestB
}
```

## Review

The report is correct when it groups by fully-qualified name and preserves the pointer
semantics of `Elapsed`. Grouping is what `TestBuildReportAggregates` checks: two tests
in one package must not collide, and each attribute must land under its own test. The
`*float64` matters because a `run` event carries no elapsed time; if `Elapsed` were a
plain `float64` you could not tell "not set" from "zero", and a stray zero could
overwrite the real timing — copying only when `ev.Elapsed != nil` avoids that.

The mistakes to avoid: do not treat every decode error as EOF — only `io.EOF` ends the
loop cleanly, and any other error must surface as `ErrMalformedStream`, which
`TestBuildReportMalformed` pins with `errors.Is`. Do not crash on unmodeled actions;
`run` and `output` events fall through the `switch` by design, because real streams
contain many action types and a consumer that panics on the first unfamiliar one is
useless. And remember `Attr` never enforced anything: the enforcement is exactly this
tool's `MissingOwner` set, which is why building the consumer is the point of the
lesson. Run `go test -race` to confirm the aggregation is clean.

## Resources

- [`cmd/test2json`](https://pkg.go.dev/cmd/test2json) — the JSON event stream, its `Action` values, and the `Key`/`Value`/`Elapsed` fields.
- [`encoding/json.Decoder`](https://pkg.go.dev/encoding/json#Decoder) — decoding a stream of consecutive JSON values with repeated `Decode` calls.
- [Extending go test for LLM Evaluation](https://mattermost.com/blog/extending-go-test-for-llm-evaluation/) — a real-world use of `t.Attr` for eval scores consumed downstream.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../10-container-aware-gomaxprocs/00-concepts.md](../10-container-aware-gomaxprocs/00-concepts.md)
