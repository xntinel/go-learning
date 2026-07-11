# Exercise 10: The approval-testing received/approved file pattern

The `-update` flag makes regeneration one keystroke — which is also its danger. The
approval-testing workflow (from the ApprovalTests family) makes accepting a change
a deliberate, human act: on a mismatch it writes the actual output to a
`.received` file and fails; a developer inspects it and promotes it to `.approved`
to accept. This module implements that state machine and contrasts it with the
automatic flag.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
approval/                  independent module: example.com/approval
  go.mod                   go 1.26
  approval.go              Render(User), compare(dir,name,got) (outcome, error)
  testdata/
    report.approved        the committed, human-approved contract
  cmd/
    demo/
      main.go              renders the report and prints it
  approval_test.go         Approve helper + missing/mismatch/match state tests
```

Files: `approval.go`, `testdata/report.approved`, `cmd/demo/main.go`, `approval_test.go`.
Implement: `Render(User) []byte` and `compare(dir, name, got)` returning one of three outcomes — approved-match, missing-approved (wrote received), mismatch (wrote received).
Test: drive all three branches against a `t.TempDir`; verify the committed `.approved` matches; verify a matching run deletes a stale `.received`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/approval/cmd/demo ~/go-exercises/approval/testdata
cd ~/go-exercises/approval
go mod init example.com/approval
```

### The received/approved state machine

There are exactly two committed states for a case and one transient one. The
`.approved` file is the contract, committed to version control. The `.received`
file is transient working state, written whenever the actual output does not match
the approved value, and git-ignored — it must never be committed, because it is the
un-blessed output, not the contract. `compare` encodes the machine: read the
`.approved` file; if it does not exist (`errors.Is(err, os.ErrNotExist)`), write the
output to `.received` and report *missing-approved*; if it exists but the bytes
differ, write `.received` and report *mismatch*; if the bytes match, delete any
stale `.received` and report *approved-match*. Both failing branches leave a
`.received` file on disk holding exactly what the code produced, so the developer
can open it, diff it against `.approved`, and — only if the change is intended —
promote it with a single `mv`.

The contrast with `-update` is the whole point. `-update` rewrites the golden in
place the instant you run it, which makes turning a red test green a reflex: run
the flag, commit, ship — including any regression. The received/approved split
inserts a deliberate human step between "the output changed" and "the new output is
the contract." You cannot accept a change without physically promoting the received
file, and that friction is a feature: it is what stops an unreviewed re-approval
from laundering a serialization bug into a green build. In review, the diff of the
`.approved` file *is* the change description, and a reviewer reads it because
nothing else could have produced it.

`errors.Is(err, os.ErrNotExist)` is the right check for the missing-file branch
because `os.ReadFile` wraps the underlying `*PathError`; matching the sentinel is
robust where a string compare on the error text would be brittle. On the match
branch, removing a stale `.received` is likewise guarded with
`errors.Is(err, os.ErrNotExist)` so "already absent" is not treated as a failure.

Create `approval.go`:

```go
package approval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// User is the record the report renders.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Render returns indented JSON for u with exactly one trailing newline.
func Render(u User) []byte {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("render: marshal: %v", err))
	}
	return append(data, '\n')
}

// outcome is the result of comparing produced output against an approved file.
type outcome int

const (
	approvedMatch outcome = iota
	missingApproved
	mismatch
)

// compare implements the received/approved state machine. On a missing approved
// file or a byte mismatch it writes the produced output to <name>.received and
// reports the corresponding outcome; on a match it deletes any stale received
// file and reports approvedMatch.
func compare(dir, name string, got []byte) (outcome, error) {
	approved := filepath.Join(dir, name+".approved")
	received := filepath.Join(dir, name+".received")

	want, err := os.ReadFile(approved)
	if errors.Is(err, os.ErrNotExist) {
		if werr := writeReceived(received, got); werr != nil {
			return 0, werr
		}
		return missingApproved, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read approved: %w", err)
	}
	if !bytes.Equal(want, got) {
		if werr := writeReceived(received, got); werr != nil {
			return 0, werr
		}
		return mismatch, nil
	}
	if rerr := os.Remove(received); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return 0, fmt.Errorf("remove stale received: %w", rerr)
	}
	return approvedMatch, nil
}

func writeReceived(path string, got []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir received dir: %w", err)
	}
	if err := os.WriteFile(path, got, 0o644); err != nil {
		return fmt.Errorf("write received: %w", err)
	}
	return nil
}
```

Now the committed, human-approved contract:

Create `testdata/report.approved`:

