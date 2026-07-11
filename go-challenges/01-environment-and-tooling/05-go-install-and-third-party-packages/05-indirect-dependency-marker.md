# Exercise 5: Reading the // indirect Marker in go.mod

A `require` line with a `// indirect` comment tells you the module is in your
graph only because a dependency of yours needs it — nothing in your own code
imports it. This exercise makes that visible by importing one module,
`rsc.io/quote/v3`, that transitively pulls in two more, then reading `go.mod`,
`go mod why`, and `go mod graph` to trace exactly who requires what.

This module is self-contained; it links `rsc.io/quote/v3` and its transitive
dependencies.

## What you'll build

```text
greetquote/                     independent module: example.com/greetquote
  go.mod                        rsc.io/quote/v3 direct; sampler + x/text // indirect
  go.sum
  proverb.go                    Go() string -> quote.GoV3()
  proverb_test.go               asserts the deterministic proverb; Example
  cmd/demo/
    main.go                     prints the proverb
```

Files: `proverb.go`, `proverb_test.go`, `cmd/demo/main.go`.
Implement: `Go() string` returning `quote.GoV3()`.
Test: assert the exact proverb string; `Example` with `// Output:`.
Verify: `go test -count=1 -race ./...`

Set up the module and add the dependency:

```bash
mkdir -p ~/go-exercises/greetquote/cmd/demo
cd ~/go-exercises/greetquote
go mod init example.com/greetquote
go get rsc.io/quote/v3@latest
go mod tidy
```

### Direct versus indirect, made concrete

`rsc.io/quote/v3` is the canonical example the Go module tutorial uses precisely
because it has a dependency chain: `rsc.io/quote/v3` imports `rsc.io/sampler`,
which imports `golang.org/x/text`. Your code imports only `rsc.io/quote/v3`, so
after `go mod tidy` your `go.mod` looks like this:

```text
require rsc.io/quote/v3 v3.1.0

require (
	golang.org/x/text v0.14.0 // indirect
	rsc.io/sampler v1.3.0 // indirect
)
```

The first `require` has no comment: your package imports it directly. The two in
the second block carry `// indirect` because *no* file in your main module
imports them — they are reachable only through `quote/v3`. The exact versions
depend on what `@latest` resolves to; the structure is the point.

`go mod tidy` is what keeps those annotations truthful. If you later added a
direct `import "golang.org/x/text/language"` to your own code, the next
`go mod tidy` would strip the `// indirect` from the `x/text` line. Two commands
explain provenance:

```bash
go mod why -m rsc.io/sampler     # names a chain from your code to sampler
go mod graph | grep sampler      # prints the raw require edges involving sampler
go list -m all                   # lists every module in the build, one per line
```

`go mod why -m rsc.io/sampler` prints a path like
`example.com/greetquote` -> `rsc.io/quote/v3` -> `rsc.io/sampler`, which is the
answer to "why is this transitive module here, and can I remove it?" (you cannot,
without dropping `quote/v3`).

`quote.GoV3()` returns a fixed Go proverb, so the test can assert it exactly.

Create `proverb.go`:

```go
// proverb.go
package proverb

import "rsc.io/quote/v3"

// Go returns a well-known Go proverb, sourced from the transitively-linked
// rsc.io/quote/v3 module. The value is deterministic.
func Go() string {
	return quote.GoV3()
}
```

### The demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/greetquote"
)

func main() {
	fmt.Println(proverb.Go())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
Don't communicate by sharing memory, share memory by communicating.
```

### The test

The proverb is deterministic, so the test asserts it exactly. This also proves
the transitive chain (`quote/v3` -> `sampler` -> `x/text`) links and runs.

Create `proverb_test.go`:

```go
// proverb_test.go
package proverb

import (
	"fmt"
	"testing"
)

func TestGo(t *testing.T) {
	t.Parallel()

	const want = "Don't communicate by sharing memory, share memory by communicating."
	if got := Go(); got != want {
		t.Fatalf("Go() = %q, want %q", got, want)
	}
}

func ExampleGo() {
	fmt.Println(Go())
	// Output: Don't communicate by sharing memory, share memory by communicating.
}
```

## Review

The graph is annotated correctly when `rsc.io/quote/v3` appears in `go.mod`
without a comment (you import it) and `rsc.io/sampler` and `golang.org/x/text`
appear with `// indirect` (you do not). The trap is hand-editing `go.mod` to add
or remove a `// indirect` comment — the marker is derived, not authored, so
`go mod tidy` will overwrite your edit to match the real import graph. Confirm
with `go mod why -m rsc.io/sampler` (it names the chain through `quote/v3`),
`go build ./...`, and `go test -race ./...`.

## Resources

- [Create a Go module (the rsc.io/quote tutorial)](https://go.dev/doc/tutorial/create-module) — the origin of this dependency chain.
- [`go mod why` and `go mod graph`](https://go.dev/ref/mod#go-mod-why) — tracing provenance of a required module.
- [Go Modules Reference: indirect dependencies](https://go.dev/ref/mod#go-mod-file-require) — what `// indirect` means and when it is written.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-go-install-and-use-standalone-tool.md](04-go-install-and-use-standalone-tool.md) | Next: [06-read-binary-build-metadata.md](06-read-binary-build-metadata.md)
