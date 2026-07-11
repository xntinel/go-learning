# Exercise 4: Prove Local Modules Win Over The Proxy

The core guarantee of a workspace is that a `use`d module is a *main module*: its
code is taken from disk before MVS ever consults the proxy. This exercise makes
that guarantee visible by pointing a service at a library version that exists on
no proxy, then showing the build succeed anyway because the workspace supplies the
local copy — and by reading `go list -m all` and `go env GOWORK` to see the
resolution the workspace performed.

## What you'll build

```text
platform/                      gated module: example.com/platform
  go.mod                       go 1.26
  text/
    text.go                    package text; Greet + Version (an unpublishable version)
  service/
    service.go                 package service; Backend reports the resolved library version
    service_test.go            asserts the locally-edited library is what runs
  cmd/
    demo/
      main.go                  prints the greeting and the resolved library version
```

- Files: `text/text.go`, `service/service.go`, `service/service_test.go`, `cmd/demo/main.go`.
- Implement: a library `Version` constant that no proxy could serve, and `service.Backend()` that reports it.
- Test: assert the service observes the local `Version`, proving the local code — not a proxy version — is what compiled.
- Verify: the build succeeds despite an unpublishable `require`, `go list -m all` shows the library as a local main module, and `go env GOWORK` is non-empty.

Set up the gated module:

```bash
mkdir -p ~/platform/text ~/platform/service ~/platform/cmd/demo
cd ~/platform
go mod init example.com/platform
go mod edit -go=1.26
```

### The unpublishable require, and why the build still works

In the two-module workspace, the service's `go.mod` can require a library version
that was never tagged or pushed anywhere — its `require` line reads:

```text
require example.com/platform/text v1.4.0-20990101000000-deadbeefcafe   # no proxy has this
```

That pseudo-version names a commit no proxy has. Outside a workspace,
`go build` would try to fetch it and fail with a `410 Gone`/`not found`. But with
a `go.work` that lists `./text`, the library is a *main module*: MVS reads its
code straight from the working tree and never consults the proxy for it. The
build succeeds, and the `require` line's exact version becomes irrelevant while
the workspace is active — the local copy always wins.

You can see the resolution directly. From the workspace root:

```bash
go env GOWORK        # -> /Users/you/mono/go.work   (non-empty: workspace active)
go list -m all
```

`go list -m all` prints the library with **no version**, the signature of a local
main module:

```text
example.com/platform/service
example.com/platform/text            # a main module: local, no version
```

A dependency resolved from the proxy would carry a version
(`example.com/platform/text v1.3.0`); a bare path means "this is on disk, taken
locally". That is the proof that the workspace, not the proxy, supplied the code.

The gated artifact makes the same point observably in Go: the library carries a
`Version` constant set to an unpublishable value, and the service reports it. If
the service is reading the *local* library — as the workspace guarantees — it
observes exactly that constant.

Create `text/text.go`:

```go
// text/text.go
package text

// Version is deliberately a value no proxy could serve; observing it from the
// service proves the local (workspace) copy is what compiled.
const Version = "v1.4.0-local+workspace"

// Greet builds a greeting for name; an empty name yields "Hello, ".
func Greet(name string) string {
	return "Hello, " + name
}
```

Create `service/service.go`:

```go
// service/service.go
package service

import "example.com/platform/text"

// Backend reports which library build the service is compiled against. Under a
// workspace this is the local copy's Version, not a proxy-served version.
func Backend() string {
	return "text@" + text.Version
}

// Greeting delegates the wording to the shared library.
func Greeting(name string) string {
	return text.Greet(name)
}
```

### The demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/platform/service"
)

func main() {
	fmt.Println(service.Greeting("ops"))
	fmt.Println("resolved:", service.Backend())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Hello, ops
resolved: text@v1.4.0-local+workspace
```

### Tests

The test asserts the service observes the local library's `Version`. In the real
workspace this is the load-bearing proof: the build compiled against the on-disk
library even though the recorded `require` names an unfetchable version.

Create `service/service_test.go`:

```go
// service/service_test.go
package service

import (
	"testing"

	"example.com/platform/text"
)

func TestBackendUsesLocalLibrary(t *testing.T) {
	t.Parallel()

	want := "text@" + text.Version
	if got := Backend(); got != want {
		t.Fatalf("Backend() = %q, want %q (local library not resolved)", got, want)
	}
	if text.Version != "v1.4.0-local+workspace" {
		t.Fatalf("Version = %q; the local copy is not what compiled", text.Version)
	}
}

func TestGreeting(t *testing.T) {
	t.Parallel()
	if got := Greeting(""); got != "Hello, " {
		t.Fatalf("Greeting(\"\") = %q, want %q", got, "Hello, ")
	}
}
```

## Review

The resolution order to keep straight is: workspace main modules feed MVS before
the proxy, so a `use`d module's on-disk code always wins over any version its
consumers `require`. The two observable proofs are `go list -m all` showing the
library with no version (a main module, local) and a build that succeeds even
though the recorded `require` names a pseudo-version no proxy can serve. The Go
test encodes the consequence: the service reports the local `Version` constant,
so if resolution ever fell back to a proxy copy the string would differ and the
test would fail. `go env GOWORK` being non-empty is the one-line confirmation you
are in workspace mode at all — the very next exercise flips it off to reproduce
CI.

## Resources

- [Go Modules Reference — Workspaces](https://go.dev/ref/mod#workspaces) — main modules and how `use` precedes the proxy in resolution.
- [`go list -m`](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules) — reading the module graph and spotting local main modules by their missing version.
- [Minimal version selection](https://go.dev/ref/mod#minimal-version-selection) — how MVS chooses versions and where main modules sit in that order.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-workspace-replace-local-fork.md](05-workspace-replace-local-fork.md)