```text
{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/approval"
)

func main() {
	os.Stdout.Write(approval.Render(approval.User{ID: "u1", Name: "Alice", Email: "alice@example.com"}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}
```

### Tests

`Approve` wraps `compare` for the committed `testdata/` directory and fails with a
promote instruction on either non-match branch. `TestApprovedMatch` proves the
committed `.approved` matches the rendered report. The three `t.TempDir` tests drive
each branch of the state machine in isolation: a missing approved writes a received
and reports missing; a differing approved writes a received and reports mismatch; a
matching approved deletes a stale received and reports match.

Create `approval_test.go`:

```go
package approval

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Approve compares got against the committed testdata/<name>.approved file,
// failing with a promote instruction on a missing or mismatched approval.
func Approve(t *testing.T, name string, got []byte) {
	t.Helper()
	oc, err := compare("testdata", name, got)
	if err != nil {
		t.Fatalf("approve %s: %v", name, err)
	}
	switch oc {
	case missingApproved:
		t.Fatalf("no testdata/%s.approved; wrote testdata/%s.received. Inspect it and promote:\n  mv testdata/%s.received testdata/%s.approved",
			name, name, name, name)
	case mismatch:
		t.Fatalf("output differs from testdata/%s.approved; wrote testdata/%s.received. Inspect the diff and, if intended, promote:\n  mv testdata/%s.received testdata/%s.approved",
			name, name, name, name)
	}
}

func sample() User {
	return User{ID: "u1", Name: "Alice", Email: "alice@example.com"}
}

func TestApprovedMatch(t *testing.T) {
	Approve(t, "report", Render(sample()))
}

func TestMissingApprovedWritesReceived(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := Render(sample())

	oc, err := compare(dir, "case", got)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if oc != missingApproved {
		t.Fatalf("outcome = %d, want missingApproved", oc)
	}
	received, err := os.ReadFile(filepath.Join(dir, "case.received"))
	if err != nil {
		t.Fatalf("received not written: %v", err)
	}
	if !bytes.Equal(received, got) {
		t.Fatalf("received bytes differ from produced output")
	}
}

func TestMismatchWritesReceived(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "case.approved"), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("seed approved: %v", err)
	}
	got := Render(sample())

	oc, err := compare(dir, "case", got)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if oc != mismatch {
		t.Fatalf("outcome = %d, want mismatch", oc)
	}
	received, err := os.ReadFile(filepath.Join(dir, "case.received"))
	if err != nil {
		t.Fatalf("received not written: %v", err)
	}
	if !bytes.Equal(received, got) {
		t.Fatalf("received bytes differ from produced output")
	}
}

func TestMatchRemovesStaleReceived(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := Render(sample())
	if err := os.WriteFile(filepath.Join(dir, "case.approved"), got, 0o644); err != nil {
		t.Fatalf("seed approved: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "case.received"), []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed received: %v", err)
	}

	oc, err := compare(dir, "case", got)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if oc != approvedMatch {
		t.Fatalf("outcome = %d, want approvedMatch", oc)
	}
	if _, err := os.Stat(filepath.Join(dir, "case.received")); !os.IsNotExist(err) {
		t.Fatalf("stale received not removed on match (stat err = %v)", err)
	}
}
```

## Review

The received/approved pattern is correct when accepting a change requires a
deliberate act — promoting a `.received` file to `.approved` — rather than a reflex.
The three `TempDir` tests prove the state machine: a missing approval and a
mismatch each leave a `.received` holding the exact produced bytes, and a match
sweeps a stale `.received` away so it never lingers to be mistakenly committed. The
one rule the code cannot enforce, and the reviewer must, is that only `.approved`
files are committed — add `*.received` to `.gitignore`, because a committed
received file leaks un-blessed output and obscures the real contract. Weighed
against the `-update` flag of Exercise 4, this workflow trades convenience for a
human checkpoint: slower to accept a change, but far harder to accidentally
launder a regression into a green build. Run `go test -race` to confirm all four
branches.

## Resources

- [os: ReadFile, WriteFile, Remove](https://pkg.go.dev/os#ReadFile) — the file primitives behind the approved/received machine.
- [errors: Is and os.ErrNotExist](https://pkg.go.dev/os#pkg-variables) — matching the not-exist sentinel robustly.
- [ApprovalTests](https://approvaltests.com/) — the received/approved workflow this module models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-error-envelope-snapshot.md](09-error-envelope-snapshot.md) | Next: [../24-property-based-testing/00-concepts.md](../24-property-based-testing/00-concepts.md)
