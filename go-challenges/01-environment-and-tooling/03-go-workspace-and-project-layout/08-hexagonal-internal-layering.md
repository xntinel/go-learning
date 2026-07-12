# Exercise 8: A Layered internal/ Subtree for a Hexagonal Service

In a real backend service the `internal/` subtree carries the architecture. This
exercise builds a hexagonal (ports-and-adapters) decomposition: `internal/core`
holds domain types and port *interfaces* and imports zero infrastructure;
`internal/adapters` implements those ports, depending inward on core;
`internal/platform` holds cross-cutting helpers; and `cmd/server` wires them. The
direction of the import edges *is* the design, and the test proves that direction
by asserting `core`'s dependency closure contains no adapter and no third-party
package.

This module is fully self-contained: its own `go mod init`, the three layers, a
wiring binary, a demo, and a dependency-direction test.

## What you'll build

```text
service/                       module github.com/example/service
  go.mod                       go 1.24
  internal/
    core/
      core.go                  Greeting, Greeter port, Service (imports only stdlib)
      core_test.go             Service against a fake Greeter; deps-direction check
    adapters/
      adapters.go              PrefixGreeter implements core.Greeter
    platform/
      platform.go              cross-cutting helper (banner)
  cmd/
    server/main.go             wires adapters + platform into core
    demo/main.go               runnable: same wiring, prints a greeting
```

- Files: `internal/core/core.go`, `internal/core/core_test.go`, `internal/adapters/adapters.go`, `internal/platform/platform.go`, `cmd/server/main.go`, `cmd/demo/main.go`.
- Implement: `core.Greeter` (port interface), `core.Service` depending on the port; `adapters.PrefixGreeter` satisfying it; `platform.Banner`; a `cmd/server` that wires them.
- Test: `Service` exercised against a fake `Greeter` (no adapter import), plus `TestCoreHasNoInfraDeps` running `go list -deps ./internal/core` and asserting the closure has no `internal/adapters`, no `internal/platform`, and no non-stdlib path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/08-hexagonal-internal-layering/internal/core go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/08-hexagonal-internal-layering/internal/adapters go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/08-hexagonal-internal-layering/internal/platform go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/08-hexagonal-internal-layering/cmd/server go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/08-hexagonal-internal-layering/cmd/demo
cd go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/08-hexagonal-internal-layering
go mod edit -go=1.24
```

### Import direction is the architecture

Hexagonal architecture inverts the usual dependency: the domain does not depend
on infrastructure; infrastructure depends on the domain. `internal/core` defines
the domain type `Greeting`, a *port* — the `Greeter` interface — and a `Service`
that takes a `Greeter` and orchestrates it. Critically, `core` imports only the
standard library: it names the *interface* it needs, not any concrete database or
HTTP client. `internal/adapters` then provides `PrefixGreeter`, a concrete type
that satisfies `core.Greeter`, and it imports `core` (inward). `internal/platform`
holds cross-cutting helpers — here a `Banner` — used by the wiring layer.
`cmd/server` is the composition root: it constructs the adapter and the service
and connects them. Nothing depends on `cmd`.

The reason to enforce the direction, not just draw it, is that the compiler will
happily let `core` import `adapters` — the `internal/` rule does not police
intra-module layers. So `core` is testable in isolation only as long as its
import closure stays clean. This exercise asserts that: `TestCoreHasNoInfraDeps`
runs `go list -deps ./internal/core`, which prints the full transitive dependency
set, and fails if any entry is the adapters package, the platform package, or any
path outside the standard library (detected as any import path whose first segment
contains a dot, i.e. a domain name). If a future edit makes `core` reach for an
adapter, the closure changes and the test breaks — the architecture is guarded by
CI, not by memory. The next exercise generalizes this into a full walk over every
package; this one proves the single most important edge.

Create `internal/core/core.go`:

```go
package core

import "fmt"

// Greeting is a domain value: the result the service produces.
type Greeting struct {
	Audience string
	Text     string
}

// Greeter is a port: the capability the core needs from the outside world,
// expressed as an interface so the core depends on no concrete adapter.
type Greeter interface {
	Greet(name string) string
}

// Service orchestrates the domain using an injected Greeter. It has no
// knowledge of how greetings are actually produced.
type Service struct {
	greeter Greeter
}

// NewService wires a Greeter into the service (dependency inversion: the
// concrete implementation is passed in, not imported).
func NewService(g Greeter) *Service {
	return &Service{greeter: g}
}

// Welcome produces a Greeting for name.
func (s *Service) Welcome(name string) Greeting {
	return Greeting{
		Audience: name,
		Text:     fmt.Sprintf("welcome, %s", s.greeter.Greet(name)),
	}
}
```

Create `internal/adapters/adapters.go`:

```go
package adapters

