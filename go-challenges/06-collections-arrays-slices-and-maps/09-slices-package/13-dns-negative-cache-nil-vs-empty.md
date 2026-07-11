# Exercise 13: Preserve Nil-Versus-Empty Across A DNS Negative Cache

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

RFC 2308 gives a DNS resolver two different ways to answer "no records here",
and a caching resolver like CoreDNS or Unbound has to keep them apart: NXDOMAIN
means the queried name does not exist at all, while NODATA means the name
exists but has no records of the requested type. The distinction matters
downstream -- an MX lookup that gets NODATA for a domain still knows the domain
is registered and can retry other record types, while NXDOMAIN means stop
asking. A resolver that queries several upstream sources for redundancy (a
primary and a fallback forwarder, or several authoritative servers during a
zone transfer) has to merge their answers for the same name into one cache
entry without losing which of the two negative states applies.

In Go, that distinction has nowhere else to live but a single bit of slice
metadata: whether `[]string` is `nil` or a non-nil, zero-length slice. That is
exactly the "nil versus empty is per-function" contract the `slices` package
documents case by case -- `Clone` preserves nilness, `Concat` and `Collect`
return nil for empty input, `Equal` treats nil and empty as the same value --
and a DNS merge is where getting this wrong stops being a formatting detail and
starts being a wrong answer cached for a TTL. This module builds the merge
function that keeps NXDOMAIN and NODATA apart on purpose, contrasted against
the one-line hand-rolled version that collapses them by accident.

This module is fully self-contained: its own `go mod init`, a reusable package,
and its tests. Nothing here imports another exercise.

## What you'll build

```text
dnscache/                 module example.com/dnscache
  go.mod                  go 1.24
  dnscache.go             Answer, State constants; MergeAnswers, Unchanged
  dnscache_test.go        merge table, aliasing, State table, Unchanged table, concurrency,
                          the naive-merge contrast, ExampleMergeAnswers
```

- Files: `dnscache.go`, `dnscache_test.go`.
- Implement: `type Answer struct { Name string; Records []string }` with `(Answer).State() string` classifying `Records` as `NXDOMAIN` (nil), `NODATA` (non-nil, empty), or `Answered` (populated); `MergeAnswers(sources ...[]string) []string` returning nil only when every source is nil, a non-nil empty slice when at least one source confirmed the name exists without contributing records, and the concatenation of every source's records otherwise; `Unchanged(prev, next []string) bool` wrapping `slices.Equal`.
- Test: the merge table (all NXDOMAIN, one NODATA among NXDOMAIN, records among NXDOMAIN, records from multiple sources, no sources, all NODATA); `MergeAnswers` never aliasing a source; the `State` table across all three classifications; the `Unchanged` table including nil-versus-empty; concurrent calls from many goroutines; the naive-merge contrast that collapses NXDOMAIN and NODATA; `ExampleMergeAnswers`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dnscache
cd ~/go-exercises/dnscache
go mod init example.com/dnscache
go mod edit -go=1.24
```

### Why the naive merge can't tell NXDOMAIN from NODATA

The merge every implementation starts out as looks unimpeachable:

```go
var out []string
for _, src := range sources {
    out = append(out, src...)
}
return out
```

It is correct whenever at least one source has real records -- `append` grows
`out` and returns a populated slice, same as `MergeAnswers` would. The bug is
in what happens when every source contributes zero elements. `append(out,
src...)` with zero elements to append is a no-op: it returns `out` completely
unchanged, whether `src` was `nil` (that source said NXDOMAIN) or a non-nil
empty slice (that source said NODATA). After the loop, `out` is `nil` either
way, and there is no way to look at that `nil` afterward and recover which
case actually happened. One source's NODATA answer -- "this name exists" --
silently vanishes into the same value as three sources all saying "this name
does not exist". The bug does not show up in a test that only checks the
records slice's *contents*; it shows up only when something downstream checks
`Records == nil` to decide whether to retry the query at all, and gets that
decision wrong.

`MergeAnswers` fixes this with one extra bit of state carried through the
loop: whether any source was non-nil at all, independent of whether it
contributed elements. That is the difference between "no source ever said
this name exists" (return nil, NXDOMAIN) and "some source said it exists,
just not with these records" (return a non-nil empty slice, NODATA).

Create `dnscache.go`:

```go
// Package dnscache merges DNS answers collected from several upstream
// resolvers for the same name, preserving the distinction RFC 2308 draws
// between two different negative answers: NXDOMAIN (the name does not
// exist) and NODATA (the name exists, but has no records of the requested
// type). That distinction is carried entirely by whether a []string is nil
// or a non-nil empty slice, so every function here documents which one it
// returns and why, the same way the slices package documents it per
// function rather than uniformly.
//
// Every function in this package is pure: none reads or writes shared
// state, so MergeAnswers, Unchanged, and Answer.State are all safe to call
// concurrently from any number of goroutines with no synchronization.
package dnscache

