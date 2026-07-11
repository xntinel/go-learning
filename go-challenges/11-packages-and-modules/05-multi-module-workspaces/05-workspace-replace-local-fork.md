# Exercise 5: Patch A Forked Dependency Across Every Module At Once

It is 2 a.m. and a shared third-party dependency has a bug that breaks every
service on the platform. You fork it, fix it locally, and need every module in the
workspace to build against the fork *now* — without touching each module's
`go.mod`. A single `replace` in `go.work` does exactly that: it redirects the
dependency to your local fork for every workspace module simultaneously, and it
evaporates the moment the workspace is off, so nothing about the release changes.

## What you'll build

```text
platform/                      gated module: example.com/platform
  go.mod                       go 1.26
  vendorlib/
    vendorlib.go               package vendorlib; Sanitize (the FORKED, fixed copy)
  consumer/
    consumer.go                package consumer; Normalize uses vendorlib.Sanitize
    consumer_test.go           asserts the forked behavior is what compiled
  cmd/
    demo/
      main.go                  normalizes messy input through the forked dependency
```

- Files: `vendorlib/vendorlib.go`, `consumer/consumer.go`, `consumer/consumer_test.go`, `cmd/demo/main.go`.
- Implement: a fork of `Sanitize` that fixes the upstream bug (it strips *all* leading/trailing whitespace, not just spaces).
- Test: assert the forked behavior (tabs and newlines are trimmed) is what the consumer sees.
- Verify: with the `go.work` replace, every module builds against the fork; removing it reverts to upstream. No network — the fork is a sibling directory.

Set up the gated module:

```bash
mkdir -p ~/platform/vendorlib ~/platform/consumer ~/platform/cmd/demo
cd ~/platform
go mod init example.com/platform
go mod edit -go=1.26
```

### One replace for the whole workspace

The buggy upstream dependency is `example.com/vendorlib`. Its `Sanitize` trims
only ASCII spaces, so a value ending in a tab or newline slips through and
corrupts downstream keys. Every service imports it. Rather than add a `replace` to
each service's `go.mod` (and risk committing one), you fork the dependency into a
sibling directory and redirect it once, at the workspace level:

```bash
# fork the dependency into the monorepo and fix it there
git clone https://example.com/vendorlib ./fork/vendorlib   # then edit the fix

# redirect every workspace module to the fork with ONE directive
go work edit -replace=example.com/vendorlib=./fork/vendorlib
```

That writes a wildcard `replace` into `go.work`:

```text
go 1.26

use (
	./text
	./services/greeter
	./services/billing
)

replace example.com/vendorlib => ./fork/vendorlib
```

Now `./text`, `./services/greeter`, and `./services/billing` all build against the
fork with no change to any of their `go.mod` files. Two properties make this the
right incident tool. First, a *wildcard* `replace` in `go.work` (no version on the
left) overrides any version-specific `replace` an individual `go.mod` might carry —
the workspace wins. Second, the redirect is scoped to the workspace: run
`GOWORK=off go build`, or delete the `replace` with
`go work edit -dropreplace=example.com/vendorlib`, and every module reverts to the
real upstream. Contrast the per-module alternative,
`go mod edit -replace=example.com/vendorlib=../fork/vendorlib`, which you would
have to add — and later remember to remove — in every single module's `go.mod`.

The gated artifact ships the *forked* (fixed) `Sanitize` as the local copy the
consumer builds against, so the test observes the fix. The buggy upstream behavior
is described but not compiled, exactly as the real fork replaces it.

Create `vendorlib/vendorlib.go` — the fork, with the fix:

```go
// vendorlib/vendorlib.go
package vendorlib

import "strings"

// Sanitize trims surrounding whitespace from s. The FORKED behavior strips all
// Unicode whitespace (spaces, tabs, newlines); the buggy upstream trimmed only
// ASCII spaces, letting a trailing tab or newline through.
func Sanitize(s string) string {
	return strings.TrimSpace(s)
}
```

Create `consumer/consumer.go`:

```go
// consumer/consumer.go
package consumer

import "example.com/platform/vendorlib"

// Normalize cleans a user-supplied key using the shared dependency. Under the
// workspace replace it runs the fork, so tabs and newlines are stripped.
func Normalize(key string) string {
	return vendorlib.Sanitize(key)
}
```

### The demo

The demo feeds input with a trailing tab and newline through the consumer; the
fork strips them, so the printed value has visible bounds around clean text.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/platform/consumer"
)

func main() {
	raw := "\torder-42\n"
	fmt.Printf("normalized: %q\n", consumer.Normalize(raw))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
normalized: "order-42"
```

### Tests

The test pins the forked behavior: a value wrapped in tabs and newlines comes back
clean. If the workspace replace were removed and the build fell back to the buggy
upstream, the tab-trailing case would fail — which is precisely the regression the
fork exists to fix.

Create `consumer/consumer_test.go`:

```go
// consumer/consumer_test.go
package consumer

import (
	"fmt"
	"testing"
)

func TestNormalizeUsesFork(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trailing tab", "order-42\t", "order-42"},
		{"surrounding newlines", "\norder-42\n", "order-42"},
		{"leading spaces and tab", "  \tvalue", "value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Normalize(tc.in); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q (fork not active?)", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleNormalize() {
	fmt.Printf("%q\n", Normalize("\tapi-key\n"))
	// Output: "api-key"
}
```

## Review

The incident pattern is: fork, fix, and redirect the whole workspace with one
`go work edit -replace`, rather than editing every module's `go.mod`. Two facts
make it safe. A wildcard `go.work` replace overrides version-specific `go.mod`
replaces, so you do not fight per-module directives during the incident. And the
override is workspace-scoped: `GOWORK=off` or `-dropreplace` reverts every module
to upstream at once, so a downstream consumer of your released module never sees
the fork — the release still depends on a real, tagged version. The test proves
the fork is what compiled by asserting the fixed behavior (tabs and newlines
trimmed); the moment the build resolves the upstream copy instead, those rows
fail. Do not leave the `replace` as the permanent fix — land a tagged upstream
release and drop the workspace redirect once it is available.

## Resources

- [Go Modules Reference — Workspaces](https://go.dev/ref/mod#workspaces) — `replace` in `go.work` and how a wildcard replace overrides `go.mod` replaces.
- [go command — Workspace maintenance](https://pkg.go.dev/cmd/go#hdr-Workspace_maintenance) — `go work edit -replace` and `-dropreplace`.
- [`strings.TrimSpace`](https://pkg.go.dev/strings#TrimSpace) — the Unicode-whitespace trimming the fork uses.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-go-work-sync-ci-parity.md](06-go-work-sync-ci-parity.md)