import (
	"fmt"

	"github.com/example/service/internal/core"
)

// PrefixGreeter is an adapter: a concrete implementation of the core.Greeter
// port. It depends inward on core; core does not depend on it.
type PrefixGreeter struct {
	Prefix string
}

// Greet satisfies core.Greeter.
func (p PrefixGreeter) Greet(name string) string {
	return fmt.Sprintf("%s%s", p.Prefix, name)
}

// Compile-time assertion that the adapter satisfies the port.
var _ core.Greeter = PrefixGreeter{}
```

Create `internal/platform/platform.go`:

```go
package platform

import "fmt"

// Banner is a cross-cutting helper used by the composition root, not by the
// domain. It has no dependency on core or adapters.
func Banner(name string) string {
	return fmt.Sprintf("=== %s ===", name)
}
```

Create `cmd/server/main.go` — the composition root that wires the layers:

```go
package main

import (
	"fmt"

	"github.com/example/service/internal/adapters"
	"github.com/example/service/internal/core"
	"github.com/example/service/internal/platform"
)

func main() {
	fmt.Println(platform.Banner("service"))
	svc := core.NewService(adapters.PrefixGreeter{Prefix: "Mr. "})
	g := svc.Welcome("Gopher")
	fmt.Printf("%s -> %s\n", g.Audience, g.Text)
}
```

### The runnable demo

The demo performs the same wiring and prints the produced greeting, showing the
composition root connecting an adapter to the core.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"github.com/example/service/internal/adapters"
	"github.com/example/service/internal/core"
	"github.com/example/service/internal/platform"
)

func main() {
	fmt.Println(platform.Banner("demo"))
	svc := core.NewService(adapters.PrefixGreeter{Prefix: "Dr. "})
	g := svc.Welcome("Ada")
	fmt.Printf("audience=%s text=%q\n", g.Audience, g.Text)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== demo ===
audience=Ada text="welcome, Dr. Ada"
```

### Tests

The first test exercises `Service` against a *fake* `Greeter` defined in the test
— proving the core needs no adapter to be tested. The second asserts the
dependency direction by inspecting `core`'s transitive closure.

Create `internal/core/core_test.go`:

```go
package core

import (
	"os/exec"
	"strings"
	"testing"
)

// fakeGreeter is a test double for the port; the core is tested with no adapter.
type fakeGreeter struct{}

func (fakeGreeter) Greet(name string) string { return "test-" + name }

func TestServiceWelcome(t *testing.T) {
	t.Parallel()

	svc := NewService(fakeGreeter{})
	got := svc.Welcome("Grace")

	if got.Audience != "Grace" {
		t.Errorf("Audience = %q, want %q", got.Audience, "Grace")
	}
	if want := "welcome, test-Grace"; got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
}

const modulePath = "github.com/example/service"

func TestCoreHasNoInfraDeps(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}

	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		if strings.Contains(dep, "/internal/adapters") {
			t.Errorf("core must not depend on adapters, found: %s", dep)
		}
		if strings.Contains(dep, "/internal/platform") {
			t.Errorf("core must not depend on platform, found: %s", dep)
		}
		// In-module packages (including core itself) are expected; the point is
		// that nothing OUTSIDE the module and outside the standard library leaks
		// in. A first path segment containing a dot marks a domain-named module.
		if strings.HasPrefix(dep, modulePath) {
			continue
		}
		if first, _, _ := strings.Cut(dep, "/"); strings.Contains(first, ".") {
			t.Errorf("core must depend only on the standard library, found: %s", dep)
		}
	}
}
```

## Review

The layering is correct when `core` compiles and tests with no adapter in sight:
`Service` is driven by a fake `Greeter` in the test, and its dependency closure is
pure standard library. `TestCoreHasNoInfraDeps` is what makes that a guarantee
rather than a convention — it fails the moment `core` imports the adapters or
platform package, or any third-party module. The design mistake to avoid is
letting the domain reach outward "just this once" for a concrete type; express the
need as a port interface in `core` and satisfy it in `adapters`, so the arrow
always points inward. This exercise guards one edge explicitly; the next turns the
same idea into a rule engine over every package. Run `go test -race ./...` to
confirm.

## Resources

- [Organizing a Go module](https://go.dev/doc/modules/layout) — official guidance on `internal/` subtrees and package boundaries.
- [`go list -deps`](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules) — printing a package's transitive dependency closure.
- [Accept interfaces, return structs (Go Code Review Comments)](https://go.dev/wiki/CodeReviewComments#interfaces) — where to define the port interface (the consumer, i.e. core).

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-architecture-guard-test.md](09-architecture-guard-test.md)