import "slices"

// negative-answer states an Answer's Records field can classify as. See
// Answer.State.
const (
	NXDOMAIN = "NXDOMAIN" // Records == nil: the name does not exist.
	NODATA   = "NODATA"   // Records != nil, len 0: the name exists, no matching records.
	Answered = "ANSWERED" // Records has at least one entry.
)

// Answer is the merged result for a single DNS name: the record set
// collected from every upstream source that answered, with the nil-versus-
// empty distinction of Records preserved as the signal for which RFC 2308
// negative-answer state applies.
type Answer struct {
	Name    string
	Records []string
}

// State classifies a.Records per RFC 2308: NXDOMAIN when Records is nil,
// NODATA when Records is non-nil but empty, Answered otherwise.
func (a Answer) State() string {
	switch {
	case a.Records == nil:
		return NXDOMAIN
	case len(a.Records) == 0:
		return NODATA
	default:
		return Answered
	}
}

// MergeAnswers combines the record sets several upstream sources returned
// for the same name, preserving nil-versus-empty as the negative-answer
// signal: MergeAnswers returns nil only if every source is nil (every
// upstream reported NXDOMAIN), a non-nil empty slice if at least one source
// confirmed the name exists but contributed no records (any source is
// NODATA or Answered with zero entries), and the concatenation of every
// source's records otherwise.
//
// The returned slice is freshly allocated; it never aliases any element of
// sources, so the caller may retain or mutate it without affecting the
// inputs.
func MergeAnswers(sources ...[]string) []string {
	var out []string
	sawExists := false
	for _, s := range sources {
		if s == nil {
			continue // this source said NXDOMAIN; it contributes nothing.
		}
		sawExists = true
		out = append(out, s...)
	}
	if out == nil && sawExists {
		return []string{} // NODATA: at least one source confirmed the name exists.
	}
	return out // nil (NXDOMAIN) or the concatenated records.
}

// Unchanged reports whether a fresh merge (next) has the same records as
// what is already cached (prev), so a caller can skip rewriting the cache
// entry when a refresh would not change it. Unchanged is a thin wrapper
// over slices.Equal, and inherits its contract exactly: nil and a non-nil
// empty slice compare equal, since slices.Equal defines equality by content
// (both are zero elements), not by nilness. A caller that must react to a
// state transition -- an NXDOMAIN entry becoming NODATA on refresh -- needs
// Answer.State on both sides for that; Unchanged answers only "same
// records", which is what deciding whether to rewrite the cache needs.
func Unchanged(prev, next []string) bool {
	return slices.Equal(prev, next)
}
```

### Using it

Call `MergeAnswers` once per name, after every configured upstream has
answered (or timed out and contributed `nil`), and wrap the result in an
`Answer` with the queried name to classify and cache. `State` is what a
caller checks before deciding what to do with a cache hit: NXDOMAIN means stop
querying, NODATA means the name is real but this record type is not, and
Answered means use `Records`. Every function in the package is pure and holds
no state, which is what the doc comment's concurrency claim rests on --
nothing here needs a mutex because nothing here has memory of its own.

`Unchanged` is the piece that keeps a refresh sweep cheap: call it with the
currently cached records and a fresh `MergeAnswers` result, and skip the
write when it reports `true`. Because it is built on `slices.Equal`, a
refresh that produces the exact same nil-versus-empty answer as before is
recognized as unchanged even though the two `[]string` values are not the
same allocation -- which is also why `Unchanged` alone cannot detect an
NXDOMAIN-to-NODATA transition on its own terms: both are "no records", and
telling them apart is `Answer.State`'s job, checked on both sides by the
caller when that transition matters.

`ExampleMergeAnswers` in the `_test.go` file is the runnable demonstration of
this module: `go test` executes it and compares its stdout against the
`// Output:` comment, so it cannot drift from the code it documents.

```go
func ExampleMergeAnswers() {
	nxdomain := MergeAnswers(nil, nil)
	fmt.Println(Answer{Name: "gone.example.", Records: nxdomain}.State())

	nodata := MergeAnswers(nil, []string{})
	fmt.Println(Answer{Name: "example.com.", Records: nodata}.State())

	answered := MergeAnswers([]string{"1.1.1.1"}, nil, []string{"1.0.0.1"})
	fmt.Println(Answer{Name: "cloudflare-dns.com.", Records: answered}.State(), answered)

	fmt.Println("refresh would skip:", Unchanged(answered, MergeAnswers([]string{"1.1.1.1"}, []string{"1.0.0.1"})))

	// Output:
	// NXDOMAIN
	// NODATA
	// ANSWERED [1.1.1.1 1.0.0.1]
	// refresh would skip: true
}
```

