# Exercise 6: The replace Directive for Local and Fork Development

When you need a change in a library you depend on, you rarely tag a release first.
You develop the app against a local checkout of the library with a `replace`
directive, or against a fork. This exercise builds the library side of that
workflow — a local fork module — and drives the app-side `replace`/`dropreplace`
loop with real commands.

This module is fully self-contained: its own `go mod init`, all code inline, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
greeterfork/                independent module: example.com/greeter (a local fork)
  go.mod
  greeter.go                Greeting + Farewell (the fork adds Farewell)
  cmd/
    demo/
      main.go               exercises both functions
  greeter_test.go           table test over both
```

- Files: `greeter.go`, `cmd/demo/main.go`, `greeter_test.go`.
- Implement: `Greeting(name)` plus a new `Farewell(name)` — the feature you forked to add before it exists upstream.
- Test: table asserting both functions.
- Verify: build/test the fork; then drive the app-side `replace`/`dropreplace` workflow with `go mod edit` and `go list -m`.

Set up the fork module (its path is the *upstream* module path — a fork keeps the
original import path so consumers do not have to rewrite imports):

```bash
mkdir -p go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/06-replace-directive-for-local-and-fork-development/cmd/demo
cd go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/06-replace-directive-for-local-and-fork-development
```

### Why replace, and its two hard rules

Say your service imports `example.com/greeter` and you need a `Farewell` function
that upstream does not have yet. You clone the library to `../greeterfork`, add the
function, and point your service's build at that local checkout with a `replace`
in the service's `go.mod`:

```text
require example.com/greeter v0.0.0
replace example.com/greeter v0.0.0 => ../greeterfork
```

Two rules are load-bearing. First, `replace` redirects an *existing* requirement —
it does not create one. You still need the matching `require`; a `replace` with no
`require` is inert. Second, `replace` takes effect **only in the main module**. If
your service is itself consumed as a dependency by some other module, that other
module ignores your `replace` entirely and sees only the raw `require`. So a
`replace` pointing at `../greeterfork` that ships in a tagged release leaves every
consumer's build pointing at a path that exists only on your machine — the classic
broken-consumer footgun. Add it for local work; drop it before you release.

The fork itself is an ordinary module. Here it adds `Farewell`, the reason for the
fork.

Create `greeter.go`:

```go
package greeter

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned when the trimmed name is empty.
var ErrEmptyName = errors.New("name must not be empty")

// Greeting builds a greeting for name.
func Greeting(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("Hello, %s!", trimmed), nil
}

// Farewell is the function this fork adds ahead of upstream.
func Farewell(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("Goodbye, %s!", trimmed), nil
}
```

### The fork's demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/greeter"
)

func main() {
	g, _ := greeter.Greeting("Sam")
	f, _ := greeter.Farewell("Sam")
	fmt.Println(g)
	fmt.Println(f)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Hello, Sam!
Goodbye, Sam!
```

### The fork's test

Create `greeter_test.go`:

```go
package greeter

import (
	"errors"
	"testing"
)

func TestGreetingAndFarewell(t *testing.T) {
	t.Parallel()

	if g, err := Greeting("Sam"); err != nil || g != "Hello, Sam!" {
		t.Fatalf("Greeting = %q, %v; want %q, nil", g, err, "Hello, Sam!")
	}
	if f, err := Farewell("Sam"); err != nil || f != "Goodbye, Sam!" {
		t.Fatalf("Farewell = %q, %v; want %q, nil", f, err, "Goodbye, Sam!")
	}
	if _, err := Farewell("  "); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("Farewell(blank) err = %v, want ErrEmptyName", err)
	}
}
```

Run the gate on the fork:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

### Driving the app side

Now create the consuming app in a sibling directory and wire the `replace` with
`go mod edit` (which edits `go.mod` mechanically, no manual editing):

```bash
mkdir -p go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/06-replace-directive-for-local-and-fork-development/cmd/demo
cd go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/06-replace-directive-for-local-and-fork-development
go mod edit -require example.com/greeter@v0.0.0
go mod edit -replace example.com/greeter@v0.0.0=../greeterfork
```

Write a `cmd/demo/main.go` in the app that calls `greeter.Farewell`, then build.
The app's `go.mod` now carries both the requirement and the redirect:

```text
    require example.com/greeter v0.0.0
    replace example.com/greeter v0.0.0 => ../greeterfork
```

Confirm the redirect resolves to the local path, then run:

```bash
go list -m -f '{{.Path}} => {{with .Replace}}{{.Path}}{{end}}' example.com/greeter
go build ./...
go run ./cmd/demo
```

`go list -m -f` prints `example.com/greeter => ../greeterfork`, proving the build
is using the local fork. Now drop the replace and watch the build fall back to
fetching the (non-existent, un-published) module:

```bash
go mod edit -dropreplace example.com/greeter@v0.0.0
go build ./...
```

The build now fails trying to download `example.com/greeter v0.0.0`:

```text
go: downloading example.com/greeter v0.0.0
unrecognized import path "example.com/greeter": ... 404 Not Found
```

That failure is the whole lesson about leaked `replace` directives, made concrete:
without the local redirect there is nothing real to fetch. In a monorepo you would
`replace` to the sibling path for development and rely on a published version (or a
workspace, covered in the next lesson) for the committed build — and you would
never let the local `replace` reach a consumer.

## Review

The fork is correct when it builds, tests pass, and the app resolves
`example.com/greeter` to `../greeterfork` via `go list -m -f`. The mistakes to
avoid: adding a `replace` without the matching `require` (it does nothing);
believing a `replace` in your module affects downstream consumers (it never does —
it is main-module-only); and, worst, shipping a local-path `replace` in a release,
which breaks every consumer's build. Use `go mod edit -replace`/`-dropreplace`
rather than hand-editing so the directive is always well-formed, and treat a
stray `replace` in a release diff as a blocker in review. For the fork-with-version
form, the target is `replace old => example.com/fork/x vX.Y.Z`, which points at a
real published fork instead of a local path and is safe to keep because it still
resolves for everyone.

## Resources

- [go.mod replace directive](https://go.dev/ref/mod#go-mod-file-replace) — syntax, the main-module-only rule, and the require pairing.
- [go mod edit](https://go.dev/ref/mod#go-mod-edit) — `-require`, `-replace`, and `-dropreplace` flags.
- [Requiring module code in a local directory](https://go.dev/doc/modules/managing-dependencies#local_directory) — the local-fork development workflow.

---

Back to [05-pin-and-inspect-the-build-list.md](05-pin-and-inspect-the-build-list.md) | Next: [07-go-mod-tool-directive-reproducible-tooling.md](07-go-mod-tool-directive-reproducible-tooling.md)
