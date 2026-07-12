# Exercise 6: Migrate a Legacy v1 Config to v2 Without Losing Coverage

This is the real on-the-job task: a repo has a v1 `.golangci.yml` and you must move
it to the v2 schema without silently dropping coverage. You run `golangci-lint
migrate`, reconcile the output, re-add the comments the tool dropped, and — the part
that makes the migration trustworthy — diff the enabled-linter set before and after
to prove nothing vanished except the intended merges.

This module is self-contained: its own `go mod init`, a small `envconf` package to
lint against, a demo, and a test.

## What you'll build

```text
cfgmigrate/                   independent module: example.com/cfgmigrate
  go.mod                      go 1.24
  envconf.go                  LoadPort(def) reads PORT, validates range
  envconf_test.go             t.Setenv table: default, valid, invalid, range
  cmd/
    demo/
      main.go                 prints the resolved port
  .golangci.yml               the migrated v2 config (shown in prose)
```

- Files: `envconf.go`, `envconf_test.go`, `cmd/demo/main.go`, plus the v1 and v2 configs in prose.
- Implement: `LoadPort(def int) (int, error)` reading `PORT`, defaulting when unset, and returning `ErrInvalidPort` for non-numeric or out-of-range values.
- Test: a `t.Setenv`-driven table covering default, valid, non-numeric, and out-of-range.
- Verify: `go test -count=1 -race ./...`; then `golangci-lint migrate`, `config verify`, and a before/after linter diff.

### The starting point: a representative v1 config

A typical v1 `.golangci.yml` from a real service looks like this — `disable-all`
with an explicit enable list that includes `gosimple`, a formatter (`gofmt`,
`goimports`) mixed in among the linters, a `linters-settings` block, and an
`issues.exclude-rules` scoping a check to test files:

```yaml
# v1 config — do NOT keep as-is under golangci-lint v2
linters:
  disable-all: true
  enable:
    - errcheck
    - govet
    - staticcheck
    - gosimple      # merged into staticcheck in v2
    - stylecheck    # merged into staticcheck in v2
    - unused
    - ineffassign
    - gofmt         # a formatter — moves to the formatters section in v2
    - goimports     # a formatter — moves to the formatters section in v2

linters-settings:
  goimports:
    local-prefixes: example.com/cfgmigrate

issues:
  exclude-rules:
    - path: _test\.go
      linters:
        - errcheck
```

Under v2 this file is not merely deprecated — several parts are *errors*.
`disable-all` is gone, `gosimple`/`stylecheck` no longer exist as separate names,
and `gofmt`/`goimports` are not linters anymore. Left unmigrated, v2 refuses to run
it; migrated carelessly, it quietly stops formatting and loses the two style checks
that folded into `staticcheck`.

### Run the migration and reconcile

```bash
golangci-lint migrate
```

The tool rewrites `.golangci.yml` in place, saves the original as
`.golangci.bck.yml`, and applies the mechanical mappings: `disable-all: true`
becomes `linters.default: none`; `gosimple` and `stylecheck` are dropped because
`staticcheck` now *absorbs* them; `gofmt` and `goimports` move out of `linters` into
a new `formatters` section; `linters-settings` splits into `linters.settings` and
`formatters.settings`; and `issues.exclude-rules` becomes
`linters.exclusions.rules`. One thing it does **not** do is carry over your comments —
those are dropped, and you re-add them by hand.

The reconciled result:

```yaml
version: "2"

linters:
  default: none
  enable:
    - errcheck
    - govet
    - staticcheck   # now also covers the former gosimple + stylecheck
    - unused
    - ineffassign
  exclusions:
    rules:
      - path: _test\.go
        linters:
          - errcheck

formatters:
  enable:
    - gofmt
    - goimports
  settings:
    goimports:
      local-prefixes:
        - example.com/cfgmigrate
```

Validate it against the schema:

```bash
golangci-lint config verify
```

### Prove no coverage was lost

The migration is only trustworthy if the *effective* set of analyzers did not shrink
by accident. Diff the enabled linters before and after by pointing `golangci-lint
linters` at each config and comparing the "Enabled" section:

