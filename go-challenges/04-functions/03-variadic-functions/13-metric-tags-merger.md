# Exercise 13: A Metric Tag Merger with Later-Wins Precedence

**Nivel: Intermedio** — validacion rapida (un test corto).

Metrics libraries (Datadog, StatsD, OpenTelemetry) all layer tags: a global
set from the process, a service-level set, and a per-call set, merged into
one before the metric is emitted. This module builds
`Merge(tagSets ...map[string]string) map[string]string`, a variadic of
*maps* rather than scalars — an unusual but real shape, and one where the
aliasing question is about shared maps, not shared slice backing arrays.

## What you'll build

```text
tagsmerge/                 independent module: example.com/tags-merge
  go.mod                   go 1.24
  tagsmerge.go              package tagsmerge; func Merge(tagSets ...map[string]string) map[string]string
  tagsmerge_test.go         table test: precedence, three layers, nil set, zero sets, no-mutation check
```

- Files: `tagsmerge.go`, `tagsmerge_test.go`.
- Implement: `Merge(tagSets ...map[string]string) map[string]string` combining any number of tag maps, later maps overriding earlier ones per key.
- Test: two-layer and three-layer precedence, a `nil` map among the sets, zero sets, and a dedicated case proving the caller's input maps are never mutated.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A variadic of maps, and why the output must be a fresh map

`tagSets ...map[string]string` is still exactly a slice under the hood —
`[]map[string]string` — but each element is itself a reference type. If
`Merge` tried to reuse one of the input maps as its accumulator (say,
`out := tagSets[0]`) and then wrote into it, every caller holding a
reference to that same map would see its tags silently change after a call
they didn't expect to mutate anything. So `Merge` always allocates its own
`out` map and only ever reads from `tagSets`, writing key by key in
iteration order: for maps, "later wins" means the last write to `out[k]`
survives, which is naturally whichever tag set was walked last. A `nil` map
argument is a normal, functioning input — ranging over a nil map is a
zero-iteration no-op, so it contributes nothing and never panics.

Create `tagsmerge.go`:

```go
// tagsmerge.go
package tagsmerge

// Merge combines any number of metric-tag maps into one, with later maps
// overriding earlier ones on conflicting keys. A nil map argument contributes
// nothing. Merge never mutates any of its input maps; it always builds and
// returns a fresh one.
func Merge(tagSets ...map[string]string) map[string]string {
	out := make(map[string]string)
	for _, tags := range tagSets {
		for k, v := range tags {
			out[k] = v
		}
	}
	return out
}
```

### Test

Create `tagsmerge_test.go`:

```go
// tagsmerge_test.go
package tagsmerge

import (
	"reflect"
	"testing"
)

func TestMerge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sets []map[string]string
		want map[string]string
	}{
		{
			name: "later set wins on conflict",
			sets: []map[string]string{
				{"env": "prod", "region": "us-east-1"},
				{"env": "staging"},
			},
			want: map[string]string{"env": "staging", "region": "us-east-1"},
		},
		{
			name: "three layers merge cleanly",
			sets: []map[string]string{
				{"service": "billing"},
				{"team": "payments"},
				{"service": "billing-v2"},
			},
			want: map[string]string{"service": "billing-v2", "team": "payments"},
		},
		{
			name: "nil map among the sets is skipped",
			sets: []map[string]string{
				{"a": "1"},
				nil,
				{"b": "2"},
			},
			want: map[string]string{"a": "1", "b": "2"},
		},
		{
			name: "zero sets returns empty map",
			sets: nil,
			want: map[string]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Merge(tc.sets...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Merge(%v) = %v, want %v", tc.sets, got, tc.want)
			}
		})
	}
}

func TestMergeDoesNotMutateInputs(t *testing.T) {
	t.Parallel()

	base := map[string]string{"env": "prod"}
	override := map[string]string{"env": "staging"}

	_ = Merge(base, override)

	if base["env"] != "prod" {
		t.Fatalf("base mutated: got %q, want %q", base["env"], "prod")
	}
	if override["env"] != "staging" {
		t.Fatalf("override mutated: got %q, want %q", override["env"], "staging")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Merge` is correct when the last tag set to define a key is the one that
survives, a `nil` set is a harmless no-op, zero sets yields an empty (never
nil) map, and — the property `TestMergeDoesNotMutateInputs` pins down —
none of the caller's maps are altered as a side effect. The senior point:
variadic elements are not always scalars or slices; when they are reference
types like maps, the aliasing risk shifts from "the callee appended into my
backing array" to "the callee wrote into my map", and the fix is the same
discipline — the callee builds its own output rather than adopting the
caller's memory.

## Resources

- [Go maps in action](https://go.dev/blog/maps) — map semantics, including that ranging over a nil map is safe and yields nothing.
- [`reflect.DeepEqual`](https://pkg.go.dev/reflect#DeepEqual) — comparing maps by content in tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-env-overlay-builder.md](12-env-overlay-builder.md) | Next: [14-csv-row-builder.md](14-csv-row-builder.md)
