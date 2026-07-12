# Exercise 8: Reconcile dependency drift between modules with go work sync

Two modules in one workspace can require different versions of a shared
dependency. Inside the workspace, Minimal Version Selection runs over the union
and every member builds against the higher version — so the drift is invisible
until a member is built standalone in CI and resolves the lower one. `go work
sync` pushes the reconciled result back into each member's `go.mod`. This exercise
builds the version-comparison helper that mirrors the MVS max rule the command
applies, using `golang.org/x/mod/semver`.

## What you'll build

```text
mvs/                          module: example.com/mono/mvs
  go.mod                      requires golang.org/x/mod
  mvs.go                      SelectVersion(required), Reconcile(a, b)
  mvs_test.go                 max-rule + divergence-detection tests
  cmd/demo/main.go            reconciles two members' requirements
```

- Files: `mvs.go`, `mvs_test.go`, `cmd/demo/main.go`.
- Implement: `SelectVersion(required []string) string` returning the MVS-selected (maximum) version, and `Reconcile(a, b string) (selected string, diverged bool)`.
- Test: assert `SelectVersion` picks the higher of several requirements (the MVS max rule), ignores invalid input, and `Reconcile` reports divergence when two members disagree.
- Verify: `go test -count=1 -race ./...` (needs `golang.org/x/mod`).

Set up the module:

```bash
go get golang.org/x/mod
```

### What go work sync actually does

MVS selects, for each dependency, the **maximum of the minimum versions** anyone
in the build requires. In a workspace, "anyone" is the union of every member's
requirements. So if `api/go.mod` requires `example.com/lib v1.3.0` and
`worker/go.mod` requires `example.com/lib v1.5.0`, the workspace builds *both*
against `v1.5.0` — the max. Everything is green.

Now build `api` on its own in CI, outside the workspace. Its `go.mod` still says
`v1.3.0`, so standalone it resolves `v1.3.0` — a *different* build than the one you
tested. If `v1.4.0` fixed a bug `api` now depends on transitively, the standalone
build regresses. `go work sync` prevents this: it computes the workspace-wide MVS
result and writes it back into each member's `go.mod`, so `api/go.mod` is bumped to
`v1.5.0` and a later standalone build agrees with the workspace build. You confirm
the reconciliation with `go list -m example.com/lib` in each member (same version
now) and can inspect the full picture with `go list -m all` and `go mod graph`.

The helper here encodes the single rule at the center of that machinery: given a
set of required versions for one dependency, the selected version is their
maximum under semantic-version ordering. `golang.org/x/mod/semver` provides that
ordering — `semver.Compare` and `semver.Max` operate on canonical `vX.Y.Z`
strings, and `semver.IsValid` filters junk. `Reconcile` layers the workspace
question on top: given two members' requirements, what does everyone end up on,
and did they *disagree* to begin with (the drift `go work sync` exists to erase)?

Create `mvs.go`:

```go
package mvs

import "golang.org/x/mod/semver"

// SelectVersion returns the version Minimal Version Selection would choose for a
// single dependency given the versions required across the build: the maximum
// under semantic-version ordering. Invalid entries are ignored; if nothing is
// valid it returns "".
func SelectVersion(required []string) string {
	selected := ""
	for _, v := range required {
		if !semver.IsValid(v) {
			continue
		}
		if selected == "" {
			selected = v
			continue
		}
		selected = semver.Max(selected, v)
	}
	return selected
}

// Reconcile reports the version two workspace members converge on (the MVS max)
// and whether they required different versions to begin with. Divergence is the
// drift that go work sync writes back into each member's go.mod.
func Reconcile(a, b string) (selected string, diverged bool) {
	selected = SelectVersion([]string{a, b})
	diverged = semver.Compare(a, b) != 0
	return selected, diverged
}
```

### The runnable demo

The demo models the `api` / `worker` divergence: `api` requires `v1.3.0`, `worker`
requires `v1.5.0`. It prints the reconciled version both members end up on after
`go work sync`, and that they diverged.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mono/mvs"
)

func main() {
	apiReq := "v1.3.0"
	workerReq := "v1.5.0"

	selected, diverged := mvs.Reconcile(apiReq, workerReq)
	fmt.Printf("api requires:    %s\n", apiReq)
	fmt.Printf("worker requires: %s\n", workerReq)
	fmt.Printf("reconciled:      %s (diverged: %v)\n", selected, diverged)

	all := mvs.SelectVersion([]string{"v1.2.0", "v1.5.0", "v1.4.3", "v1.5.0"})
	fmt.Printf("union of four:   %s\n", all)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
api requires:    v1.3.0
worker requires: v1.5.0
reconciled:      v1.5.0 (diverged: true)
union of four:   v1.5.0
```

### Tests

The tests pin the MVS max rule directly. `TestSelectVersion` asserts the selected
version is the maximum across several requirements regardless of order, that
invalid entries are skipped, and that an all-invalid input yields `""`.
`TestReconcile` proves the divergence flag: two different requirements diverge and
converge on the max, while two identical ones do not diverge.

Create `mvs_test.go`:

```go
package mvs

import "testing"

func TestSelectVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		required []string
		want     string
	}{
		{"single", []string{"v1.2.0"}, "v1.2.0"},
		{"max wins regardless of order", []string{"v1.2.0", "v1.5.0", "v1.4.3"}, "v1.5.0"},
		{"max wins when highest is first", []string{"v2.0.1", "v1.9.9"}, "v2.0.1"},
		{"invalid entries ignored", []string{"latest", "v1.1.0", "garbage"}, "v1.1.0"},
		{"all invalid yields empty", []string{"latest", "main"}, ""},
		{"empty yields empty", nil, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := SelectVersion(tc.required); got != tc.want {
				t.Errorf("SelectVersion(%v) = %q, want %q", tc.required, got, tc.want)
			}
		})
	}
}

func TestReconcile(t *testing.T) {
	t.Parallel()

	selected, diverged := Reconcile("v1.3.0", "v1.5.0")
	if selected != "v1.5.0" {
		t.Errorf("reconciled = %q, want v1.5.0", selected)
	}
	if !diverged {
		t.Error("diverged = false, want true for v1.3.0 vs v1.5.0")
	}

	selected, diverged = Reconcile("v1.5.0", "v1.5.0")
	if selected != "v1.5.0" {
		t.Errorf("reconciled = %q, want v1.5.0", selected)
	}
	if diverged {
		t.Error("diverged = true, want false for identical requirements")
	}
}
```

## Review

The helper is correct when `SelectVersion` returns the semantic-version maximum of
its valid inputs — order-independent, invalid entries skipped — because that is
exactly what MVS computes for one dependency, and what `go work sync` writes back
into each member. `Reconcile` adds the workspace insight: two members that require
different versions have *drifted*, and the workspace silently masks it by building
everyone on the max.

The operational mistake is forgetting `go work sync` after bumping a member's
dependency. The workspace build stays green because union MVS picks the higher
version for everyone; then CI builds a member standalone, its unbumped `go.mod`
selects the lower version, and the build diverges from what you tested. Run
`go work sync` after any member dependency change, and use `go list -m <dep>` in
each member to confirm they agree before you tag.

## Resources

- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — `Compare`, `Max`, and `IsValid` over canonical versions.
- [Go Modules Reference: Minimal version selection](https://go.dev/ref/mod#minimal-version-selection) — the max-of-minimums rule.
- [`cmd/go`: `go work sync`](https://pkg.go.dev/cmd/go#hdr-Sync_workspace_build_list_to_modules) — how the workspace MVS result is written back to members.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-tag-a-subdirectory-module.md](09-tag-a-subdirectory-module.md)
