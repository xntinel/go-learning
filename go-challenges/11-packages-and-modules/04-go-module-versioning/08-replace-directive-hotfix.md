# Exercise 8: Emergency-patch an upstream dependency with replace

Upstream ships a security bug and has not cut a fix yet. You cannot wait, and you
cannot edit their code in your `go/pkg/mod` cache. The standard hotfix is a local
fork wired in with `replace`: the `require` line stays pinned to the vulnerable
version, but the build compiles your patched `./fork` instead. Here you model the
buggy upstream, the patched fork, and the app that uses it, and you prove the
replace wiring by parsing the app's `go.mod`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
hotfix/                     independent module: example.com/hotfix
  go.mod
  app.go                    SafeJoin() using the patched fork; ErrUnsafePath
  upstream/
    safepath.go             the BUGGY upstream: accepts path traversal
  fork/
    safepath.go             the PATCHED fork: rejects "../" traversal
  cmd/
    demo/
      main.go               runnable: buggy vs patched vs app
  app_test.go               upstream buggy, fork patched, app safe, replace wiring
```

- Files: `app.go`, `upstream/safepath.go`, `fork/safepath.go`, `cmd/demo/main.go`, `app_test.go`.
- Implement: a buggy `upstream.Clean` that accepts `../` traversal, a patched `fork.Clean` that rejects it, and `SafeJoin(base, name)` using the fork.
- Test: assert the upstream accepts traversal, the fork rejects it, the app is safe, and `modfile` confirms the app `go.mod`'s `replace` maps the upstream path to `./fork` with the `require` unchanged.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/04-go-module-versioning/08-replace-directive-hotfix/upstream go-solutions/11-packages-and-modules/04-go-module-versioning/08-replace-directive-hotfix/fork go-solutions/11-packages-and-modules/04-go-module-versioning/08-replace-directive-hotfix/cmd/demo
cd go-solutions/11-packages-and-modules/04-go-module-versioning/08-replace-directive-hotfix
go mod edit -go=1.26
```

### How replace rewrites the build without touching the require

In the real layout there are two modules: the app, whose source imports
`example.com/upstream/safepath` and whose `go.mod` still says `require
example.com/upstream/safepath v1.4.0`, and a local `./fork` directory containing a
patched copy. One line does the redirection:

```
replace example.com/upstream/safepath => ./fork
```

Now `go build` resolves every import of `example.com/upstream/safepath` to the code
in `./fork`. The import statements in the app do not change — that is the point; you
are not rewriting call sites, you are substituting the module the compiler pulls the
package from. The `require` line stays pinned to the vulnerable `v1.4.0` as a record
of what you are patching *against*, and the `replace` overrides where the bytes come
from. When upstream finally releases `v1.4.1`, you delete the `replace`, bump the
`require`, and the fork is gone.

The property that makes `replace` a *hotfix* tool and not a shipping mechanism: it
takes effect only in the *main* module's `go.mod` and is ignored when your module is
consumed as a dependency. A downstream consumer of your app gets the unpatched
upstream unless they add their own replace. So a `replace => ./local` is safe for an
application you deploy, dangerous in a library you publish, and never a substitute
for landing the fix upstream.

This exercise gates as one module, so the app imports the fork package directly to
demonstrate the patched behavior end to end; the `replace` wiring itself is proven by
parsing a fixture app `go.mod` with `modfile` and confirming it maps the upstream
path to `./fork` while the `require` stays put. Both halves of the real setup are
exercised — the compiled-in patched code, and the directive that would wire it.

The bug is a path-traversal hole, a real and common one: `Clean` is supposed to
reject an upload name that escapes its base directory, and the buggy upstream forgets
the `..` check.

Create `upstream/safepath.go` (the buggy dependency):

```go
package safepath

import "strings"

// Clean is the BUGGY upstream: it strips a leading slash but fails to reject
// parent-directory traversal, so "../etc/passwd" is wrongly reported safe.
func Clean(name string) (string, bool) {
	return strings.TrimPrefix(name, "/"), true
}
```

Create `fork/safepath.go` (the patched fork — same package name, drop-in):

```go
package safepath

import "strings"

// Clean is the PATCHED fork: it rejects any path element equal to "..", closing
// the traversal hole while leaving legitimate paths untouched.
func Clean(name string) (string, bool) {
	trimmed := strings.TrimPrefix(name, "/")
	for _, part := range strings.Split(trimmed, "/") {
		if part == ".." {
			return "", false
		}
	}
	return trimmed, true
}
```

Create `app.go` (imports the fork, as `replace` would wire it):

```go
package hotfix

import (
	"errors"
	"fmt"

	"example.com/hotfix/fork"
)

// ErrUnsafePath is returned when an upload name attempts directory traversal.
var ErrUnsafePath = errors.New("hotfix: unsafe upload path")

// SafeJoin validates an upload name with the patched safepath fork (wired in via
// the replace directive) and joins it under base, or returns ErrUnsafePath.
func SafeJoin(base, name string) (string, error) {
	clean, ok := safepath.Clean(name)
	if !ok {
		return "", fmt.Errorf("hotfix: %q: %w", name, ErrUnsafePath)
	}
	return base + "/" + clean, nil
}
```

