# Exercise 8: Table-driven snapshots with per-case golden files

Snapshots scale across many cases the same way assertions do: a table of named
cases, each mapping to its own `testdata/<name>.golden`, driven by `t.Run`
subtests. Adding a case becomes adding a row plus a golden file, and one failing
case reports its own name and golden path without disturbing the others.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
tablegold/                 independent module: example.com/tablegold
  go.mod                   go 1.26
  render.go                User, Render(User) []byte
  testdata/
    alice.golden           per-case reference outputs
    bob.golden
    no_email.golden
  cmd/
    demo/
      main.go              renders one case and prints it
  render_test.go           table of cases, t.Run subtests, per-case -update golden
```

Files: `render.go`, `testdata/{alice,bob,no_email}.golden`, `cmd/demo/main.go`, `render_test.go`.
Implement: `Render(User) []byte` producing indented JSON with a trailing newline.
Test: a table of named cases, each compared against `testdata/<name>.golden` in its own `t.Run` subtest; `-update` regenerates all.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/08-table-driven-golden/cmd/demo go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/08-table-driven-golden/testdata
cd go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/08-table-driven-golden
```

### One row, one golden, one subtest

The table-driven pattern you already use for assertions carries over to snapshots
with one addition: each case names a golden file. A case is `{name, input}`; the
golden path is `filepath.Join("testdata", name+".golden")`; the subtest is
`t.Run(name, ...)`. Running each case as a subtest is what makes a table of
snapshots usable at scale — a failure reports `TestGolden/no_email`, not a bare
line number, and the `goldenFile` helper prints the exact golden path, so you know
precisely which case drifted and which file to inspect. The `-update` flag flows
through the shared helper, so `go test -update` regenerates every case's golden in
one run; you then read the aggregate `git diff` before committing.

Two modern-Go details matter here. Since Go 1.22 the loop variable is scoped per
iteration, so there is no `tc := tc` shadowing dance — the `tc` captured by each
subtest closure is that iteration's value even though the subtests could run
later. And the case name doubles as the filename, so keep names filesystem-safe
(lowercase, underscores, no slashes) — a name with a slash would create a nested
directory or, worse, escape `testdata/`.

The cost this pattern trades away is that a case with no golden file fails on the
read rather than silently passing; that is the correct default, because a missing
golden means "nobody approved this output yet," and the fix is a deliberate
`go test -update` after you have looked at what the case produces.

Create `render.go`:

```go
package tablegold

import (
	"encoding/json"
	"fmt"
)

// User is the record each table case renders.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Render returns indented JSON for u with exactly one trailing newline.
func Render(u User) []byte {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("render: marshal: %v", err))
	}
	return append(data, '\n')
}
```

Now the per-case goldens:

Create `testdata/alice.golden`:

```text
{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}
```

Create `testdata/bob.golden`:

```text
{
  "id": "u2",
  "name": "Bob",
  "email": "bob@example.com"
}
```

Create `testdata/no_email.golden`:

```text
{
  "id": "u3",
  "name": "Carol",
  "email": ""
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/tablegold"
)

func main() {
	os.Stdout.Write(tablegold.Render(tablegold.User{ID: "u1", Name: "Alice", Email: "alice@example.com"}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}
```

### Tests

`TestGolden` iterates the table; each case runs as a subtest that renders its input
and compares against `testdata/<name>.golden` through the shared `goldenFile`
helper. A failure names the subtest and the golden path; `-update` rewrites every
case's golden.

Create `render_test.go`:

```go
package tablegold

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

func goldenFile(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
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
		t.Fatalf("golden mismatch for %s (run: go test -update)\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestGolden(t *testing.T) {
	cases := []struct {
		name string
		user User
	}{
		{"alice", User{ID: "u1", Name: "Alice", Email: "alice@example.com"}},
		{"bob", User{ID: "u2", Name: "Bob", Email: "bob@example.com"}},
		{"no_email", User{ID: "u3", Name: "Carol", Email: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			goldenFile(t, tc.name, Render(tc.user))
		})
	}
}
```

## Review

The table-driven golden scales the snapshot pattern the way a table scales
assertions: a case is a row plus a file, a failure names itself, and `-update`
regenerates the whole set. The subtest name doubling as the filename is the ergonomic
core — it is why a failure points at `no_email.golden` directly — and it is also the
constraint: keep names filesystem-safe. Rely on Go 1.22 per-iteration loop scoping
rather than the old `tc := tc` shadow, and remember that a missing golden failing the
read is a feature, not a bug: it forces a deliberate approval for any case nobody has
blessed yet. Run `go test -race`, and when a case drifts, inspect that one golden's
diff rather than reflexively regenerating the table.

## Resources

- [testing: T.Run](https://pkg.go.dev/testing#T.Run) — subtests that name and isolate each table case.
- [Go blog: Using subtests and sub-benchmarks](https://go.dev/blog/subtests) — the table-plus-subtests idiom.
- [path/filepath: Join](https://pkg.go.dev/path/filepath#Join) — mapping a case name to its golden path portably.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-http-handler-approval.md](07-http-handler-approval.md) | Next: [09-error-envelope-snapshot.md](09-error-envelope-snapshot.md)
