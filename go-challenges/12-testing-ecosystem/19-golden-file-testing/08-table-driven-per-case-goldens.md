# Exercise 8: Table-Driven Subtests with One Golden File Per Case

A template renderer has many fixtures — a welcome email, a password reset, a
purchase receipt — and cramming them into one golden hides failures. The pattern
that scales is one `testdata/<case>.golden` per `t.Run` subtest: each fixture is
independently updatable, independently reviewable, and its failure is isolated.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
mailtmpl/                  independent module: example.com/mailtmpl
  go.mod                   go 1.26
  mail.go                  RenderEmail(kind, data) via text/template
  testdata/
    welcome.golden
    password_reset.golden
    purchase_receipt.golden
  cmd/
    demo/
      main.go              renders each template kind and prints it
  mail_test.go             per-case subtests, safeName mapping, -update
```

Files: `mail.go`, three `testdata/*.golden`, `cmd/demo/main.go`, `mail_test.go`.
Implement: `RenderEmail(kind string, data map[string]any) (string, error)` over a `text/template` set.
Test: a table of named cases, each mapped by a filesystem-safe name to its own golden, run under `t.Run`; `-update` regenerates all cases; a failing case names its file.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/19-golden-file-testing/08-table-driven-per-case-goldens/cmd/demo go-solutions/12-testing-ecosystem/19-golden-file-testing/08-table-driven-per-case-goldens/testdata
cd go-solutions/12-testing-ecosystem/19-golden-file-testing/08-table-driven-per-case-goldens
```

### One golden per subtest, and the name mapping

The temptation with many fixtures is a single golden holding all of them
concatenated. It is a trap: a change to the welcome email rewrites the shared
file, and the diff buries — or a careless `-update` masks — a simultaneous
regression in the receipt. Worse, a failure in one case aborts before the others
run, so you fix one thing, rerun, discover the next, and iterate blindly. The
per-case pattern fixes all of this: each `t.Run` subtest owns exactly one
`testdata/<case>.golden`, so cases are isolated (one failure does not mask
another), independently updatable (`-update` regenerates every case in one run,
and you review each file's diff separately), and self-describing (a failure names
its own file so you know which fixture drifted).

The one piece of glue is the name mapping. A human-readable case name like
`"purchase receipt"` is not a safe file name, and `t.Run` itself sanitizes subtest
names (spaces become underscores) for its `-run` filter. So a `safeName` helper
lowercases the case name and replaces spaces and slashes with underscores,
producing `purchase_receipt.golden`. Keep the mapping total and deterministic —
two different case names must not collapse to the same file, or one golden would
shadow another.

`RenderEmail` executes one named template from a shared set. The templates are
deterministic text with a single-trailing-newline policy, so each golden is a
stable, human-readable email a reviewer can proofread in the diff.

Create `mail.go`:

```go
package mailtmpl

import (
	"bytes"
	"strings"
	"text/template"
)

var tmpl = template.Must(template.New("root").Parse(`
{{define "welcome"}}Subject: Welcome to {{.Product}}

Hi {{.Name}},

Your {{.Product}} account is ready. Sign in to get started.
{{end}}
{{define "password_reset"}}Subject: Reset your password

Hi {{.Name}},

Use this link within {{.TTLMinutes}} minutes to reset your password:
{{.ResetURL}}
{{end}}
{{define "receipt"}}Subject: Receipt for {{.Invoice}}

Hi {{.Name}},

We charged {{.Amount}} for invoice {{.Invoice}}. Thank you.
{{end}}
`))

// RenderEmail executes the named template with data, applying a single-trailing-
// newline policy so the output is a stable golden target.
func RenderEmail(kind string, data map[string]any) (string, error) {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, kind, data); err != nil {
		return "", err
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}
```

Now the committed per-case goldens.

Create `testdata/welcome.golden`:

```text
Subject: Welcome to Bastion

Hi Ada,

Your Bastion account is ready. Sign in to get started.
```

Create `testdata/password_reset.golden`:

```text
Subject: Reset your password

Hi Ada,

Use this link within 30 minutes to reset your password:
https://app.example.com/reset?token=abc123
```

Create `testdata/purchase_receipt.golden`:

```text
Subject: Receipt for INV-1001

Hi Ada,

We charged $49.50 for invoice INV-1001. Thank you.
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mailtmpl"
)

func main() {
	out, err := mailtmpl.RenderEmail("welcome", map[string]any{"Name": "Ada", "Product": "Bastion"})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Subject: Welcome to Bastion

Hi Ada,

Your Bastion account is ready. Sign in to get started.
```

### Tests

Each case names its human-readable title, the template kind, and its data;
`safeName` maps the title to the golden file. Every subtest reads or writes only
its own golden, and on mismatch names that file.

Create `mail_test.go`:

```go
package mailtmpl

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

// safeName maps a human case title to a filesystem-safe golden base name.
func safeName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	return s
}

func goldenPerCase(t *testing.T, base, got string) {
	t.Helper()
	path := filepath.Join("testdata", base+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test -update)", path, err)
	}
	if string(want) != got {
		t.Fatalf("golden mismatch for %s (run: go test -update)\n--- got ---\n%s--- want ---\n%s", path, got, want)
	}
}

func TestEmailGoldens(t *testing.T) {
	cases := []struct {
		name string
		kind string
		data map[string]any
	}{
		{"welcome", "welcome", map[string]any{"Name": "Ada", "Product": "Bastion"}},
		{"password reset", "password_reset", map[string]any{"Name": "Ada", "ResetURL": "https://app.example.com/reset?token=abc123", "TTLMinutes": 30}},
		{"purchase receipt", "receipt", map[string]any{"Name": "Ada", "Amount": "$49.50", "Invoice": "INV-1001"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderEmail(tc.kind, tc.data)
			if err != nil {
				t.Fatalf("RenderEmail(%q): %v", tc.kind, err)
			}
			goldenPerCase(t, safeName(tc.name), got)
		})
	}
}

func TestSafeNameIsInjective(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, name := range []string{"welcome", "password reset", "purchase receipt"} {
		base := safeName(name)
		if prev, ok := seen[base]; ok {
			t.Fatalf("safeName collision: %q and %q both map to %q", prev, name, base)
		}
		seen[base] = name
	}
}

func ExampleRenderEmail() {
	out, _ := RenderEmail("receipt", map[string]any{"Name": "Ada", "Amount": "$49.50", "Invoice": "INV-1001"})
	fmt.Print(out)
	// Output:
	// Subject: Receipt for INV-1001
	//
	// Hi Ada,
	//
	// We charged $49.50 for invoice INV-1001. Thank you.
}
```

## Review

The pattern is correct when each subtest touches exactly one golden, so a change
to the welcome email cannot rewrite or mask the receipt, and a failure names the
file that drifted. The `safeName` mapping must be injective — the
`TestSafeNameIsInjective` guard proves no two case titles collapse to one file,
which would silently make one fixture shadow another. Isolation is the whole
point: `-update` regenerates every case in one run, but you still read each
file's diff separately in review. Contrast this with the one-shared-golden
anti-pattern, where the isolation is gone and a broken case hides behind a
green-looking aggregate. When you add a template kind, add a case row and a
`testdata/<name>.golden`; the `-update` run creates the new file and the review
sees a new fixture appear.

## Resources

- [text/template](https://pkg.go.dev/text/template) — the `ExecuteTemplate` and `define` blocks used here.
- [testing: T.Run subtests](https://pkg.go.dev/testing#T.Run) — isolated, independently named subtests.
- [path/filepath.Join](https://pkg.go.dev/path/filepath#Join) — building the per-case golden path portably.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-ci-update-guard-and-stale-goldens.md](09-ci-update-guard-and-stale-goldens.md)
