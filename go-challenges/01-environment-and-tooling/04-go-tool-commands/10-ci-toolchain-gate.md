# Exercise 10: The Single CI Gate Script (Whole-Task Capstone)

This is the deliverable a senior owns for a repository: one reproducible,
network-free script that runs the whole toolchain gate in order, fails on the
first violation, and names the stage that failed. This module builds a clean,
gating package and wraps it in a `check.sh` (and a `Makefile` target) composing
`gofmt -l`, `go vet`, `go build`, `go test -race -shuffle=on`, and a `go list`
import guard.

## What you'll build

```text
ci-gate/                       module example.com/ci-gate
  go.mod
  internal/
    circle/
      circle.go                Area(radius) (float64, error)
      circle_test.go           table-ish test, errors.Is, epsilon
  cmd/
    demo/
      main.go                  imports circle
  check.sh                     the single gate: gofmt, vet, build, test, guard
  Makefile                     .PHONY check target that runs check.sh
```

- Files: `internal/circle/circle.go`, `internal/circle/circle_test.go`, `cmd/demo/main.go`, plus `check.sh` and `Makefile`.
- Implement: a clean `circle` package and a `demo` that imports it.
- Test: `circle` math and the sentinel via `errors.Is`.
- Verify: `./check.sh` exits 0 on the clean module; an unformatted file, a vet finding, or a failing test each makes it exit non-zero and name the failing stage.

### Why one composed gate, and why this order

Every command in the previous nine exercises catches a different class of defect.
The senior deliverable is not "run them when you remember" but a single script
that runs all of them, in the right order, with correct exit-code propagation, so
that CI and a laptop behave identically and the first violation stops the build
with a named stage. `set -euo pipefail` is what makes propagation correct:
`-e` exits on any command that returns non-zero, `-u` errors on an unset variable,
and `-o pipefail` makes a pipeline fail if any stage of it fails (so a failure on
the left of a pipe is not masked by a `grep` on the right).

The order is deliberate, cheapest-and-most-fundamental first: `gofmt -l` (a string
check, no compile) before `go vet` (needs type info) before `go build` (full
compile) before `go test -race` (the slowest) before the `go list` import guard.
A formatting failure should not wait behind a two-minute race test. Every stage
is offline: no `go get`, no network, so the gate is reproducible on an air-gapped
runner.

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
	"math"
	"testing"
)

func TestArea(t *testing.T) {
	t.Parallel()
	got, err := Area(3)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if math.Abs(got-9*math.Pi) > 1e-9 {
		t.Fatalf("Area(3) = %v", got)
	}
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

	"example.com/ci-gate/internal/circle"
)

func main() {
	a, _ := circle.Area(5)
	fmt.Printf("radius 5.0 -> area %.5f\n", a)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
radius 5.0 -> area 78.53982
```

### The gate script

The Go code above is assembled and gated. The script and Makefile below are the
deliverable — save them at the module root. `check.sh`:

```bash
#!/usr/bin/env bash
# check.sh - the single reproducible CI gate. Fails fast, names the stage.
set -euo pipefail

echo "==> gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "FAIL gofmt: not canonical:" >&2
	echo "$unformatted" >&2
	exit 1
fi

echo "==> go vet"
go vet ./...

echo "==> go build"
go build ./...

echo "==> go test (race, shuffle)"
go test -count=1 -race -shuffle=on ./...

echo "==> import guard"
BANNED="example.com/ci-gate/internal/legacy"
if go list -deps ./cmd/demo | grep -qx "$BANNED"; then
	echo "FAIL import-guard: forbidden import present: $BANNED" >&2
	exit 1
fi

echo "OK: all stages passed"
```

`Makefile` — a `.PHONY` target so `make check` never collides with a file named
`check`:

```makefile
.PHONY: check
check:
	./check.sh
```

Make the script executable and run the gate:

```bash
chmod +x check.sh
./check.sh
```

On the clean module it walks every stage and exits 0:

```text
==> gofmt
==> go vet
==> go build
==> go test (race, shuffle)
?   	example.com/ci-gate/cmd/demo	[no test files]
ok  	example.com/ci-gate/internal/circle	1.259s
==> import guard
OK: all stages passed
```

### Proving each stage bites

Introduce one defect at a time; each makes the gate exit non-zero at the stage
that owns it. Do NOT leave these in the module.

(a) An unformatted file stops at `gofmt`:

```text
==> gofmt
FAIL gofmt: not canonical:
internal/circle/messy.go
```

(b) A `printf` bug that compiles stops at `go vet`:

```text
==> go vet
# example.com/ci-gate/internal/circle
internal/circle/bug.go:5:14: fmt.Printf format %d has arg "x" of wrong type string
```

(c) A failing test stops at `go test`:

```text
==> go test (race, shuffle)
--- FAIL: TestBoom (0.00s)
FAIL	example.com/ci-gate/internal/circle
```

In each case `set -e` halts the script at the failing stage, later stages never
run, and the exit code is non-zero — exactly what a CI system keys off. Because
the gate does no network I/O and uses only the toolchain, it produces the same
verdict on a developer's laptop and on a fresh runner.

## Review

The gate is correct when `./check.sh` (or `make check`) exits 0 on the clean
module and non-zero, with the failing stage named, on an unformatted file, a vet
finding, or a failing test. The design points that matter: `set -euo pipefail`
for correct exit-code propagation, cheapest-checks-first ordering so a trivial
failure does not wait behind the race test, and zero network so the gate is
reproducible anywhere. This one script is the on-the-job artifact — the contract
that keeps the repository shippable — that the previous nine exercises were
building toward.

## Resources

- [Command go — testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-race`, `-count`, `-shuffle` used in the gate.
- [Bash set builtin](https://www.gnu.org/software/bash/manual/html_node/The-Set-Builtin.html) — `-e`, `-u`, `-o pipefail` and exit-code propagation.
- [GNU make — phony targets](https://www.gnu.org/software/make/manual/html_node/Phony-Targets.html) — why `check` is declared `.PHONY`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-version-injection-and-reproducible-builds.md](09-version-injection-and-reproducible-builds.md) | Next: [../05-go-install-and-third-party-packages/00-concepts.md](../05-go-install-and-third-party-packages/00-concepts.md)
