# Exercise 7: Selecting Package Files with //go:build Constraints

Real services carry environment-specific code: a safe production stub of a secret
source by default, a developer implementation behind a flag, integration tests too
slow for the default run. Build constraints are how you gate which files even
reach the compiler. This exercise builds a `secretsource` package with the same
`Load()` symbol provided by two mutually exclusive files, plus an integration test
that only runs under a tag.

This module is self-contained. Nothing here imports another exercise.

## What you'll build

```text
secretsource/                      module: example.com/secretsource
  go.mod
  secretsource/load_stub.go        //go:build !dev  -> safe default Load()
  secretsource/load_dev.go         //go:build dev   -> developer Load()
  secretsource/load_stub_test.go   //go:build !dev  -> asserts the stub value
  secretsource/load_dev_test.go    //go:build dev   -> asserts the dev value
  secretsource/integration_test.go //go:build integration -> gated slow test
  cmd/demo/main.go                 prints Load()
```

- Files: the seven above.
- Implement: one `Load()` selected by `//go:build !dev`, another by `//go:build dev`; tests gated to match; an integration test behind `//go:build integration`.
- Test: default `go test` asserts the stub; `go test -tags=dev` asserts the dev value; the integration test is skipped without the tag.
- Verify: `go test -count=1 -race ./...` (default), then `go test -tags=dev ./...` and `go test -tags=integration ./...`.

Set up the module:

```bash
mkdir -p ~/go-exercises/secretsource/secretsource ~/go-exercises/secretsource/cmd/demo
cd ~/go-exercises/secretsource
go mod init example.com/secretsource
go mod edit -go=1.26
```

### One symbol, two files, mutually exclusive constraints

Two files in the same package both define `func Load() string`. Ordinarily that is
a duplicate-symbol compile error — but a build constraint makes each file compile
only in a build the other is excluded from, so exactly one is ever present.
`load_stub.go` carries `//go:build !dev` (compiled unless the `dev` tag is set) and
returns a safe placeholder that never touches a real secret manager;
`load_dev.go` carries `//go:build dev` and returns a value a developer wants
locally. The constraints are complementary, so `!dev` and `dev` partition every
build: default builds get the stub, `-tags=dev` builds get the dev version, and no
build ever sees both `Load` definitions.

The `//go:build` line must be the first line of the file, and it must be followed
by a blank line before the `package` clause — `go vet` flags a malformed or
misplaced constraint. (The older `// +build` form is deprecated; use `//go:build`.)

Create `secretsource/load_stub.go`:

```go
//go:build !dev

package secretsource

// Load returns a safe placeholder. This file compiles unless the dev tag is set,
// so it is the production default and never reaches a real secret manager.
func Load() string {
	return "stub-secret"
}
```

Create `secretsource/load_dev.go`:

```go
//go:build dev

package secretsource

// Load returns a developer value. This file compiles only under -tags=dev,
// replacing the stub with the same symbol from a different file.
func Load() string {
	return "dev-secret-from-vault"
}
```

### The demo

The demo prints `Load()`. Built normally it prints the stub; built with
`-tags=dev` it prints the dev value — same source, different file selected by the
constraint.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/secretsource/secretsource"
)

func main() {
	fmt.Println("secret source:", secretsource.Load())
}
```

Run it both ways:

```bash
go run ./cmd/demo
go run -tags=dev ./cmd/demo
```

Expected output:

```text
secret source: stub-secret
```

With `-tags=dev` the second run prints `secret source: dev-secret-from-vault`.

### Tests

Each `Load()` value is asserted by a test gated with the *same* constraint as the
implementation it checks, so the default `go test` run verifies the stub and
`go test -tags=dev` verifies the dev value — neither test ever asserts a value that
is not compiled in. The integration test is behind `//go:build integration`, so it
is excluded from the default run entirely and only compiles and runs under
`-tags=integration` (where it also honors `-short` by skipping).

Create `secretsource/load_stub_test.go`:

```go
//go:build !dev

package secretsource

import "testing"

func TestLoadDefaultsToStub(t *testing.T) {
	if got := Load(); got != "stub-secret" {
		t.Fatalf("Load() = %q, want stub-secret", got)
	}
}
```

Create `secretsource/load_dev_test.go`:

```go
//go:build dev

package secretsource

import "testing"

func TestLoadReturnsDevValue(t *testing.T) {
	if got := Load(); got != "dev-secret-from-vault" {
		t.Fatalf("Load() = %q, want dev-secret-from-vault", got)
	}
}
```

Create `secretsource/integration_test.go`:

```go
//go:build integration

package secretsource

import "testing"

func TestLoadIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	if Load() == "" {
		t.Fatal("Load() returned empty secret")
	}
}
```

## Review

The setup is correct when exactly one `Load` compiles per build: `//go:build !dev`
and `//go:build dev` partition every build so there is never a duplicate symbol,
and the same partition applies to the tests so each asserts only the value that is
actually present. The default `go test` run — the one the gate performs — sees the
stub and its `!dev` test; the dev file, its test, and the integration test are all
excluded. Two traps: forgetting the blank line after `//go:build` (which turns the
directive into an ordinary comment and silently compiles the file everywhere), and
reaching for the deprecated `// +build` form. Run `go vet ./...` to catch a
malformed constraint, then `go test -tags=dev` and `go test -tags=integration` to
exercise the other selections.

## Resources

- [cmd/go: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — `//go:build` syntax, filename suffixes, and `-tags`.
- [`go/build/constraint`](https://pkg.go.dev/go/build/constraint) — the constraint expression grammar `go vet` checks.
- [`testing.Short`](https://pkg.go.dev/testing#Short) — gating slow tests with `-short`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-break-import-cycle-inversion.md](06-break-import-cycle-inversion.md) | Next: [08-embed-migrations-fs.md](08-embed-migrations-fs.md)
