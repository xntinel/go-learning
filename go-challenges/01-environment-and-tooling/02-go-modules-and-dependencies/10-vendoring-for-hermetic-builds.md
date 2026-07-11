# Exercise 10: Vendoring for Hermetic, Proxy-Free Builds

Some builds must never touch the network: airgapped CI, a regulated audit, a
release you must be able to reproduce byte-for-byte years later. `go mod vendor`
copies every dependency's source into the repository and lets the toolchain build
from that tree alone. This exercise produces a vendor tree and proves the build is
hermetic.

This module is fully self-contained: its own `go mod init`, all code inline, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
vendored/                   independent module: example.com/vendored
  go.mod                    require golang.org/x/text
  slugify.go                Slugify using cases + norm-style folding
  cmd/
    demo/
      main.go               slugifies a few titles
  slugify_test.go           table over slugification
  (vendor/                  produced by go mod vendor; not checked in here)
```

- Files: `slugify.go`, `cmd/demo/main.go`, `slugify_test.go`.
- Implement: `Slugify(s) string` that folds case and joins words with hyphens.
- Test: table over multi-word and mixed-case titles.
- Verify: `go mod vendor`, inspect `vendor/modules.txt`, build/test with `-mod=vendor` and `GOPROXY=off`, prove idempotency.

Set up the module:

```bash
mkdir -p ~/go-exercises/vendored/cmd/demo
cd ~/go-exercises/vendored
go mod init example.com/vendored
go get golang.org/x/text/cases
```

### The service code

`Slugify` lowercases (folds) a title and joins its words with hyphens — the kind of
helper that turns "The Go Programming Language" into a URL slug.

Create `slugify.go`:

```go
package vendored

import (
	"strings"

	"golang.org/x/text/cases"
)

// Slugify returns a lowercased, hyphen-joined slug of s. Case folding uses
// golang.org/x/text so the behavior is Unicode-correct.
func Slugify(s string) string {
	folded := cases.Fold().String(strings.TrimSpace(s))
	return strings.Join(strings.Fields(folded), "-")
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/vendored"
)

func main() {
	for _, title := range []string{"The Go Programming Language", "Hello   World"} {
		fmt.Printf("%s -> %s\n", title, vendored.Slugify(title))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
The Go Programming Language -> the-go-programming-language
Hello   World -> hello-world
```

`strings.Fields` collapses runs of whitespace, so the double space in the second
title yields a single hyphen.

### The test

Create `slugify_test.go`:

```go
package vendored

import "testing"

func TestSlugify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "title cased", input: "The Go Programming Language", want: "the-go-programming-language"},
		{name: "collapses spaces", input: "Hello   World", want: "hello-world"},
		{name: "trims edges", input: "  Edge Case  ", want: "edge-case"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Slugify(tc.input); got != tc.want {
				t.Fatalf("Slugify(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
```

Run the gate:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

### Producing and proving a hermetic build

`go mod vendor` copies the source of every module in the build list into `vendor/`
and writes `vendor/modules.txt`, a manifest that lists each module, its version,
its `## explicit` / go-directive metadata, and the exact packages vendored:

```bash
go mod vendor
cat vendor/modules.txt
```

```text
# golang.org/x/text v0.38.0
## explicit; go 1.25.0
golang.org/x/text/cases
golang.org/x/text/internal
golang.org/x/text/internal/language
golang.org/x/text/internal/language/compact
golang.org/x/text/internal/tag
golang.org/x/text/language
golang.org/x/text/transform
golang.org/x/text/unicode/norm
```

Only the packages your build actually imports are copied, not the whole module —
which is why a vendored tree is often much smaller than the module cache. When a
`vendor/` directory is present and consistent with `go.mod`, the toolchain selects
`-mod=vendor` automatically and builds *only* from that tree. Prove it is hermetic
by turning the proxy off entirely:

```bash
GOPROXY=off go build -mod=vendor ./...
GOPROXY=off go test -mod=vendor ./...
```

With `GOPROXY=off` the toolchain cannot fetch anything; the build succeeds anyway
because every byte it needs is under `vendor/`. That is the airgapped-CI guarantee.
`go mod vendor` is idempotent — running it again after no dependency change leaves
the tree byte-identical:

```bash
go mod vendor
git diff --stat vendor/
```

A clean `git diff` is the proof. The failure mode to guard against is the reverse:
change a dependency and forget to re-run `go mod vendor`. Then `vendor/` drifts
from `go.mod`, and a `-mod=vendor` build either uses stale code or fails the
consistency check with "inconsistent vendoring". Treat a dependency change and its
`go mod vendor` as one atomic commit, and treat a dirty `vendor/` diff in review as
a blocker.

### When vendoring earns its keep

Vendoring trades repository size and a maintenance step for three things:
airgapped/offline builds, a build that does not depend on any proxy being up, and a
dependency tree you can grep and audit in-tree during review. For a regulated or
airgapped backend those are worth it. For most services, a committed `go.sum` plus
the module proxy already gives reproducibility without the vendor tree, so do not
vendor by reflex — vendor when the offline or audit requirement is real.

## Review

The build is hermetic when `GOPROXY=off go build -mod=vendor ./...` succeeds and
`vendor/modules.txt` lists exactly the modules `go list -m all` reports. The
mistakes to avoid: committing a `vendor/` tree that has drifted from `go.mod` (run
`go mod vendor` after every dependency change and verify a clean diff); assuming
`-mod=vendor` is always on (it is auto-selected only when a consistent `vendor/`
exists — otherwise the build uses the cache/proxy); and vendoring reflexively when
a committed `go.sum` already delivers the reproducibility you need. Use
`GOPROXY=off` in the vendored CI job so a drifted tree fails loudly instead of
silently falling back to the network.

## Resources

- [go mod vendor](https://go.dev/ref/mod#go-mod-vendor) — what is copied and how `-mod=vendor` is selected.
- [vendor/modules.txt](https://go.dev/ref/mod#vendoring) — the manifest format and the consistency check.
- [build commands and -mod](https://go.dev/ref/mod#build-commands) — `-mod=vendor` and `GOPROXY=off` semantics.

---

Back to [09-supply-chain-security-gate.md](09-supply-chain-security-gate.md) | Next: [../03-go-workspace-and-project-layout/00-concepts.md](../03-go-workspace-and-project-layout/00-concepts.md)
