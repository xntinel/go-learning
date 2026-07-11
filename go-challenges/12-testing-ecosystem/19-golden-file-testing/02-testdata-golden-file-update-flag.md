# Exercise 2: On-Disk testdata Goldens Driven by a -update Flag

The inline string does not scale past a few lines. The canonical Go idiom moves
the expectation into `testdata/<name>.golden`, reads it with a helper, and
regenerates it with a package-level `-update` flag. You build an invoice
renderer and pin its output as a committed golden file.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
invoicegold/               independent module: example.com/invoicegold
  go.mod                   go 1.26
  invoice.go               Invoice, LineItem, RenderInvoice -> []byte
  testdata/
    invoice.golden         committed reference output
  cmd/
    demo/
      main.go              renders a sample invoice and prints it
  invoice_test.go          -update flag, goldenFile helper, TempDir write path
```

Files: `invoice.go`, `testdata/invoice.golden`, `cmd/demo/main.go`, `invoice_test.go`.
Implement: `RenderInvoice(Invoice) []byte` producing a deterministic fixed-width invoice.
Test: a `goldenFile(t, name, got)` helper that writes under `-update` and otherwise reads and byte-compares, naming the golden path on mismatch; plus a `TempDir` round-trip of the write path.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/invoicegold/cmd/demo ~/go-exercises/invoicegold/testdata
cd ~/go-exercises/invoicegold
go mod init example.com/invoicegold
```

### The -update flag and the goldenFile helper

The whole idiom rests on one package-level flag:

```go
var update = flag.Bool("update", false, "regenerate golden files in testdata/")
```

The `go test` binary parses this for you before tests run, so `go test -update`
sets it with no `flag.Parse()` call of your own. A single helper centralizes the
read/write/compare logic so every golden assertion is one line at the call site.
Under `*update` it creates the `testdata/` directory with `MkdirAll(..., 0o755)`
and writes the produced bytes with `WriteFile(..., 0o644)` — an explicit,
committable file mode. Otherwise it reads the committed golden and compares bytes
exactly, and on mismatch it prints the golden *path* so a reviewer can open the
file and read the diff, plus the exact command to regenerate. That path in the
failure message is the difference between a usable golden suite and an
infuriating one.

The invoice is deliberately deterministic: fixed-width columns produced with
`fmt.Fprintf` format verbs, a total computed from the line items, and no clock or
id anywhere. Byte comparison is the right contract here because an invoice is a
document whose exact layout matters — a shifted column or a changed separator is
a real change a reviewer should see. Note the trailing-newline policy: the
renderer ends every line, including the last, with a single `\n`, and the golden
file on disk carries exactly that. Keeping the writer and the file agreed on one
trailing newline is what stops the classic "looks identical but bytes differ"
failure.

Create `invoice.go`:

```go
package invoicegold

import (
	"bytes"
	"fmt"
	"strings"
)

// LineItem is one billable row.
type LineItem struct {
	Name  string
	Qty   int
	Price float64
}

// Invoice is a billing document rendered to a stable text layout.
type Invoice struct {
	Number   string
	Customer string
	Items    []LineItem
}

// RenderInvoice produces a deterministic fixed-width invoice. Every line,
// including the last, ends in a single newline, which is the golden file's
// trailing-newline contract.
func RenderInvoice(inv Invoice) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "INVOICE %s\n", inv.Number)
	fmt.Fprintf(&b, "Customer: %s\n", inv.Customer)
	b.WriteString(strings.Repeat("-", 40) + "\n")
	var total float64
	for _, it := range inv.Items {
		amount := float64(it.Qty) * it.Price
		total += amount
		fmt.Fprintf(&b, "%-12s %2d x %8.2f = %9.2f\n", it.Name, it.Qty, it.Price, amount)
	}
	b.WriteString(strings.Repeat("-", 40) + "\n")
	fmt.Fprintf(&b, "%-33s %9.2f\n", "TOTAL", total)
	return b.Bytes()
}
```

Now the committed golden. In a real project you generate this once with
`go test -update` and commit it; here it is provided so the test has a reference
to read.