### Tests

`TestMergeAnswers` is the table across every combination of NXDOMAIN,
NODATA, and populated sources, including no sources at all and every source
being NODATA; each case checks both the records and the nilness explicitly,
because nilness is exactly what this function exists to get right.
`TestMergeAnswersNeverAliasesSources` pins the aliasing contract stated on
the function: mutating the merged result must never reach back into a
source. `TestAnswerState` and `TestUnchanged` table their own functions,
`TestUnchanged` specifically pinning that nil and empty compare equal, the
`slices.Equal` contract `Unchanged` inherits on purpose.
`TestMergeAnswersConcurrentUse` exercises the package doc's concurrency
claim with twenty goroutines calling `MergeAnswers` at once.

`TestNaiveMergeCollapsesNxdomainAndNodata` is the module's core test.
`mergeNaive` is unexported and unreachable from the package API; it is the
loop from the prose above. The test shows it returning the identical `nil`
result for "every source is NXDOMAIN" and "one source confirmed NODATA
among otherwise-NXDOMAIN sources" -- proving the collapse numerically rather
than asserting it -- and then shows `MergeAnswers` telling the same two
scenarios apart.

Create `dnscache_test.go`:

```go
package dnscache

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

// mergeNaive is the merge every hand-rolled version of this function starts
// out as: append everything from every source into one slice. It is not
// part of the package API. It looks correct because it is correct for the
// common case -- at least one source has real records -- and wrong in
// exactly the case this package exists for: when every source contributes
// zero elements, append(out, src...) never allocates regardless of whether
// src was nil (NXDOMAIN) or non-nil empty (NODATA), so out stays nil either
// way and the two negative-answer states become indistinguishable.
func mergeNaive(sources ...[]string) []string {
	var out []string
	for _, s := range sources {
		out = append(out, s...)
	}
	return out
}

func TestMergeAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sources [][]string
		want    []string
		wantNil bool
	}{
		{"every source is NXDOMAIN", [][]string{nil, nil, nil}, nil, true},
		{"one source confirms NODATA", [][]string{nil, {}, nil}, []string{}, false},
		{"one source has records, rest NXDOMAIN", [][]string{nil, {"1.1.1.1"}, nil}, []string{"1.1.1.1"}, false},
		{"records from two sources concatenate", [][]string{{"1.1.1.1"}, {"2.2.2.2"}}, []string{"1.1.1.1", "2.2.2.2"}, false},
		{"no sources at all", nil, nil, true},
		{"a source with records and an empty source", [][]string{{"1.1.1.1"}, {}}, []string{"1.1.1.1"}, false},
		{"three sources, all NODATA", [][]string{{}, {}, {}}, []string{}, false},
		{"single NXDOMAIN source", [][]string{nil}, nil, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := MergeAnswers(tc.sources...)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("MergeAnswers(%v) = %v, want %v", tc.sources, got, tc.want)
			}
			if (got == nil) != tc.wantNil {
				t.Fatalf("MergeAnswers(%v) nil = %v, want %v", tc.sources, got == nil, tc.wantNil)
			}
		})
	}
}

// TestMergeAnswersNeverAliasesSources pins the aliasing contract: mutating
// the returned slice must never reach back into any source slice.
func TestMergeAnswersNeverAliasesSources(t *testing.T) {
	t.Parallel()

	a := []string{"1.1.1.1"}
	got := MergeAnswers(a, []string{"2.2.2.2"})
	got[0] = "tampered"
	if a[0] != "1.1.1.1" {
		t.Fatalf("mutating the merged result changed a source slice: %q", a[0])
	}
}

func TestAnswerState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []string
		want    string
	}{
		{"nil is NXDOMAIN", nil, NXDOMAIN},
		{"empty is NODATA", []string{}, NODATA},
		{"populated is Answered", []string{"1.1.1.1"}, Answered},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := Answer{Name: "example.com", Records: tc.records}
			if got := a.State(); got != tc.want {
				t.Errorf("State() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		prev, next []string
		want       bool
	}{
		{"nil vs nil", nil, nil, true},
		{"nil vs empty are equal", nil, []string{}, true},
		{"same records", []string{"1.1.1.1"}, []string{"1.1.1.1"}, true},
		{"different records", []string{"1.1.1.1"}, []string{"2.2.2.2"}, false},
		{"records vs nil", []string{"1.1.1.1"}, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Unchanged(tc.prev, tc.next); got != tc.want {
				t.Errorf("Unchanged(%v, %v) = %v, want %v", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

// TestMergeAnswersConcurrentUse exercises the package doc's concurrency
// claim directly: many goroutines calling the pure functions in this
// package at once, none sharing any mutable state, must all see correct,
// independent results.
func TestMergeAnswersConcurrentUse(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.0.0.%d", i)
			got := MergeAnswers([]string{ip}, nil)
			if len(got) != 1 || got[0] != ip {
				t.Errorf("goroutine %d: MergeAnswers = %v, want [%s]", i, got, ip)
			}
		}(i)
	}
	wg.Wait()
}

// TestNaiveMergeCollapsesNxdomainAndNodata is the heart of the module. It
// shows mergeNaive returning bit-for-bit the same nil result for two
// upstream scenarios that must be cached differently: every source
// reporting NXDOMAIN, and one source confirming NODATA among otherwise
// NXDOMAIN sources. MergeAnswers tells them apart; mergeNaive cannot.
func TestNaiveMergeCollapsesNxdomainAndNodata(t *testing.T) {
	t.Parallel()

	allNxdomain := mergeNaive(nil, nil, nil)
	oneNodata := mergeNaive(nil, []string{}, nil)

	if allNxdomain != nil || oneNodata != nil {
		t.Fatalf("mergeNaive results = %v, %v; want both nil to demonstrate the collapse", allNxdomain, oneNodata)
	}
	if !slices.Equal(allNxdomain, oneNodata) {
		t.Fatal("mergeNaive no longer collapses the two states; test is stale")
	}

	// MergeAnswers keeps them apart.
	if got := MergeAnswers(nil, nil, nil); got != nil {
		t.Fatalf("MergeAnswers(all NXDOMAIN) = %v, want nil", got)
	}
	got := MergeAnswers(nil, []string{}, nil)
	if got == nil {
		t.Fatal("MergeAnswers(one NODATA source) = nil, want non-nil empty")
	}
	if len(got) != 0 {
		t.Fatalf("MergeAnswers(one NODATA source) = %v, want empty", got)
	}
}

// ExampleMergeAnswers is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleMergeAnswers() {
	nxdomain := MergeAnswers(nil, nil)
	fmt.Println(Answer{Name: "gone.example.", Records: nxdomain}.State())

	nodata := MergeAnswers(nil, []string{})
	fmt.Println(Answer{Name: "example.com.", Records: nodata}.State())

	answered := MergeAnswers([]string{"1.1.1.1"}, nil, []string{"1.0.0.1"})
	fmt.Println(Answer{Name: "cloudflare-dns.com.", Records: answered}.State(), answered)

	fmt.Println("refresh would skip:", Unchanged(answered, MergeAnswers([]string{"1.1.1.1"}, []string{"1.0.0.1"})))

	// Output:
	// NXDOMAIN
	// NODATA
	// ANSWERED [1.1.1.1 1.0.0.1]
	// refresh would skip: true
}
```

