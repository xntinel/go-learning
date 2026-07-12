# Exercise 1: Keep go build ./... Green in a Mixed Monorepo

The most useful thing you can do with the `ignore` directive is prove it works —
in a test that CI runs. This exercise builds the real on-the-job artifact: a
guardrail test that materializes a mixed monorepo, drives the actual go toolchain
as a subprocess, and asserts that a broken non-Go subtree breaks the wildcards
without `ignore` and is invisible to them with it.

This module is fully self-contained. It has its own `go mod init`, a pure helper
that models the directive's path semantics, a demo, and a test that shells out to
`go` against a throwaway fixture module.

## What you'll build

```text
monorepoguard/                  independent module: example.com/monorepoguard
  go.mod                        go 1.25 (the ignore directive needs it)
  guard.go                      Ignored(pkg, module, dir) bool; FilterIgnored(...) []string
  cmd/
    demo/
      main.go                   prints before/after package counts using FilterIgnored
  guard_test.go                 table test of Ignored; a subprocess guardrail test; an Example
```

- Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
- Implement: `Ignored` (mirrors `./`-anchored vs bare-name path matching) and `FilterIgnored` (drops ignored packages from a list).
- Test: build a fixture module in `t.TempDir()` with a non-compiling `generated/` package, run real `go list/build/vet` as subprocesses, and assert the broken package is present-and-breaking without `ignore`, absent-and-green with it.
- Verify: `go test -count=1 -race ./...`

Set up the module. The directive is a Go 1.25 feature, so pin the language
version:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/08-go-mod-ignore-and-monorepo/01-monorepo-build-guardrail/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/08-go-mod-ignore-and-monorepo/01-monorepo-build-guardrail
go mod edit -go=1.25
```

### Why a subprocess test, and why hermetic

The behavior under test belongs to the `go` command, not to any function you can
call in-process: whether `go build ./...` succeeds is decided by the toolchain
walking a directory tree. So the honest way to test it is to *run the toolchain*.
The test writes a throwaway module into `t.TempDir()` — a buildable `app` package,
a real `internal/store` package, a `generated/` directory whose Go file references
an undefined symbol, and a `node_modules/leftpad` whose Go file does the same —
then runs `go list`, `go build`, and `go vet` against it and inspects the results.

The child processes must be *hermetic* so the assertions do not depend on the
developer's environment. Three environment variables matter. `GOPROXY=off`
forbids any network fetch: the fixture has no dependencies, so it must build from
nothing. `GOFLAGS=` clears any inherited flags (CI often sets `-mod=mod` or
`-mod=vendor`, which would change behavior). `GOTOOLCHAIN=auto` is the subtle one:
the fixture's `go.mod` says `go 1.25`, and the `go` binary on `PATH` may be older;
`auto` lets the child switch to a cached Go 1.25 toolchain rather than erroring.
Do *not* use `GOTOOLCHAIN=local` here — if the `go` on `PATH` predates 1.25 it
will reject the `ignore` line as an unknown directive and the test will fail for
the wrong reason. `hermeticEnv` builds this environment by copying the current one
and overriding exactly those three keys.

### The two path forms, modeled in a pure function

Before the subprocess test, the module ships a pure helper so the directive's path
semantics are testable without shelling out. `Ignored(pkg, module, dir)` reports
whether an import path `pkg` is dropped by one ignore entry `dir`. It reproduces
the two forms exactly: a `dir` beginning with `./` is anchored at the module root
and matches only that subtree (`./generated` drops `module/generated` and below,
but never `module/internal/generated`); a bare `dir` matches a directory of that
name at *any* depth (`node_modules` drops `module/node_modules` and
`module/web/node_modules/leftpad` alike). This is the distinction that bites
teams — a bare name silently ignores more than one place — so it is worth pinning
down in a table test. `FilterIgnored` layers over it to prune a whole package
list, which is what the demo shows.

Create `guard.go`:

```go
package monorepoguard

import "strings"