```bash
golangci-lint linters -c .golangci.bck.yml | sed -n '/Enabled/,/Disabled/p' > before.txt
golangci-lint linters -c .golangci.yml    | sed -n '/Enabled/,/Disabled/p' > after.txt
diff before.txt after.txt
```

The only expected difference is that `gosimple` and `stylecheck` disappear as
separate lines — their checks now run *inside* `staticcheck`, so coverage is
preserved, not lost. If any *other* linter vanished, the migration ate something and
you fix the enable list. This diff-and-verify step is the entire discipline: the
mechanical migration is easy; proving it did not silently reduce the gate is the
work.

Finally, confirm identical findings against the code:

```bash
golangci-lint run ./...
```

### The code being linted

Create `envconf.go`:

```go
package envconf

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// ErrInvalidPort is returned when PORT is set but not a valid TCP port.
var ErrInvalidPort = errors.New("invalid port")

// LoadPort reads the PORT environment variable, returning def when it is unset
// or empty. A set-but-invalid value returns ErrInvalidPort (wrapped with %w).
func LoadPort(def int) (int, error) {
	raw, ok := os.LookupEnv("PORT")
	if !ok || raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("PORT=%q: %w", raw, ErrInvalidPort)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("PORT=%d out of range: %w", n, ErrInvalidPort)
	}
	return n, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/cfgmigrate"
)

func main() {
	port, err := envconf.LoadPort(8080)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("listening on :%d\n", port)
}
```

Run it (with `PORT` unset, the default applies):

```bash
go run ./cmd/demo
```

Expected output:

```
listening on :8080
```

### Tests

`t.Setenv` sets `PORT` for the duration of a test and restores it after; because it
is incompatible with `t.Parallel`, these subtests run sequentially. The table covers
the default (unset), a valid value, a non-numeric value, and an out-of-range value.

Create `envconf_test.go`:

```go
package envconf

import (
	"errors"
	"testing"
)

func TestLoadPort(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		want    int
		wantErr error
	}{
		{name: "unset uses default", set: false, want: 8080},
		{name: "empty uses default", set: true, value: "", want: 8080},
		{name: "valid", set: true, value: "9090", want: 9090},
		{name: "non-numeric", set: true, value: "abc", wantErr: ErrInvalidPort},
		{name: "out of range", set: true, value: "70000", wantErr: ErrInvalidPort},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("PORT", tc.value)
			} else {
				// Ensure PORT is not inherited from the environment.
				t.Setenv("PORT", "")
			}

			got, err := LoadPort(8080)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("LoadPort() err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadPort() unexpected err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("LoadPort() = %d, want %d", got, tc.want)
			}
		})
	}
}
```

## Review

The migration is correct when `golangci-lint config verify` accepts the v2 file and
the before/after linter diff shows *only* `gosimple` and `stylecheck` gone —
absorbed into `staticcheck`, not lost. The three traps the tool cannot save you from
are exactly the ones this exercise drills: assuming a v1 file still runs (it does
not), assuming a naive delete of `gosimple`/`stylecheck` keeps their checks (it does,
but only because `staticcheck` subsumes them — verify it), and forgetting that
`gofmt`/`goimports` moved to `formatters` (a config that drops them silently stops
formatting). Re-adding the comments `migrate` discarded is not cosmetic: the comment
explaining *why* a check is excluded on `_test.go` is what stops the next engineer
from widening it. The `envconf` code exists only to give the linter something to
chew on; the deliverable is a v2 config proven equivalent to the v1 gate.

## Resources

- [golangci-lint: v1 to v2 Migration Guide](https://golangci-lint.run/docs/product/migration-guide/) — the `migrate` command and every key mapping.
- [golangci-lint: Configuration File (v2)](https://golangci-lint.run/docs/configuration/file/) — the v2 schema the migration targets.
- [staticcheck](https://staticcheck.dev/) — the analyzer that now includes the former `gosimple` and `stylecheck`.

---

Back to [05-expanding-linter-set-gocritic.md](05-expanding-linter-set-gocritic.md) | Next: [07-formatters-section-and-fmt-gate.md](07-formatters-section-and-fmt-gate.md)
