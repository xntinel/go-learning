# Exercise 7: go env and the Cross-Compile Target Matrix

`go env` is the toolchain's configuration surface, and `GOOS`/`GOARCH` turn it
into a cross-compiler. This module builds a small binary, inspects the
environment with `go env -json` and `go env -changed`, validates that JSON with a
real Go gadget, enumerates every target with `go tool dist list`, and
cross-compiles for `linux/amd64` and `darwin/arm64`.

## What you'll build

```text
env-matrix/                    module example.com/env-matrix
  go.mod
  internal/
    circle/
      circle.go                Area(radius) (float64, error)
      circle_test.go
  cmd/
    demo/
      main.go                  the binary we cross-compile
    envcheck/
      main.go                  reads stdin, exits non-zero unless it is valid JSON
      main_test.go             tests validate()
```

- Files: `internal/circle/circle.go`, `internal/circle/circle_test.go`, `cmd/demo/main.go`, `cmd/envcheck/main.go`, `cmd/envcheck/main_test.go`.
- Implement: the `circle` library, a `demo` binary, and an `envcheck` command whose `validate` returns a sentinel error unless stdin is valid JSON.
- Test: `circle` math and `envcheck`'s `validate` on good and bad input.
- Verify: `go env -json | go run ./cmd/envcheck` prints `valid JSON`; `GOOS=linux GOARCH=amd64 go build` produces an ELF binary.

### The configuration surface

`go env` prints the settings that govern every build. Passing names filters it:
`go env GOOS GOARCH` prints just those two. `go env -json` emits the whole set as
a JSON object for machine parsing. Two write operations persist defaults into the
user's env file: `go env -w NAME=VALUE` sets one, `go env -u NAME` removes it.

The operationally important flag is `go env -changed` (Go 1.24): it prints only
the settings whose effective value differs from a clean default. When a CI runner
misbehaves and a laptop does not, `go env -changed` on both, diffed, isolates the
single divergent setting instead of forcing you to read the whole environment.
For example, after persisting a `GOFLAGS` default, `-changed` shows exactly that:

```bash
go env -w GOFLAGS=-mod=readonly
go env -changed | grep GOFLAGS
go env -u GOFLAGS
```

```text
GOFLAGS='-mod=readonly'
```

The `-u` restores the default, and `go env -changed` no longer lists it.

### A real JSON gadget

`cmd/envcheck` is a small filter: it reads stdin and exits non-zero unless the
input is valid JSON. The logic is a testable `validate` function so it does not
require shelling out. Piping `go env -json` into it proves both that the JSON is
well-formed and that the gadget works.

Create `cmd/envcheck/main.go`:

```go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// errInvalidJSON reports that stdin did not contain valid JSON.
var errInvalidJSON = errors.New("stdin is not valid JSON")

// validate reads all of r and returns errInvalidJSON unless it is valid JSON.
func validate(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return errInvalidJSON
	}
	return nil
}

func main() {
	if err := validate(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("valid JSON")
}
```

Create `cmd/envcheck/main_test.go`:

```go
package main

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	if err := validate(strings.NewReader(`{"GOOS":"linux"}`)); err != nil {
		t.Fatalf("valid JSON rejected: %v", err)
	}
	if err := validate(strings.NewReader("not json")); !errors.Is(err, errInvalidJSON) {
		t.Fatalf("err = %v, want errInvalidJSON", err)
	}
}
```

Pipe the environment through it:

```bash
go env -json | go run ./cmd/envcheck
```

```text
valid JSON
```

### The library and the binary to cross-compile

Create `internal/circle/circle.go`:

```go
package circle

import (
	"errors"
	"math"
)

// ErrNegativeRadius is returned when the radius is negative.
var ErrNegativeRadius = errors.New("radius must not be negative")

// Area returns the area of a circle with the given radius.
func Area(radius float64) (float64, error) {
	if radius < 0 {
		return 0, ErrNegativeRadius
	}
	return math.Pi * radius * radius, nil
}
```

Create `internal/circle/circle_test.go`:

```go
package circle

import (
	"errors"
	"testing"
)

func TestArea(t *testing.T) {
	t.Parallel()
	if _, err := Area(-1); !errors.Is(err, ErrNegativeRadius) {
		t.Fatalf("Area(-1) err = %v", err)
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/env-matrix/internal/circle"
)

func main() {
	a, _ := circle.Area(5)
	fmt.Printf("radius 5.0 -> area %.5f\n", a)
}
```

Run it on the host:

```bash
go run ./cmd/demo
```

Expected output:

```text
radius 5.0 -> area 78.53982
```

### The target matrix

`go tool dist list` is the authoritative, always-current enumeration of every
supported `GOOS/GOARCH` pair. Derive a release matrix from it rather than a
hand-written list:

```bash
go tool dist list | grep -E '^(linux/amd64|darwin/arm64)$'
```

```text
darwin/arm64
linux/amd64
```

Cross-compile by setting `GOOS`/`GOARCH` on the build. `CGO_ENABLED=0` forces a
static, pure-Go binary with no libc dependency — the right default for a scratch
or distroless container image:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/demo-linux ./cmd/demo
file bin/demo-linux
```

```text
bin/demo-linux: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked, ... not stripped
```

```bash
GOOS=darwin GOARCH=arm64 go build -o bin/demo-darwin ./cmd/demo
file bin/demo-darwin
```

```text
bin/demo-darwin: Mach-O 64-bit executable arm64
```

`file` confirms the target: an ELF for `linux`, a Mach-O for `darwin`, produced
from the same source on one host.

## Review

The module is correct when `go test ./...` passes (covering both `circle` and
`envcheck`'s `validate`) and `go env -json | go run ./cmd/envcheck` prints
`valid JSON`. The cross-compiles are correct when `file` reports an ELF for
`linux/amd64` and a Mach-O for `darwin/arm64`. The traps: maintaining a
hand-written target list that rots instead of deriving it from
`go tool dist list`, and debugging a broken runner by dumping the whole `go env`
instead of `go env -changed`, which isolates the divergent setting in one line.

## Resources

- [Command go — go env](https://pkg.go.dev/cmd/go#hdr-Print_Go_environment_information) — `-json`, `-changed`, `-w`, `-u`.
- [Installing Go — GOOS and GOARCH](https://go.dev/wiki/GoArm) — how the environment selects a cross-compile target.
- [encoding/json — Valid](https://pkg.go.dev/encoding/json#Valid) — the well-formedness check the gadget uses.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-gofmt-canonical-gate.md](06-gofmt-canonical-gate.md) | Next: [08-go-list-dependency-audit.md](08-go-list-dependency-audit.md)