// Ignored reports whether the package import path pkg is dropped by a single
// ignore-directive entry dir, given the module path. It mirrors the go.mod
// semantics: a dir starting with "./" is anchored at the module root and
// ignores only that subtree; a bare dir matches a directory of that name at any
// depth in the module.
func Ignored(pkg, module, dir string) bool {
	rel := strings.TrimPrefix(pkg, module)
	rel = strings.TrimPrefix(rel, "/")
	if anchored, ok := strings.CutPrefix(dir, "./"); ok {
		anchored = strings.Trim(anchored, "/")
		return rel == anchored || strings.HasPrefix(rel, anchored+"/")
	}
	name := strings.Trim(dir, "/")
	for _, seg := range strings.Split(rel, "/") {
		if seg == name {
			return true
		}
	}
	return false
}

// FilterIgnored returns pkgs with every package dropped by any entry in ignored
// removed, preserving input order.
func FilterIgnored(pkgs []string, module string, ignored []string) []string {
	kept := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		drop := false
		for _, d := range ignored {
			if Ignored(p, module, d) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, p)
		}
	}
	return kept
}
```

### The runnable demo

The demo takes the package list a real `go list ./...` would print for a small
monorepo and shows how the ignore set prunes it: `./generated` drops one package,
the bare `node_modules` drops the nested `leftpad`, leaving the two packages CI
should actually build.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/monorepoguard"
)

func main() {
	pkgs := []string{
		"example.com/app",
		"example.com/app/internal/store",
		"example.com/app/generated",
		"example.com/app/node_modules/leftpad",
	}
	ignored := []string{"./generated", "node_modules"}

	kept := monorepoguard.FilterIgnored(pkgs, "example.com/app", ignored)

	fmt.Printf("packages before ignore: %d\n", len(pkgs))
	fmt.Printf("packages after ignore:  %d\n", len(kept))
	for _, p := range kept {
		fmt.Printf("  build: %s\n", p)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
packages before ignore: 4
packages after ignore:  2
  build: example.com/app
  build: example.com/app/internal/store
```

### The guardrail test

`TestGuardrail` is the artifact a release engineer ships. It is table-driven over
two cases: the fixture without an `ignore` block and with one. In the no-ignore
case it asserts the broken `generated` package appears in `go list ./...` and that
`go build ./...` fails. In the with-ignore case it asserts the package is absent
from `./...`, that `go build` and `go vet` succeed, and — the same result under a
different pattern — that `generated` is absent from `go list all`, which walks the
whole module too. `t.Context()` ties each subprocess to the test's lifetime so a
cancelled test does not leak a `go` process. `TestIgnored` pins the path-matching
semantics; `ExampleFilterIgnored` gives a deterministic, verified illustration.

Create `guard_test.go`:

