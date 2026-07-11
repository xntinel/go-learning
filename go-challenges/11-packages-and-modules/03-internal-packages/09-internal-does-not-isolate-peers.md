# Exercise 9: Failure Mode — internal/billing Can Freely Import internal/auth

The most expensive misconception about `internal` is that it isolates the packages
underneath it from each other. It does not. Two packages sharing one `internal`
parent — `internal/auth` and `internal/billing` — can import each other freely,
because both are inside the one allow-list that `internal` defines. This exercise
proves that with a real build, then shows the actual fix for peer isolation: nest a
deeper `internal` under the owner.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
peerprobe/                    module example.com/peerprobe
  go.mod
  probe.go                    GoAvailable, PeersFixture, NestedFixture, BuildPkg
  probe_test.go               phase 1: peers import freely; phase 2: nested internal isolates
  cmd/demo/main.go            runnable demo printing both phases' outcomes
```

- Files: `probe.go`, `probe_test.go`, `cmd/demo/main.go`.
- Implement: a phase-1 fixture where `internal/billing` imports `internal/auth`, and a phase-2 fixture where private code moved to `billing/internal/secret` is out of `auth`'s reach.
- Test: assert phase 1 builds (no peer isolation), and phase 2's illegal import is rejected while the owner subtree still builds.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/peerprobe/cmd/demo
cd ~/go-exercises/peerprobe
go mod init example.com/peerprobe
```

### Why one shared internal does not isolate peers

`internal` draws exactly one boundary: outsiders versus the subtree rooted at the
parent of the `internal` directory. Everything inside that subtree is mutually
visible. With a module-root `internal/`, the parent is the module root and the
allow-list is the entire module — so `internal/auth`, `internal/billing`, and every
other package in the module can import one another. There is no second, finer
boundary between siblings. Believing that `internal/billing`'s code is hidden from
`internal/auth` "because it is internal" is a design error you can ship: `auth`
imports `billing`'s guts, a coupling forms, and a later refactor of `billing`
silently breaks `auth`.

Phase 1 demonstrates this directly. In module `example.com/fix`:

```text
fix/
  internal/auth/       const Token
  internal/billing/    imports internal/auth  -> BUILDS (same allow-list, no isolation)
```

The build succeeds — which is the whole point. If you need `billing`'s private code
genuinely hidden from `auth`, you must introduce a deeper boundary whose parent is
`billing`, not the module root. Phase 2 moves the private code to
`billing/internal/secret`:

```text
fix/
  billing/internal/secret/   const Key   -> allow-list is billing/ and below
  billing/charge/            imports secret  -> BUILDS (charge is under billing/)
  auth/                      imports secret  -> REJECTED (auth is not under billing/)
```

Now `secret`'s allow-list is only `billing` and its subtree. `billing/charge` still
imports it fine, but `auth` — a sibling of `billing` at the module root — is outside
that subtree, so the toolchain rejects the import with
`use of internal package ... not allowed`. That is real peer isolation: it comes
from nesting a deeper `internal` under the owner, never from a single shared one.

Create `probe.go`:

```go
// Package peerprobe builds fixture modules proving that a shared internal parent
// does not isolate sibling packages, and that a nested internal does.
package peerprobe

import (
	"os"
	"os/exec"
	"path/filepath"
)

// GoAvailable reports whether a go toolchain is on PATH.
func GoAvailable() bool {
	_, err := exec.LookPath("go")
	return err == nil
}

// PeersFixture: internal/billing imports internal/auth under one shared internal.
func PeersFixture() map[string]string {
	m := map[string]string{}
	m["go.mod"] = "module example.com/fix\n\ngo 1.21\n"
	m["internal/auth/auth.go"] = "package auth\n\nconst Token = \"t\"\n"
	m["internal/billing/billing.go"] = "package billing\n\nimport _ \"example.com/fix/internal/auth\"\n"
	return m
}

// NestedFixture: private code lives under billing/internal/secret. billing/charge
// may import it; auth (a sibling of billing) may not.
func NestedFixture() map[string]string {
	m := map[string]string{}
	m["go.mod"] = "module example.com/fix\n\ngo 1.21\n"
	m["billing/internal/secret/secret.go"] = "package secret\n\nconst Key = \"k\"\n"
	m["billing/charge/charge.go"] = "package charge\n\nimport _ \"example.com/fix/billing/internal/secret\"\n"
	m["auth/auth.go"] = "package auth\n\nimport _ \"example.com/fix/billing/internal/secret\"\n"
	return m
}

// BuildPkg writes files under dir and runs `go build ./<pkg>` there, returning
// combined output and the command error (non-nil on a build failure).
func BuildPkg(dir string, files map[string]string, pkg string) (string, error) {
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	cmd := exec.Command("go", "build", "./"+pkg)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
```

