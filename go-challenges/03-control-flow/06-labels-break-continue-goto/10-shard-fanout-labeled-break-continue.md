# Exercise 10: Stop a shard fan-out the moment a key is found

**Nivel: Intermedio** — validacion rapida (un test corto).

A sharded key-value store fans a lookup out across shards in a fixed order. The
moment one shard produces a live match, querying the remaining shards is wasted
work. And if a shard reports mid-scan that its own data is corrupt, trusting any
record after that point — even one with the right key — would return garbage.

## What you'll build

```text
shardlookup/                independent module: example.com/shardlookup
  go.mod                     go 1.24
  shardlookup.go             Record, FindInShards
  shardlookup_test.go        table test: first/later shard, corruption, tombstone, no match
```

Set up the module:

```bash
mkdir -p ~/go-exercises/shardlookup
cd ~/go-exercises/shardlookup
go mod init example.com/shardlookup
go mod edit -go=1.24
```

Create `shardlookup.go`:

```go
package shardlookup

// Record is one entry inside a shard's local record list. Corrupt marks a
// sentinel position after which the shard's own data cannot be trusted for
// this read; Tombstoned marks a deleted key that must not match.
type Record struct {
	Key        string
	Value      string
	Tombstoned bool
	Corrupt    bool
}

// FindInShards scans shards in order, and within a shard scans records in
// order, looking for key. The moment a live (non-tombstoned) match is found
// the ENTIRE fan-out stops — later shards are never touched. If a shard
// reports a corruption sentinel partway through, any remaining records in
// that shard are discarded and the scan resumes at the NEXT shard, never at
// the next record of the corrupt one.
func FindInShards(shards [][]Record, key string) (value string, ok bool, shardIdx, recIdx int) {
	shardIdx, recIdx = -1, -1
search:
	for s, shard := range shards {
		for r, rec := range shard {
			if rec.Corrupt {
				// This shard cannot be trusted from here on. Abandon it
				// entirely and move straight to the next shard.
				continue search
			}
			if rec.Tombstoned {
				continue
			}
			if rec.Key == key {
				value, ok, shardIdx, recIdx = rec.Value, true, s, r
				break search
			}
		}
	}
	return value, ok, shardIdx, recIdx
}
```

### Why this needs a label, twice

Both decisions live inside the inner (records) loop, so a bare `break` or
`continue` there could never reach the shards loop. `break search` on a match
leaves both loops at once — the classic first-match-wins shape, but across
independent partitions rather than rows of one grid. `continue search` on a
corruption sentinel is the more interesting case: it discards the rest of the
CURRENT shard's records and resumes at the next shard's first record, not the
current shard's next record. A bare `continue` would do the latter, and a match
sitting right after the sentinel would wrongly win.

Create `shardlookup_test.go`:

```go
package shardlookup

import "testing"

func TestFindInShards(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		shards    [][]Record
		key       string
		wantValue string
		wantOK    bool
		wantShard int
		wantRec   int
	}{
		"match in first shard": {
			shards: [][]Record{
				{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}},
				{{Key: "b", Value: "99"}},
			},
			key: "b", wantValue: "2", wantOK: true, wantShard: 0, wantRec: 1,
		},
		"match only in later shard": {
			shards: [][]Record{
				{{Key: "a", Value: "1"}},
				{{Key: "b", Value: "2"}},
			},
			key: "b", wantValue: "2", wantOK: true, wantShard: 1, wantRec: 0,
		},
		"corrupt sentinel hides a later match in the same shard": {
			shards: [][]Record{
				{{Key: "x", Value: "0"}, {Corrupt: true}, {Key: "b", Value: "bad"}},
				{{Key: "b", Value: "good"}},
			},
			key: "b", wantValue: "good", wantOK: true, wantShard: 1, wantRec: 0,
		},
		"tombstoned record does not match": {
			shards: [][]Record{
				{{Key: "b", Value: "old", Tombstoned: true}},
				{{Key: "b", Value: "new"}},
			},
			key: "b", wantValue: "new", wantOK: true, wantShard: 1, wantRec: 0,
		},
		"no match anywhere": {
			shards: [][]Record{
				{{Key: "a", Value: "1"}},
				{{Key: "c", Value: "3"}},
			},
			key: "zzz", wantValue: "", wantOK: false, wantShard: -1, wantRec: -1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v, ok, s, r := FindInShards(tc.shards, tc.key)
			if v != tc.wantValue || ok != tc.wantOK || s != tc.wantShard || r != tc.wantRec {
				t.Fatalf("FindInShards(%q) = (%q,%v,%d,%d), want (%q,%v,%d,%d)",
					tc.key, v, ok, s, r, tc.wantValue, tc.wantOK, tc.wantShard, tc.wantRec)
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

`FindInShards` is correct when it returns the first live match in shard order
and stops looking immediately — the corruption test is the one that proves it:
a match value that sits right after a `Corrupt` record must never win, because
the labeled `continue` throws away the rest of that shard before it can be
reached. If you swap either label for a bare `break`/`continue`, the corruption
test or the "later shard" test breaks, because the search would keep chewing
through a shard it should have abandoned, or stop only the inner loop on a
match instead of the whole fan-out.

## Resources

- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — what a labeled `continue` targets.
- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — what a labeled `break` targets.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-two-way-merge-join-labeled-break.md](09-two-way-merge-join-labeled-break.md) | Next: [11-region-failover-labeled-continue-break.md](11-region-failover-labeled-continue-break.md)