```go
package monorepoguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIgnored(t *testing.T) {
	t.Parallel()
	const mod = "example.com/app"
	tests := []struct {
		name string
		pkg  string
		dir  string
		want bool
	}{
		{"anchored root match", mod + "/generated", "./generated", true},
		{"anchored subtree match", mod + "/generated/api", "./generated", true},
		{"anchored does not match at depth", mod + "/internal/generated", "./generated", false},
		{"bare matches at root", mod + "/node_modules", "node_modules", true},
		{"bare matches at any depth", mod + "/web/node_modules/leftpad", "node_modules", true},
		{"unrelated package kept", mod + "/internal/store", "./generated", false},
		{"module root never ignored", mod, "./generated", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Ignored(tc.pkg, mod, tc.dir); got != tc.want {
				t.Fatalf("Ignored(%q, %q, %q) = %v, want %v", tc.pkg, mod, tc.dir, got, tc.want)
			}
		})
	}
}

func hermeticEnv() []string {
	drop := map[string]bool{"GOFLAGS": true, "GOTOOLCHAIN": true, "GOPROXY": true}
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			env = append(env, kv)
		}
	}
	return append(env, "GOTOOLCHAIN=auto", "GOPROXY=off", "GOFLAGS=")
}

func runGo(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = dir
	cmd.Env = hermeticEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func materialize(t *testing.T, withIgnore bool) string {
	t.Helper()
	root := t.TempDir()
	write := func(p, body string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gomod := "module example.com/app\n\ngo 1.25\n"
	if withIgnore {
		gomod += "\nignore (\n\t./generated\n\tnode_modules\n)\n"
	}
	write("go.mod", gomod)
	write("app.go", "package app\n\nfunc Hello() string { return \"hello\" }\n")
	write("internal/store/store.go", "package store\n\nfunc Ping() string { return \"pong\" }\n")
	write("generated/broken.pb.go", "package generated\n\nfunc Broken() int { return missingSymbol }\n")
	write("node_modules/leftpad/index.go", "package leftpad\n\nfunc Nope() int { return alsoMissing }\n")
	return root
}

func TestGuardrail(t *testing.T) {
	tests := []struct {
		name       string
		withIgnore bool
		wantBuild  bool
		wantListed bool
	}{
		{"without ignore", false, false, true},
		{"with ignore", true, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := materialize(t, tc.withIgnore)

			listOut, listErr := runGo(t, root, "list", "./...")
			if tc.withIgnore && listErr != nil {
				t.Fatalf("go list ./... failed under ignore: %v\n%s", listErr, listOut)
			}
			listed := slices.Contains(strings.Fields(listOut), "example.com/app/generated")
			if listed != tc.wantListed {
				t.Fatalf("generated listed=%v want %v\n%s", listed, tc.wantListed, listOut)
			}

			buildOut, buildErr := runGo(t, root, "build", "./...")
			if (buildErr == nil) != tc.wantBuild {
				t.Fatalf("go build ./... ok=%v want %v\n%s", buildErr == nil, tc.wantBuild, buildOut)
			}

			if tc.withIgnore {
				if vetOut, vetErr := runGo(t, root, "vet", "./..."); vetErr != nil {
					t.Fatalf("go vet ./... failed under ignore: %v\n%s", vetErr, vetOut)
				}
				allOut, _ := runGo(t, root, "list", "all")
				if slices.Contains(strings.Fields(allOut), "example.com/app/generated") {
					t.Fatalf("generated present in `go list all` despite ignore:\n%s", allOut)
				}
			}
		})
	}
}

func ExampleFilterIgnored() {
	pkgs := []string{
		"example.com/app",
		"example.com/app/internal/store",
		"example.com/app/generated",
		"example.com/app/node_modules/leftpad",
	}
	kept := FilterIgnored(pkgs, "example.com/app", []string{"./generated", "node_modules"})
	for _, p := range kept {
		fmt.Println(p)
	}
	// Output:
	// example.com/app
	// example.com/app/internal/store
}
```

## Review

The guardrail is correct when the two cases diverge exactly at the `ignore` line
and nowhere else: without it, `generated` is listed and `go build ./...` returns a
non-zero exit; with it, `generated` is gone from both `./...` and `all`, and
`build` and `vet` succeed. If the with-ignore case fails to build, the usual cause
is a stale toolchain — the child must reach a Go 1.25+ toolchain, which is why the
env sets `GOTOOLCHAIN=auto`, not `local`. If the no-ignore case unexpectedly
passes, the fixture's broken file is not actually broken (an undefined symbol like
`missingSymbol` is what forces the compile error).

The mistakes to avoid mirror the concepts. Do not use a bare `generated` when you
mean the root one; `TestIgnored` shows the bare form matching at any depth, which
is why the fixture anchors it as `./generated` and only uses the bare form for
`node_modules`. Do not forget `go 1.25` in the fixture's `go.mod`; an older
language line makes the toolchain reject the directive. And keep the child
hermetic: leaving `GOPROXY` or `GOFLAGS` inherited lets the developer's
environment leak into the assertion. Run `go test -race` to confirm the helper and
the subprocess plumbing are clean.

## Resources

- [Go Modules Reference: the ignore directive](https://go.dev/ref/mod#go-mod-file-ignore) — syntax and the `./`-anchored vs bare-name path semantics.
- [Go 1.25 release notes: go command](https://go.dev/doc/go1.25#go-command) — the directive's introduction and the "still included in module zip files" clause.
- [`os/exec`](https://pkg.go.dev/os/exec) — `CommandContext`, `Cmd.Dir`, `Cmd.Env`, `Cmd.CombinedOutput`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-gomod-ignore-reconciler.md](02-gomod-ignore-reconciler.md)
