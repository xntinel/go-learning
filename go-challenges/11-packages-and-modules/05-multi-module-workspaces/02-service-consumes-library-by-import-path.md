# Exercise 2: Wire A Service To The Library Through A Workspace

A service consumes the shared library by its published import path, not by a
relative file path. During development the workspace resolves that import path to
your local library copy, so an edit to the library is visible to the service on
the next build with nothing tagged. This exercise builds the service, composes it
over the library, and shows the `go.work` that makes the local resolution happen.

The gated artifact here is a single module whose packages share the
`example.com/platform/...` prefix, so the import path the service uses reads
*identically* to the real two-module split shown alongside it. That is the key
insight: the import path does not change when `text` becomes a separate module —
only the resolution mechanism (a `use` directive) does.

## What you'll build

```text
platform/                      gated module: example.com/platform
  go.mod                       go 1.26
  text/
    text.go                    package text; Greet
  service/
    service.go                 package service; Greeting composes text.Greet
    service_test.go            asserts the composed behavior
  cmd/
    demo/
      main.go                  service entrypoint: reads os.Args like a real binary
```

- Files: `text/text.go`, `service/service.go`, `service/service_test.go`, `cmd/demo/main.go`.
- Implement: `service.Greeting(name string) string` that calls `text.Greet`, imported by path.
- Test: assert `Greeting("ops") == "Hello, ops"` — the service composed over the library.
- Verify: `go build ./...` and `go test ./...` resolve the library locally with no network fetch.

Set up the gated module:

```bash
mkdir -p ~/platform/service ~/platform/text ~/platform/cmd/demo
cd ~/platform
go mod init example.com/platform
go mod edit -go=1.26
```

### The real two-module layout, and the workspace that ties it

In production these are two independent modules with independent release tags. The
directory tree and its `go.work` look like this:

```text
platform-monorepo/
  go.work                       ties the two modules together for local dev
  text/
    go.mod                      module example.com/platform/text
    text.go
  greeter/
    go.mod                      module example.com/platform/greeter
    service.go                  imports "example.com/platform/text"
```

The workspace root holds a `go.work` created with `go work init ./text ./greeter`:

```text
go 1.26

use (
	./text
	./greeter
)
```

With that `go.work` active, the `greeter` module's `import "example.com/platform/text"`
resolves to the sibling `./text` directory on disk — not the module proxy, not a
tagged version. Edit `text/text.go`, rebuild `greeter`, and the change is live
immediately; you never `go get` a new version and never add a `replace` to
`greeter/go.mod`. That is the entire value proposition of a workspace: local,
uncommitted, cross-module resolution.

The gated code below collapses the two modules into one module,
`example.com/platform`, with `text` and `service` as subpackages. Because the
subpackage import path is `example.com/platform/text` — the same string as the
separate module's path — the service source is byte-for-byte what it would be in
the two-module layout. Only the resolution mechanism differs: a subpackage of one
module here, a `use` directive over two modules in production.

Create `text/text.go`:

```go
// text/text.go
package text

// Greet builds a greeting for name; an empty name yields "Hello, ".
func Greet(name string) string {
	return "Hello, " + name
}
```

Create `service/service.go`. It imports the library by path and composes over it:

```go
// service/service.go
package service

import "example.com/platform/text"

// Greeting is the service's business logic: it delegates the wording to the
// shared platform library, imported by its module path.
func Greeting(name string) string {
	return text.Greet(name)
}
```

### The service entrypoint

The demo is the service binary: it reads its argument from `os.Args` like a real
entrypoint and prints the composed greeting.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"

	"example.com/platform/service"
)

func main() {
	name := "world"
	if len(os.Args) > 1 {
		name = os.Args[1]
	}
	fmt.Println(service.Greeting(name))
}
```

Run it:

```bash
go run ./cmd/demo ops
```

Expected output:

```
Hello, ops
```

### Tests

The test asserts the composition: the service's output is the library's output.
If someone later inlines a hand-written greeting into the service instead of
delegating to `text.Greet`, the empty-name row diverges from the library contract
and this test catches the drift.

Create `service/service_test.go`:

```go
// service/service_test.go
package service

import (
	"fmt"
	"testing"
)

func TestGreeting(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"normal", "ops", "Hello, ops"},
		{"empty", "", "Hello, "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Greeting(tc.in); got != tc.want {
				t.Fatalf("Greeting(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleGreeting() {
	fmt.Println(Greeting("platform"))
	// Output: Hello, platform
}
```

## Review

The service is correct when its output is exactly the library's output for every
input, so `Greeting("")` must be `"Hello, "` — the same contract the library
pins. The workspace claim to internalize is resolution, not composition: with the
`go.work` active, `import "example.com/platform/text"` in the service comes from
the local `./text` directory, so editing the library needs no publish, no tag,
and no `replace` in the service's `go.mod`. Prove it to yourself in the
two-module layout by changing `text.Greet` and rebuilding the service without any
`go get` — the new wording appears. In the gated single-module form the same
import path resolves as a subpackage, which is why the service source is
identical either way. The common failure is forgetting the `use` entry for the
library: then the import resolves through the proxy (or fails) instead of your
working copy.

## Resources

- [Tutorial: Getting started with multi-module workspaces](https://go.dev/doc/tutorial/workspaces) — building a service that consumes a local module via `go.work`.
- [Go Modules Reference — Workspaces](https://go.dev/ref/mod#workspaces) — how `use` makes a directory a main module resolved before the proxy.
- [Package names and import paths](https://go.dev/ref/spec#Import_declarations) — how an import path maps to a package regardless of resolution.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-convert-single-module-to-workspace.md](03-convert-single-module-to-workspace.md)