### The runnable demo

The demo shows all three in one run: the buggy upstream accepts the attack, the
patched fork rejects it, and the app — compiled against the fork — blocks it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/hotfix"
	fork "example.com/hotfix/fork"
	upstream "example.com/hotfix/upstream"
)

func main() {
	const attack = "../etc/passwd"

	if _, ok := upstream.Clean(attack); ok {
		fmt.Println("upstream (buggy): accepts", attack)
	}
	if _, ok := fork.Clean(attack); !ok {
		fmt.Println("fork (patched): rejects", attack)
	}

	if _, err := hotfix.SafeJoin("/data", attack); errors.Is(err, hotfix.ErrUnsafePath) {
		fmt.Println("app (replace -> fork): blocked traversal")
	}
	path, _ := hotfix.SafeJoin("/data", "reports/q1.csv")
	fmt.Println("app: safe join ->", path)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
upstream (buggy): accepts ../etc/passwd
fork (patched): rejects ../etc/passwd
app (replace -> fork): blocked traversal
app: safe join -> /data/reports/q1.csv
```

### Tests

The tests prove the bug exists upstream, is fixed in the fork, and is absent from the
app; then they parse a fixture app `go.mod` and confirm the `replace` maps the
upstream path to `./fork` while the `require` remains pinned.

Create `app_test.go`:

```go
package hotfix

import (
	"errors"
	"testing"

	fork "example.com/hotfix/fork"
	upstream "example.com/hotfix/upstream"
	"golang.org/x/mod/modfile"
)

const appGoMod = `module example.com/app

go 1.26

require example.com/upstream/safepath v1.4.0

replace example.com/upstream/safepath => ./fork
`

func TestUpstreamIsBuggy(t *testing.T) {
	t.Parallel()
	if _, ok := upstream.Clean("../etc/passwd"); !ok {
		t.Fatal("expected the buggy upstream to (wrongly) accept traversal")
	}
}

func TestForkIsPatched(t *testing.T) {
	t.Parallel()
	if _, ok := fork.Clean("../etc/passwd"); ok {
		t.Fatal("patched fork accepted a traversal path")
	}
	if clean, ok := fork.Clean("reports/q1.csv"); !ok || clean != "reports/q1.csv" {
		t.Fatalf("fork.Clean(reports/q1.csv) = %q,%v; want reports/q1.csv,true", clean, ok)
	}
}

func TestAppUsesPatchedFork(t *testing.T) {
	t.Parallel()
	if _, err := SafeJoin("/data", "../etc/passwd"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("SafeJoin traversal err = %v, want ErrUnsafePath", err)
	}
	got, err := SafeJoin("/data", "reports/q1.csv")
	if err != nil {
		t.Fatalf("SafeJoin safe path: %v", err)
	}
	if got != "/data/reports/q1.csv" {
		t.Fatalf("SafeJoin = %q, want /data/reports/q1.csv", got)
	}
}

func TestReplaceWiring(t *testing.T) {
	t.Parallel()
	f, err := modfile.Parse("go.mod", []byte(appGoMod), nil)
	if err != nil {
		t.Fatalf("parse app go.mod: %v", err)
	}
	if len(f.Replace) != 1 {
		t.Fatalf("got %d replaces, want 1", len(f.Replace))
	}
	rep := f.Replace[0]
	if rep.Old.Path != "example.com/upstream/safepath" {
		t.Fatalf("replace Old.Path = %q, want example.com/upstream/safepath", rep.Old.Path)
	}
	if rep.New.Path != "./fork" || rep.New.Version != "" {
		t.Fatalf("replace New = %q %q, want ./fork with no version", rep.New.Path, rep.New.Version)
	}
	// The fix rides entirely on the replace; the require stays pinned to v1.4.0.
	if len(f.Require) != 1 || f.Require[0].Mod.Version != "v1.4.0" {
		t.Fatalf("require = %+v, want the original v1.4.0 unchanged", f.Require)
	}
}
```

## Review

The setup is correct when the app, compiled against the fork, blocks the traversal
that the untouched upstream still accepts — that is `replace` doing its job, swapping
the module the compiler reads without changing a single import in the app. The
`TestReplaceWiring` assertion pins the two facts that make it a hotfix and not a
rewrite: the `replace` points the upstream path at `./fork` with no version (a
filesystem replacement), and the `require` stays pinned to the vulnerable `v1.4.0`.
The trap is treating `replace => ./fork` as a durable fix: it is ignored when your
module is consumed downstream, so it is a bridge until upstream releases `v1.4.1`,
never the shipped contract for a library.

## Resources

- [Go Modules Reference: replace directive](https://go.dev/ref/mod#go-mod-file-replace) — syntax and the main-module-only scope.
- [`modfile.Replace`](https://pkg.go.dev/golang.org/x/mod/modfile#Replace) — the parsed `Old`/`New` structure.
- [`go mod edit`](https://go.dev/ref/mod#go-mod-edit) — `-replace` to add the directive from CI or a script.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-mvs-version-resolver.md](09-mvs-version-resolver.md)
