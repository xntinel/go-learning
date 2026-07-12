# Exercise 9: Closing The Silent-Green Trap — Vet And Coverage Under Every Tag

`go vet`, `go build`, and `go test` all honor `-tags`. The corollary bites: a bug in
an `//go:build integration` file is invisible to the default `go vet ./...` because
the file is never compiled — the suite reports green while a real defect hides. This
module makes that failure concrete, shows the tagged vet run catching what the
default run misses, and produces a per-tier coverage profile.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
covtags/                   independent module: example.com/covtags
  go.mod
  audit.go                 AuditEvent; FormatAudit (default-build, coverage-measured)
  cmd/
    demo/
      main.go              formats an audit event
  audit_test.go            table-driven tests of FormatAudit + Example
  audit_integration_test.go   //go:build integration: the CORRECT tagged formatter
```

- Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`, `audit_integration_test.go`.
- Implement: `FormatAudit(AuditEvent) string`, plus a tagged integration helper that must be vetted under `-tags=integration`.
- Test: table-driven coverage of `FormatAudit` in the default build; the tagged file demonstrates the vet-under-tags requirement.
- Verify: `go vet ./...` misses a defect in a tagged file; `go vet -tags=integration ./...` catches it; `go test -tags=integration -coverprofile` measures the tagged tier.

### The silent-green trap, made concrete

Here is the bug that hides. Suppose the integration tier has a helper that formats an
audit line, and someone writes the wrong `Printf` verb — `%d` applied to a string
field:

```go
//go:build integration

package covtags

import "fmt"

func logActor(e AuditEvent) string {
	return fmt.Sprintf("actor=%d", e.Actor) // BUG: %d applied to a string field
}
```

Run `go vet ./...` with no tags and it reports nothing — because the file carries
`//go:build integration`, the `go` tool never hands it to the compiler or the vet
analyzers. The suite is green. The defect ships. It surfaces only when the
integration stage finally compiles the file, or worse, when the malformed audit line
reaches a log aggregator in production.

Run `go vet -tags=integration ./...` and vet compiles the tagged file and reports
(here the buggy helper is the file's line 8):

```text
./audit_integration_test.go:8:9: fmt.Sprintf format %d has arg e.Actor of wrong type string
```

That is the entire lesson of this module: *every tier must be vetted and built under
its own tag set*, because the default `go vet ./...` and `go build ./...` never see
tagged files. A CI pipeline that runs only the default vet against the integration
tier is not checking that tier at all. The fix is a matrix: `go vet ./...` for the
default tier, `go vet -tags=integration ./...` for the integration tier,
`go vet -tags=e2e ./...` for the e2e tier — each also building and coverage-measuring
under its own tags.

The shipped tagged file below is the *corrected* version, so this module gates clean.

Create `audit.go`:

```go
package covtags

import "fmt"

// AuditEvent is one audit-log record. Actor and Action are strings; Count is an int.
type AuditEvent struct {
	Actor  string
	Action string
	Count  int
}

// FormatAudit renders an audit event to a stable single line. The format verbs must
// match the field types: %s for the strings, %d for the int.
func FormatAudit(e AuditEvent) string {
	return fmt.Sprintf("actor=%s action=%s count=%d", e.Actor, e.Action, e.Count)
}
```

Now the corrected tagged file — the one CI's `go vet -tags=integration` must check:

Create `audit_integration_test.go`:

```go
//go:build integration

package covtags

import (
	"strings"
	"testing"
)

// TestFormatAuditIntegration stands in for a tagged test whose file the default
// vet/build never compiles. It is only checked when CI runs under -tags=integration
// — which is exactly why that stage is mandatory.
func TestFormatAuditIntegration(t *testing.T) {
	line := FormatAudit(AuditEvent{Actor: "alice", Action: "login", Count: 3})
	if !strings.Contains(line, "actor=alice") || !strings.Contains(line, "count=3") {
		t.Fatalf("formatted line = %q, missing expected fields", line)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/covtags"
)

func main() {
	line := covtags.FormatAudit(covtags.AuditEvent{Actor: "alice", Action: "login", Count: 3})
	fmt.Println(line)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
actor=alice action=login count=3
```

### Tests

The default-build tests give `FormatAudit` full coverage; the coverage of the
*tagged* helper is only measured under `-tags=integration`.

Create `audit_test.go`:

```go
package covtags

import (
	"fmt"
	"testing"
)

func TestFormatAudit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		event AuditEvent
		want  string
	}{
		{"login", AuditEvent{Actor: "alice", Action: "login", Count: 1}, "actor=alice action=login count=1"},
		{"bulk", AuditEvent{Actor: "svc", Action: "purge", Count: 42}, "actor=svc action=purge count=42"},
		{"empty", AuditEvent{}, "actor= action= count=0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FormatAudit(tc.event); got != tc.want {
				t.Fatalf("FormatAudit(%+v) = %q, want %q", tc.event, got, tc.want)
			}
		})
	}
}

func ExampleFormatAudit() {
	fmt.Println(FormatAudit(AuditEvent{Actor: "bob", Action: "logout", Count: 2}))
	// Output: actor=bob action=logout count=2
}
```

### Measuring coverage per tier

The default tier's coverage misses everything behind a tag. Measure each tier under
its own tags and inspect with `go tool cover`:

```bash
# default tier
go test -coverprofile=default.out ./...
go tool cover -func=default.out

# integration tier — compiles and covers the tagged file
go test -tags=integration -coverprofile=integ.out ./...
go tool cover -func=integ.out
```

The `integ.out` profile includes lines the `default.out` profile cannot, because
those lines live in files the default build never compiled. A coverage number
reported only from the default tier systematically overstates how much of the
tagged tiers is exercised — another face of the silent-green trap.

## Review

The single idea: `go vet`, `go build`, and `go test` all honor `-tags`, so a defect
in a tagged file — a wrong `Printf` verb, a compile error, an untested branch — is
invisible to the default tooling and hides behind a passing suite. The fix is a CI
matrix where every tier is vetted, built, and coverage-measured under its own tag
set: `go vet -tags=integration ./...`, `go build -tags=integration ./...`,
`go test -tags=integration -coverprofile=... ./...`, and the same for `e2e`. The
buggy `%d`-on-a-string illustration is deliberately not shipped; the tagged file here
is correct, but it still only gets checked when CI runs under `-tags=integration`.
Confirm `go vet ./...` and `go vet -tags=integration ./...` both pass on the shipped
code, and that the integration coverage profile reports the tagged file the default
profile omits.

## Resources

- [go command: go vet](https://pkg.go.dev/cmd/go#hdr-Report_likely_mistakes_in_packages) — vet honors `-tags` like build and test.
- [cmd/vet](https://pkg.go.dev/cmd/vet) — the analyzers, including the `printf` check.
- [go command: Testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-coverprofile` and `-tags`.
- [go tool cover](https://pkg.go.dev/cmd/cover) — turning a coverage profile into a per-function report.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-e2e-tier-second-build-tag.md](08-e2e-tier-second-build-tag.md) | Next: [../19-golden-file-testing/00-concepts.md](../19-golden-file-testing/00-concepts.md)
