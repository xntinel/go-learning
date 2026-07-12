# Exercise 11: Promote the first region that passes every health check

**Nivel: Intermedio** — validacion rapida (un test corto).

A failover controller probes regions in priority order. A region is only a
candidate if ALL of its endpoints are healthy; the instant one endpoint fails,
checking the rest of that region is pointless — it is already disqualified. The
first region that clears every endpoint is promoted, and no lower-priority
region is probed at all.

## What you'll build

```text
failover/                    independent module: example.com/failover
  go.mod                     go 1.24
  failover.go                Endpoint, Region, FirstHealthyRegion
  failover_test.go           table test: disqualify, promote, empty region, no candidate
```

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/06-labels-break-continue-goto/11-region-failover-labeled-continue-break
cd go-solutions/03-control-flow/06-labels-break-continue-goto/11-region-failover-labeled-continue-break
go mod edit -go=1.24
```

Create `failover.go`:

```go
package failover

// Endpoint is one health check inside a region's probe set.
type Endpoint struct {
	Name    string
	Healthy bool
}

// Region groups the endpoints that must ALL be healthy before the region
// can take traffic.
type Region struct {
	Name      string
	Endpoints []Endpoint
}

// FirstHealthyRegion probes regions in priority order. The first endpoint
// that fails disqualifies its whole region — the scan abandons the REST of
// that region's endpoints and moves to the next region. A region whose
// endpoints are all healthy is promoted immediately, and no further region
// is probed.
func FirstHealthyRegion(regions []Region) (name string, ok bool, regionIdx int) {
	regionIdx = -1
probe:
	for i, region := range regions {
		if len(region.Endpoints) == 0 {
			continue
		}
		for j, ep := range region.Endpoints {
			if !ep.Healthy {
				// This region is out; do not check its remaining endpoints.
				continue probe
			}
			if j == len(region.Endpoints)-1 {
				// Reached the last endpoint without a single failure.
				name, ok, regionIdx = region.Name, true, i
				break probe
			}
		}
	}
	return name, ok, regionIdx
}
```

### Why a bare continue would be a production bug here

Both `continue probe` and `break probe` are issued from inside the endpoints
loop, so a bare `continue`/`break` there could never reach the regions loop —
it would only affect the current region's own endpoint scan. That distinction
is not cosmetic: if a region's first endpoint fails but its LAST endpoint is
healthy, a bare `continue` would land on that healthy last endpoint and wrongly
promote a region that has a known failure. `continue probe` guarantees a single
failed endpoint disqualifies the whole region, no matter where in the list it
sits.

Create `failover_test.go`:

```go
package failover

import "testing"

func TestFirstHealthyRegion(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		regions       []Region
		wantName      string
		wantOK        bool
		wantRegionIdx int
	}{
		"first region fully healthy": {
			regions: []Region{
				{Name: "us-east", Endpoints: []Endpoint{{Healthy: true}, {Healthy: true}}},
				{Name: "us-west", Endpoints: []Endpoint{{Healthy: true}}},
			},
			wantName: "us-east", wantOK: true, wantRegionIdx: 0,
		},
		"first region disqualified, second promoted": {
			regions: []Region{
				{Name: "us-east", Endpoints: []Endpoint{{Healthy: false}, {Healthy: true}}},
				{Name: "us-west", Endpoints: []Endpoint{{Healthy: true}}},
			},
			wantName: "us-west", wantOK: true, wantRegionIdx: 1,
		},
		"unlabeled continue would wrongly promote a disqualified region": {
			// Region 0: first endpoint unhealthy, LAST endpoint healthy. A
			// bare (unlabeled) continue here would only skip to the next
			// endpoint of the SAME region, land on the healthy last one, and
			// wrongly promote a region that has a failing endpoint. The
			// labeled continue must abandon the whole region instead.
			regions: []Region{
				{Name: "us-east", Endpoints: []Endpoint{{Healthy: false}, {Healthy: true}}},
				{Name: "us-west", Endpoints: []Endpoint{{Healthy: true}, {Healthy: true}}},
			},
			wantName: "us-west", wantOK: true, wantRegionIdx: 1,
		},
		"no region fully healthy": {
			regions: []Region{
				{Name: "us-east", Endpoints: []Endpoint{{Healthy: false}}},
				{Name: "us-west", Endpoints: []Endpoint{{Healthy: true}, {Healthy: false}}},
			},
			wantName: "", wantOK: false, wantRegionIdx: -1,
		},
		"region with no endpoints is skipped, not promoted": {
			regions: []Region{
				{Name: "empty", Endpoints: nil},
				{Name: "us-west", Endpoints: []Endpoint{{Healthy: true}}},
			},
			wantName: "us-west", wantOK: true, wantRegionIdx: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			gotName, gotOK, gotIdx := FirstHealthyRegion(tc.regions)
			if gotName != tc.wantName || gotOK != tc.wantOK || gotIdx != tc.wantRegionIdx {
				t.Fatalf("FirstHealthyRegion() = (%q,%v,%d), want (%q,%v,%d)",
					gotName, gotOK, gotIdx, tc.wantName, tc.wantOK, tc.wantRegionIdx)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The controller is correct when the promoted region is the first one in
priority order with zero failing endpoints, and disqualified regions never
influence the result even when their last endpoint happens to be healthy. The
"unlabeled continue" test case is the one to focus on: it plants a healthy
final endpoint behind a failing earlier one specifically to catch the bug a
bare `continue` would introduce. `break probe` is the mirror case — reached
only when the inner loop walks every endpoint of a region without ever hitting
the `continue probe` branch.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — labeled statements and their scope.
- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` must name an enclosing `for`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-shard-fanout-labeled-break-continue.md](10-shard-fanout-labeled-break-continue.md) | Next: [12-settlement-batch-poison-abort.md](12-settlement-batch-poison-abort.md)