Create `testdata/invoice.golden`:

```text
INVOICE INV-1001
Customer: Globex
----------------------------------------
Widget        3 x     9.99 =     29.97
Gadget        1 x    49.50 =     49.50
Sprocket     12 x     2.25 =     27.00
----------------------------------------
TOTAL                                106.47
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/invoicegold"
)

func main() {
	inv := invoicegold.Invoice{
		Number:   "INV-1001",
		Customer: "Globex",
		Items: []invoicegold.LineItem{
			{Name: "Widget", Qty: 3, Price: 9.99},
			{Name: "Gadget", Qty: 1, Price: 49.50},
			{Name: "Sprocket", Qty: 12, Price: 2.25},
		},
	}
	os.Stdout.Write(invoicegold.RenderInvoice(inv))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
INVOICE INV-1001
Customer: Globex
----------------------------------------
Widget        3 x     9.99 =     29.97
Gadget        1 x    49.50 =     49.50
Sprocket     12 x     2.25 =     27.00
----------------------------------------
TOTAL                                106.47
```

### Tests

`TestRenderInvoiceGolden` renders the sample and runs it through `goldenFile`,
which compares against `testdata/invoice.golden` on the normal path.
`TestGoldenWriteRoundTrip` exercises the write path against a `t.TempDir` (so it
never clobbers the committed fixture): it writes, reads back, and asserts byte
equality, proving `MkdirAll`/`WriteFile` round-trip the exact bytes.

Create `invoice_test.go`:

```go
package invoicegold

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

// goldenFile compares got against testdata/name, or regenerates it under -update.
func goldenFile(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test -update)", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden mismatch for %s (run: go test -update to regenerate)\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func sampleInvoice() Invoice {
	return Invoice{
		Number:   "INV-1001",
		Customer: "Globex",
		Items: []LineItem{
			{Name: "Widget", Qty: 3, Price: 9.99},
			{Name: "Gadget", Qty: 1, Price: 49.50},
			{Name: "Sprocket", Qty: 12, Price: 2.25},
		},
	}
}

func TestRenderInvoiceGolden(t *testing.T) {
	got := RenderInvoice(sampleInvoice())
	goldenFile(t, "invoice.golden", got)
}

func TestGoldenWriteRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "out.golden")
	got := RenderInvoice(sampleInvoice())

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, got, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(back, got) {
		t.Fatalf("round-trip mismatch:\ngot:\n%s\nback:\n%s", got, back)
	}
}

func ExampleRenderInvoice() {
	inv := Invoice{Number: "INV-9", Customer: "Acme", Items: []LineItem{{Name: "Bolt", Qty: 2, Price: 1.50}}}
	os.Stdout.Write(RenderInvoice(inv))
	// Output:
	// INVOICE INV-9
	// Customer: Acme
	// ----------------------------------------
	// Bolt          2 x     1.50 =      3.00
	// ----------------------------------------
	// TOTAL                                  3.00
}
```

## Review

The pattern is correct when the default `go test` run only reads and compares,
and only `-update` writes — so CI, which never passes `-update`, can only ever
fail on a real drift. The golden lives under `testdata/`, which the `go` tool
excludes from builds and vet, and is committed like source; the mismatch message
names the path and the regenerate command so a reviewer can act. The two traps
this exercise guards against: writing the golden without a trailing-newline
policy (the renderer and the file must agree on exactly one), and letting the
write path touch the committed fixture during a normal test (the round-trip test
uses `t.TempDir` precisely to avoid that). Regenerate deliberately with
`go test -update`, then read the `git diff` before committing.

## Resources

- [os.WriteFile / os.ReadFile](https://pkg.go.dev/os#WriteFile) — the read and write primitives, with explicit file modes.
- [flag.Bool](https://pkg.go.dev/flag#Bool) — declaring the package-level `-update` flag the test binary parses.
- [cmd/go: test packages and testdata](https://pkg.go.dev/cmd/go#hdr-Test_packages) — why `testdata/` is ignored by the build tool.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-http-handler-golden-response.md](03-http-handler-golden-response.md)
