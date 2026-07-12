# Exercise 12: An Environment Overlay Builder for Subprocess Launching

**Nivel: Intermedio** — validacion rapida (un test corto).

Spawning a subprocess with a few extra environment variables is a daily
backend task — a job runner adds `DEBUG=1`, a test harness overrides `PATH`.
The naive approach, `cmd.Env = append(os.Environ(), overrides...)`, just
appends: the process ends up with two `PATH` entries, and which one the OS
honors is implementation detail, not a contract. This module builds
`Overlay(base []string, overrides ...string) []string`, a variadic function
that merges by key instead of blindly appending.

## What you'll build

```text
envoverlay/                independent module: example.com/env-overlay
  go.mod                   go 1.24
  overlay.go               package envoverlay; func Overlay(base []string, overrides ...string) []string
  overlay_test.go          table test: override in place, new key appended, later-wins, no-overrides
```

- Files: `overlay.go`, `overlay_test.go`.
- Implement: `Overlay(base []string, overrides ...string) []string` merging `KEY=VALUE` entries by key.
- Test: an override replaces a base entry in its original position; a genuinely new key is appended in override order; two overrides of the same key resolve to the later one; zero overrides returns base unchanged.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/12-env-overlay-builder
cd go-solutions/04-functions/03-variadic-functions/12-env-overlay-builder
go mod edit -go=1.24
```

### Merging by key, not by position

Both `base` and `overrides` are flat `KEY=VALUE` string slices — exactly the
shape `os.Environ()` returns and `exec.Cmd.Env` expects. `Overlay` first
records every base entry's key and value, in order. It then walks
`overrides` the same way: a key already seen updates its recorded value in
place (the *position* in the output does not change); a key not yet seen is
noted as new, to be appended after all base keys, in the order those new
keys first appeared among the overrides. The last write for any given key
wins, whether that write came from `base` or from a later override — so
`Overlay(base, "PATH=/a", "PATH=/b")` ends with `/b`, never a duplicate
`PATH` entry.

Create `overlay.go`:

```go
// overlay.go
package envoverlay

import "strings"

// Overlay merges a base environment slice (KEY=VALUE strings, e.g. from
// os.Environ()) with a variadic list of override entries in the same
// KEY=VALUE form. Overrides win over base and over earlier overrides for the
// same key. The result preserves base's original order for existing keys
// (with any overridden value applied) and appends genuinely new keys in the
// order they were first seen among overrides.
func Overlay(base []string, overrides ...string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))

	keyOf := func(entry string) (string, string) {
		if i := strings.IndexByte(entry, '='); i >= 0 {
			return entry[:i], entry[i+1:]
		}
		return entry, ""
	}

	for _, e := range base {
		k, v := keyOf(e)
		if _, seen := values[k]; !seen {
			order = append(order, k)
		}
		values[k] = v
	}
	for _, e := range overrides {
		k, v := keyOf(e)
		if _, seen := values[k]; !seen {
			order = append(order, k)
		}
		values[k] = v
	}

	result := make([]string, 0, len(order))
	for _, k := range order {
		result = append(result, k+"="+values[k])
	}
	return result
}
```

### Test

Create `overlay_test.go`:

```go
// overlay_test.go
package envoverlay

import (
	"reflect"
	"testing"
)

func TestOverlay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		base      []string
		overrides []string
		want      []string
	}{
		{
			name:      "override existing key keeps position",
			base:      []string{"HOME=/root", "PATH=/bin", "LANG=en_US"},
			overrides: []string{"PATH=/usr/bin"},
			want:      []string{"HOME=/root", "PATH=/usr/bin", "LANG=en_US"},
		},
		{
			name:      "new key appended in override order",
			base:      []string{"HOME=/root"},
			overrides: []string{"DEBUG=1", "TRACE=0"},
			want:      []string{"HOME=/root", "DEBUG=1", "TRACE=0"},
		},
		{
			name:      "later override wins over earlier override",
			base:      nil,
			overrides: []string{"MODE=dev", "MODE=prod"},
			want:      []string{"MODE=prod"},
		},
		{
			name:      "no overrides returns base unchanged",
			base:      []string{"A=1", "B=2"},
			overrides: nil,
			want:      []string{"A=1", "B=2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Overlay(tc.base, tc.overrides...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Overlay(%v, %v) = %v, want %v", tc.base, tc.overrides, got, tc.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Overlay` is correct when every base key keeps its original slot unless an
override touches it, every genuinely new key lands in override order, and
the last write for any key — base or override — is what survives. The point
is that a variadic override list is exactly a small `[]string`, and treating
it as a set of key-value writes into a map (rather than blind concatenation)
is what avoids the duplicate-entry bug that `append(os.Environ(), extra...)`
invites in real subprocess code.

## Resources

- [`os/exec`: `Cmd.Env`](https://pkg.go.dev/os/exec#Cmd) — the real-world shape this exercise mirrors.
- [`os.Environ`](https://pkg.go.dev/os#Environ) — the `KEY=VALUE` slice format used as `base`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-kv-audit-line-builder.md](11-kv-audit-line-builder.md) | Next: [13-metric-tags-merger.md](13-metric-tags-merger.md)