### The runnable demo

The demo runs both phases and prints the four outcomes, making the misconception and
its fix visible side by side.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"strings"

	"example.com/peerprobe"
)

func main() {
	if !peerprobe.GoAvailable() {
		fmt.Println("go toolchain not found on PATH")
		return
	}
	p1, err := os.MkdirTemp("", "peerprobe-p1-")
	if err != nil {
		fmt.Println("mkdir temp:", err)
		return
	}
	defer os.RemoveAll(p1)
	p2, err := os.MkdirTemp("", "peerprobe-p2-")
	if err != nil {
		fmt.Println("mkdir temp:", err)
		return
	}
	defer os.RemoveAll(p2)

	_, peersErr := peerprobe.BuildPkg(p1, peerprobe.PeersFixture(), "internal/billing")
	_, chargeErr := peerprobe.BuildPkg(p2, peerprobe.NestedFixture(), "billing/charge")
	authOut, authErr := peerprobe.BuildPkg(p2, peerprobe.NestedFixture(), "auth")

	fmt.Println("phase 1: peers import freely:", peersErr == nil)
	fmt.Println("phase 2: billing/charge imports its nested internal:", chargeErr == nil)
	fmt.Println("phase 2: auth reaches billing's nested internal:", authErr == nil)
	fmt.Println("phase 2: rejected with diagnostic:", strings.Contains(authOut, "use of internal package"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
phase 1: peers import freely: true
phase 2: billing/charge imports its nested internal: true
phase 2: auth reaches billing's nested internal: false
phase 2: rejected with diagnostic: true
```

### Tests

`TestPeersUnderSharedInternalNotIsolated` asserts phase 1 builds — the surprising,
load-bearing fact. `TestNestedInternalIsolatesPeer` asserts phase 2's `auth` import
is rejected with the diagnostic, while `TestNestedInternalStillImportableWithinOwner`
confirms `billing/charge` can still reach its own nested internal. All skip when no
`go` binary is present.

Create `probe_test.go`:

```go
package peerprobe

import (
	"strings"
	"testing"
)

func TestPeersUnderSharedInternalNotIsolated(t *testing.T) {
	t.Parallel()
	if !GoAvailable() {
		t.Skip("no go toolchain on PATH")
	}
	if out, err := BuildPkg(t.TempDir(), PeersFixture(), "internal/billing"); err != nil {
		t.Fatalf("peers under a shared internal failed to build (they should not be isolated): %v\n%s", err, out)
	}
}

func TestNestedInternalStillImportableWithinOwner(t *testing.T) {
	t.Parallel()
	if !GoAvailable() {
		t.Skip("no go toolchain on PATH")
	}
	if out, err := BuildPkg(t.TempDir(), NestedFixture(), "billing/charge"); err != nil {
		t.Fatalf("billing/charge could not import its own nested internal: %v\n%s", err, out)
	}
}

func TestNestedInternalIsolatesPeer(t *testing.T) {
	t.Parallel()
	if !GoAvailable() {
		t.Skip("no go toolchain on PATH")
	}
	out, err := BuildPkg(t.TempDir(), NestedFixture(), "auth")
	if err == nil {
		t.Fatalf("auth reached billing's nested internal; nesting did not isolate it\n%s", out)
	}
	if !strings.Contains(out, "use of internal package") {
		t.Fatalf("build failed without the internal diagnostic; got:\n%s", out)
	}
}
```

## Review

The two phases together teach the precise limit of the rule. Phase 1 must build:
peers under one shared `internal` are not isolated, and a test that expected the
opposite would be encoding the misconception. Phase 2 must reject `auth` while
letting `billing/charge` through: real isolation comes from a deeper `internal` whose
parent is the owner, and it applies only outside that owner's subtree.

The mistake this exercise exists to prevent is trusting a single shared `internal`
for peer isolation and building a coupling you thought the compiler forbade. When you
genuinely need one subsystem's guts hidden from a sibling, do not reach for more
identifiers or a naming convention — nest the private code under the owner's own
`internal` (`billing/internal/secret`) so the allow-list shrinks to exactly that
owner's subtree. That is the only construct that isolates peers, and the phase-2
build proves it.

## Resources

- [Go Modules Reference: Internal packages](https://go.dev/ref/mod#internal-packages) — the single allow-list a shared `internal` defines.
- [cmd/go: Internal Directories](https://pkg.go.dev/cmd/go#hdr-Internal_Directories) — importability by the parent subtree, at any nesting.
- [`os/exec`](https://pkg.go.dev/os/exec) — driving `go build` to confirm each outcome.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-cmd-wiring-internal-graceful-shutdown.md](08-cmd-wiring-internal-graceful-shutdown.md) | Next: [../04-go-module-versioning/00-concepts.md](../04-go-module-versioning/00-concepts.md)
