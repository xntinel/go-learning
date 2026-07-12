# Exercise 2: A Deterministic go.mod ignore Reconciler

There is no `go mod edit -ignore` flag, so any automation that must keep the
`ignore` set correct — a repo scaffolder, a lint step, a code generator that
learns of a new `generated/` directory — has to edit the directive
programmatically. This exercise builds that tool the right way, with
`golang.org/x/mod/modfile`, so the output is canonical, deduplicated, and
idempotent rather than a hand-maintained block that rots into merge conflicts.

This module is fully self-contained: its own `go mod init`, a `require` on
`x/mod`, a library, a CLI demo, and tests.

## What you'll build

```text
gomodignore/                    independent module: example.com/gomodignore
  go.mod                        go 1.25; require golang.org/x/mod
  reconcile.go                  Reconcile(data, desired) ([]byte, error); IgnorePaths(data)
  cmd/
    demo/
      main.go                   reconciles a fixed go.mod against a desired set, prints it
  reconcile_test.go             table test + idempotency + preservation + Example
```

- Files: `reconcile.go`, `cmd/demo/main.go`, `reconcile_test.go`.
- Implement: `Reconcile` (make the ignore set exactly `desired`, preserve everything else, emit canonical bytes) and `IgnorePaths` (read the current set).
- Test: assert the resulting ignore set equals the desired set, unrelated directives survive, and a second reconcile is byte-identical.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
go get golang.org/x/mod@v0.37.0
```

That yields the module's `go.mod`.

Create `go.mod`:

```go
// go.mod
module example.com/gomodignore

go 1.25

require golang.org/x/mod v0.37.0
```

### Why modfile, not string editing

A `go.mod` is a structured file with ordering rules, block versus single-line
forms, and formatting conventions. Editing it by concatenating strings works
until the second run, when you get duplicates, or until two branches both append
and you get a merge conflict, or until `gofmt`-of-go.mod (`go mod tidy`) reorders
your block and your next diff is enormous. `golang.org/x/mod/modfile` is the same
library the go command uses: `modfile.Parse` returns a `*File` with typed slices
(`Ignore []*Ignore`, each `Ignore{Path, Syntax}`), you mutate through
`AddIgnore`/`DropIgnore`, and `Cleanup` plus `Format` emit canonical bytes. The
mutators are idempotent by contract — `AddIgnore` is a no-op if the path already
exists, `DropIgnore` a no-op if it does not — which is exactly what a reconciler
needs to be safe to run on every build. `AddIgnore` and `DropIgnore` landed in
`x/mod` v0.25.0, so pin at least that.

### The reconcile algorithm

Reconciliation is a set-difference against a desired target, done in two passes so
the result is deterministic regardless of the starting order. First, drop every
existing ignore path that is not wanted. Second, add every wanted path that is not
already present, adding them in sorted order so the emitted block is stable across
runs and machines. Then `Cleanup` removes any structural cruft left by the edits
and `Format` renders the file. Because both mutators are no-ops when there is
nothing to do, running `Reconcile` on its own output with the same desired set
changes nothing and produces identical bytes — the idempotency the test pins down.
`IgnorePaths` is the read side: parse and return the sorted set, used by the test
to assert the outcome without depending on formatting.

Create `reconcile.go`:

```go
package gomodignore

import (
	"fmt"
	"slices"

	"golang.org/x/mod/modfile"
)

// Reconcile parses go.mod bytes, makes the ignore set exactly equal to desired
// (adding missing paths, dropping stale ones), and returns canonical formatted
// bytes. It preserves all other directives and is idempotent: reconciling its
// own output against the same desired set yields byte-identical bytes.
func Reconcile(data []byte, desired []string) ([]byte, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	want := make(map[string]bool, len(desired))
	for _, p := range desired {
		want[p] = true
	}
	for _, ig := range f.Ignore {
		if !want[ig.Path] {
			if err := f.DropIgnore(ig.Path); err != nil {
				return nil, fmt.Errorf("drop ignore %q: %w", ig.Path, err)
			}
		}
	}
	add := make([]string, 0, len(desired))
	for _, p := range desired {
		if !slices.ContainsFunc(f.Ignore, func(ig *modfile.Ignore) bool { return ig.Path == p }) {
			add = append(add, p)
		}
	}
	slices.Sort(add)
	for _, p := range add {
		if err := f.AddIgnore(p); err != nil {
			return nil, fmt.Errorf("add ignore %q: %w", p, err)
		}
	}
	f.Cleanup()
	return f.Format()
}

// IgnorePaths returns the sorted set of ignore paths currently in go.mod bytes.
func IgnorePaths(data []byte) ([]string, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	paths := make([]string, 0, len(f.Ignore))
	for _, ig := range f.Ignore {
		paths = append(paths, ig.Path)
	}
	slices.Sort(paths)
	return paths, nil
}
```

### The CLI demo

The demo shows the whole point: a `go.mod` that carries a stale `ignore ./old_gen`
is reconciled against the directories that actually exist today, dropping the stale
one and adding the current set in canonical order, with the `require` block left
untouched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/gomodignore"
)

func main() {
	before := []byte("module example.com/service\n\ngo 1.25\n\nrequire golang.org/x/mod v0.37.0\n\nignore ./old_gen\n")
	desired := []string{"static", "./generated", "node_modules"}

	after, err := gomodignore.Reconcile(before, desired)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Print("--- reconciled go.mod ---\n")
	fmt.Print(string(after))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
--- reconciled go.mod ---
module example.com/service

go 1.25

require golang.org/x/mod v0.37.0

ignore (
	./generated
	node_modules
	static
)
```

