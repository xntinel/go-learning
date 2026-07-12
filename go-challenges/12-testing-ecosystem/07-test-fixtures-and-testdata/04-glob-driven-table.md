# Exercise 4: Data-driven table tests — discover input/golden pairs with filepath.Glob

When a transform accumulates dozens of cases, hand-writing a table entry per case in Go becomes the bottleneck. The scalable form stores each case as two files — `testdata/<name>.input` and `testdata/<name>.golden` — and lets `filepath.Glob` discover them. Adding a case becomes adding two files, never touching Go. This module builds a SQL fingerprint normalizer driven that way.

## What you'll build

```text
normalizer/                   independent module: example.com/normalizer
  go.mod                      go 1.26
  normalize.go                Normalize(sql string) string  (literals -> ?, whitespace collapsed)
  cmd/
    demo/
      main.go                 normalizes a sample query and prints it
  normalize_test.go           filepath.Glob discovers *.input/*.golden pairs; one subtest each
  testdata/
    select.input   select.golden
    insert.input   insert.golden
    whitespace.input   whitespace.golden
```

Files: `normalize.go`, `cmd/demo/main.go`, `normalize_test.go`, and three `.input`/`.golden` pairs.
Implement: `Normalize` that replaces string and numeric literals with `?` and collapses whitespace, so equivalent queries fingerprint identically.
Test: `filepath.Glob("testdata/*.input")`, derive each `.golden` sibling, run a subtest per pair named from `filepath.Base`, and fail loudly on zero matches or a missing golden.
Verify: `go test -count=1 -race ./...`

### Discovery turns adding a case into adding files

Query fingerprinting — collapsing `WHERE id = 42` and `WHERE id = 99` to the same `WHERE id = ?` so a monitoring system can group them — is a real backend task with an open-ended set of cases. A table literal in Go grows one stanza per case and every new case is a code change and a code review. Glob-driven discovery inverts that: the test globs `testdata/*.input`, and each match becomes a subtest that reads the input, reads its `.golden` sibling, and compares. A teammate adds a case by dropping `refund.input` and `refund.golden` into `testdata/` — no Go edit, no merge conflict on a shared table.

Three details make the discovery correct rather than merely convenient. Derive the golden path *from* the input path (`strings.TrimSuffix(in, ".input") + ".golden"`) so the pairing is mechanical and cannot drift. Name each subtest with `filepath.Base(in)` so a failure reads `Normalize/select.input` and points straight at the offending files. And — the trap that silently defeats the whole approach — fail explicitly when `Glob` returns zero matches. A glob that matches nothing produces an empty loop, and an empty loop asserts nothing, so the suite goes green while testing exactly none of your cases. A single `if len(inputs) == 0 { t.Fatal(...) }` is the guard that keeps a mis-typed path or an empty `testdata/` from masquerading as a passing suite. Likewise a missing `.golden` is a hard failure, not a skip.

`Normalize` is deterministic: it replaces `'...'` string literals and bare integer literals with `?`, then collapses every run of whitespace to a single space and trims. Order matters — strings are replaced before numbers so a digit inside a quoted string is not rewritten twice, and whitespace is collapsed last so the fingerprint is single-line and stable.

Create `normalize.go`:

```go
package normalizer

import (
	"regexp"
	"strings"
)

var (
	strRe = regexp.MustCompile(`'[^']*'`)
	numRe = regexp.MustCompile(`\b\d+\b`)
	wsRe  = regexp.MustCompile(`\s+`)
)

// Normalize turns a SQL statement into a stable fingerprint: string and integer
// literals become '?', and all whitespace collapses to single spaces. Two
// queries that differ only in their literal values fingerprint identically.
func Normalize(sql string) string {
	s := strRe.ReplaceAllString(sql, "?")
	s = numRe.ReplaceAllString(s, "?")
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
```

Now the fixture pairs. Each `.input` is a raw query; each `.golden` is its fingerprint.

Create `testdata/select.input`:

```text
SELECT id, name
FROM users
WHERE id = 42 AND name = 'alice'
```

Create `testdata/select.golden`:

```text
SELECT id, name FROM users WHERE id = ? AND name = ?
```

Create `testdata/insert.input`:

```text
INSERT   INTO orders (id, total)
VALUES   (100, 200)
```

Create `testdata/insert.golden`:

```text
INSERT INTO orders (id, total) VALUES (?, ?)
```

Create `testdata/whitespace.input`:

```text
   SELECT   *   FROM   logs   WHERE ts > 1699999999
```

Create `testdata/whitespace.golden`:

```text
SELECT * FROM logs WHERE ts > ?
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/normalizer"
)

func main() {
	fmt.Println(normalizer.Normalize("SELECT *  FROM  t  WHERE id = 7 AND s = 'x'"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
SELECT * FROM t WHERE id = ? AND s = ?
```

### The test

The test globs, guards against zero matches, sorts for deterministic ordering, then runs one subtest per pair. The golden path is derived, never hardcoded. The golden file carries a trailing newline (the fixture convention), so it is trimmed before comparison.

Create `normalize_test.go`:

```go
package normalizer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestNormalizeGoldenPairs(t *testing.T) {
	t.Parallel()

	inputs, err := filepath.Glob(filepath.Join("testdata", "*.input"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(inputs) == 0 {
		t.Fatal("no *.input fixtures found under testdata/ (empty suite would pass silently)")
	}
	sort.Strings(inputs)

	for _, in := range inputs {
		golden := strings.TrimSuffix(in, ".input") + ".golden"
		t.Run(filepath.Base(in), func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatalf("read input: %v", err)
			}
			wantBytes, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden %s: %v", golden, err)
			}
			got := Normalize(string(raw))
			want := strings.TrimRight(string(wantBytes), "\n")
			if got != want {
				t.Fatalf("Normalize(%s):\ngot:  %q\nwant: %q", filepath.Base(in), got, want)
			}
		})
	}
}
```

## Review

The suite is correct when each `.input` fingerprints to its `.golden` and, crucially, when a globbed-empty `testdata/` fails instead of passing. The failure modes this exercise targets are the quiet ones: a zero-match glob that asserts nothing, a golden path built by fragile string surgery instead of derived from the input, and a missing golden swallowed as a skip. Guard the count, derive the pairing, name subtests from `filepath.Base`, and fail hard on a missing sibling. Adding coverage is then a two-file drop that a reviewer reads as data, not as a Go table they must audit.

## Resources

- [path/filepath: Glob](https://pkg.go.dev/path/filepath#Glob) — pattern discovery with sorted results.
- [path/filepath: Base and Join](https://pkg.go.dev/path/filepath#Base) — deriving names and building paths portably.
- [testing: T.Run](https://pkg.go.dev/testing#T.Run) — subtests named per case.

---

Back to [03-golden-update-flag.md](03-golden-update-flag.md) | Next: [05-embed-fixtures.md](05-embed-fixtures.md)