## Review

`MergeAnswers` is correct when it returns `nil` for exactly the case every
source reported NXDOMAIN, and a non-nil (possibly empty) slice whenever any
source confirmed the name exists -- the two states RFC 2308 defines and a
cache built on this package must never confuse. The trap it replaces is the
one-line hand-rolled merge that `append`s every source without tracking
whether any of them was non-nil: because appending zero elements to `nil` is
always a no-op regardless of whether the source was `nil` or empty,
`mergeNaive` returns identical output for "nobody has ever heard of this
name" and "this name exists but not with this record type", which
`TestNaiveMergeCollapsesNxdomainAndNodata` demonstrates directly. `Answer.State`
gives callers a name for each of the three outcomes, and `Unchanged`, a thin
`slices.Equal` wrapper, is the cheap refresh-skip check that inherits
`slices.Equal`'s own nil-versus-empty equality on purpose. Every function in
the package is pure and holds no state, so `TestMergeAnswersConcurrentUse`
confirms the doc comment's concurrency claim rather than merely stating it.
Run `go test -count=1 -race ./...` to confirm the merge and state tables, the
aliasing contract, the concurrency test, and the naive-merge contrast.

## Resources

- [RFC 2308](https://www.rfc-editor.org/rfc/rfc2308) — the NXDOMAIN/NODATA distinction this module preserves.
- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — the nil-versus-empty contract `Unchanged` is built on.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) / [`slices.Concat`](https://pkg.go.dev/slices#Concat) — two more functions in the package whose nil-versus-empty behavior differs by design, referenced in `00-concepts.md`.
- [Go Wiki: CodeReviewComments](https://go.dev/wiki/CodeReviewComments#declaring-empty-slices) — why nil and empty slices encode differently at an API boundary.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-sstable-order-guard-issortedfunc.md](12-sstable-order-guard-issortedfunc.md) | Next: [14-shard-slot-table-repeat-sentinel.md](14-shard-slot-table-repeat-sentinel.md)