### Tests

`TestReconcile` is table-driven over the four transitions that matter: adding to
an empty file, dropping a stale entry while adding a new one, clearing the whole
set, and leaving an already-correct entry untouched. Each case asserts the
resulting ignore set (via `IgnorePaths`), that the `module` and `go` directives
survive, and that a second `Reconcile` returns byte-identical output — the
idempotency guarantee. `TestReconcilePreservesRequire` pins that a `require` block
is not clobbered. `ExampleReconcile` shows the canonical block form for a fixed
input.

Create `reconcile_test.go`:

```go
package gomodignore

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestReconcile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		desired []string
		want    []string
	}{
		{
			name:    "add to empty",
			in:      "module example.com/app\n\ngo 1.25\n",
			desired: []string{"node_modules", "./generated"},
			want:    []string{"./generated", "node_modules"},
		},
		{
			name:    "drop stale and add missing",
			in:      "module example.com/app\n\ngo 1.25\n\nignore ./old_gen\n",
			desired: []string{"./generated"},
			want:    []string{"./generated"},
		},
		{
			name:    "clear all",
			in:      "module example.com/app\n\ngo 1.25\n\nignore (\n\t./a\n\t./b\n)\n",
			desired: nil,
			want:    nil,
		},
		{
			name:    "keep existing untouched",
			in:      "module example.com/app\n\ngo 1.25\n\nignore ./keep\n",
			desired: []string{"./keep", "static"},
			want:    []string{"./keep", "static"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := Reconcile([]byte(tc.in), tc.desired)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			got, err := IgnorePaths(out)
			if err != nil {
				t.Fatalf("IgnorePaths: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ignore paths = %v, want %v\n%s", got, tc.want, out)
			}
			// Unrelated directives survive.
			if !strings.Contains(string(out), "module example.com/app") ||
				!strings.Contains(string(out), "go 1.25") {
				t.Fatalf("lost module/go directive:\n%s", out)
			}
			// Idempotency: a second reconcile is byte-identical.
			out2, err := Reconcile(out, tc.desired)
			if err != nil {
				t.Fatalf("second Reconcile: %v", err)
			}
			if string(out) != string(out2) {
				t.Fatalf("not idempotent:\n--- first ---\n%s\n--- second ---\n%s", out, out2)
			}
		})
	}
}

func TestReconcilePreservesRequire(t *testing.T) {
	t.Parallel()
	in := "module example.com/app\n\ngo 1.25\n\nrequire golang.org/x/mod v0.37.0\n"
	out, err := Reconcile([]byte(in), []string{"static"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !strings.Contains(string(out), "golang.org/x/mod v0.37.0") {
		t.Fatalf("require directive lost:\n%s", out)
	}
}

func ExampleReconcile() {
	in := []byte("module example.com/app\n\ngo 1.25\n")
	out, _ := Reconcile(in, []string{"node_modules", "./generated"})
	fmt.Print(string(out))
	// Output:
	// module example.com/app
	//
	// go 1.25
	//
	// ignore (
	// 	./generated
	// 	node_modules
	// )
}
```

## Review

The reconciler is correct when its output is a pure function of the input's
non-ignore content plus the desired set, in canonical form. The two proofs are in
the test: `IgnorePaths(out)` equals the sorted desired set in every case, and a
second `Reconcile` is byte-identical, so running it on every build never produces a
spurious diff. If idempotency fails, the usual cause is skipping `Cleanup` before
`Format`, which can leave an empty or malformed block that re-renders differently.

The mistakes to avoid: do not string-append to the block by hand — that is what
produces the duplicates and non-deterministic ordering this tool exists to
prevent; let `AddIgnore`/`DropIgnore` and `Cleanup` own the structure. Do not
forget that the mutators are already idempotent, so you do not need to pre-check
existence before dropping (the code drops only non-wanted paths purely to avoid
churn, not for correctness). And remember this tool edits the directive; it does
not enact it — the go command still needs `go 1.25` and a 1.25+ toolchain to honor
the result, which Exercise 1 verifies.

## Resources

- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse`, `File.Ignore`, `AddIgnore`, `DropIgnore`, `Cleanup`, `Format`.
- [Go Modules Reference: the ignore directive](https://go.dev/ref/mod#go-mod-file-ignore) — the directive this tool writes.
- [Proposal #37724: cmd/go add ignore directive](https://github.com/golang/go/issues/37724) — design history and rationale.

---

Back to [01-monorepo-build-guardrail.md](01-monorepo-build-guardrail.md) | Next: [03-ignore-is-not-a-publish-filter.md](03-ignore-is-not-a-publish-filter.md)
