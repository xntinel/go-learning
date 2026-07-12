# Exercise 30: Consensus Voting with Weighted Results and Quorum

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye casos borde).

Not every vote should count the same, and not every vote count should
count as a decision. `Aggregate` folds a slice of weighted votes into a
per-choice tally and only declares a winner when two independent
conditions both hold: enough total weight was cast to reach quorum, and
exactly one choice holds the strict top tally. Either failing means "no
decision," not a guess.

## What you'll build

```text
consensus/                   independent module: example.com/consensus
  go.mod                     go 1.24
  consensus.go                type Vote; func Aggregate
  consensus_test.go            table: clear winner, below quorum, boundary, ties, empty
  cmd/demo/
    main.go                  aggregates three vote sets against different quora
```

- Files: `consensus.go`, `consensus_test.go`, `cmd/demo/main.go`.
- Implement: `Vote struct{ Choice string; Weight float64 }` and `Aggregate(votes []Vote, quorum float64) (winner string, ok bool)`.
- Test (table-driven): a clear winner above quorum decides; the same votes below quorum never decide, regardless of margin; total weight exactly at the quorum boundary decides; a two-way tie at the top is not a decision; a three-way tie is not a decision; an empty vote slice never reaches quorum; many small-weight votes for one choice can outweigh one large-weight vote for another.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/30-consensus-aggregate-with-quorum/cmd/demo
cd go-solutions/04-functions/10-higher-order-functions/30-consensus-aggregate-with-quorum
go mod edit -go=1.24
```

### Two thresholds, not one

It is tempting to write "aggregate votes, return whichever choice has the
most weight" as a single pass. `Aggregate` deliberately keeps two
separate gates, checked in order: first, does `total` — the sum of every
vote's weight, regardless of choice — meet `quorum`? If not, return
immediately; a 10-3 landslide among three voters out of a required
twenty is not a decision, it is an unrepresentative sample. Only once
quorum is satisfied does the second question matter: among the choices
that were voted for, is there a single strict maximum? A tie at the top —
even a tie that clears quorum easily — is deliberately treated the same
as not having enough information: `ties != 1` covers both "nobody voted"
(`ties == 0`, an empty tally) and "more than one choice is equally
ahead" (`ties > 1`) with the same check, because both are cases where
picking a winner would be inventing certainty that is not there.

The whole tally is built with a single fold over `votes` — one pass,
accumulating both the per-choice weight map and the running `total` at
the same time — which is what "reduces votes... weighted by priority"
means concretely: no separate pass to sum weights and another to group
by choice.

Create `consensus.go`:

```go
package consensus

// Vote is one source's opinion, weighted by that source's priority (a
// higher-trust source might carry more weight than a lower-trust one).
// Weight must be positive; a non-positive weight is a malformed vote and
// is not a case this package tries to make meaningful.
type Vote struct {
	Choice string
	Weight float64
}

// Aggregate folds votes into a weight tally per choice and decides a
// winner. It requires two conditions before declaring one: the total
// weight cast must reach quorum, and exactly one choice must hold the
// strictly highest tally — a tie at the top is not a decision. When
// either condition fails, ok is false and winner is "".
func Aggregate(votes []Vote, quorum float64) (winner string, ok bool) {
	tally := make(map[string]float64)
	var total float64
	for _, v := range votes {
		tally[v.Choice] += v.Weight
		total += v.Weight
	}

	if total < quorum {
		return "", false
	}

	var max float64
	ties := 0
	for choice, weight := range tally {
		switch {
		case weight > max:
			max = weight
			winner = choice
			ties = 1
		case weight == max:
			ties++
		}
	}

	if ties != 1 {
		return "", false
	}
	return winner, true
}
```

### The runnable demo

The demo runs the same five votes against two different quorum
thresholds — one it clears, one it does not — and then a separate,
deliberately tied vote set to show a clean tie deciding nothing even with
a trivially satisfied quorum.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/consensus"
)

func main() {
	votes := []consensus.Vote{
		{Choice: "promote", Weight: 3},
		{Choice: "promote", Weight: 2},
		{Choice: "hold", Weight: 1},
	}

	winner, ok := consensus.Aggregate(votes, 5)
	fmt.Printf("total weight 6, quorum 5: winner=%q ok=%v\n", winner, ok)

	winner, ok = consensus.Aggregate(votes, 7)
	fmt.Printf("total weight 6, quorum 7: winner=%q ok=%v\n", winner, ok)

	tied := []consensus.Vote{
		{Choice: "promote", Weight: 3},
		{Choice: "hold", Weight: 3},
	}
	winner, ok = consensus.Aggregate(tied, 1)
	fmt.Printf("tied 3-3, quorum 1: winner=%q ok=%v\n", winner, ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total weight 6, quorum 5: winner="promote" ok=true
total weight 6, quorum 7: winner="" ok=false
tied 3-3, quorum 1: winner="" ok=false
```

The first case's total weight of 6 clears a quorum of 5 and `promote`
holds a strict majority (5 vs. 1), so it decides. Raising the quorum to
7 makes the same votes fall short — the outcome is identical to the tie
case below in one respect: neither reaches a decision, but for entirely
different reasons, which is exactly why the two gates are checked
separately.

### Tests

The table covers both gates independently and their edge cases.
`"clear winner above quorum"` and `"below quorum never decides regardless
of margin"` use the *same* vote distribution with two different quora,
isolating the quorum gate from the winner-selection gate entirely.
`"total weight exactly at quorum boundary decides"` pins down that the
comparison is inclusive (`total < quorum` fails to disqualify a total
that exactly equals quorum). The two tie cases prove `ties != 1` catches
both a two-way and a three-way tie. `"no votes never reaches quorum"`
guards the empty-input path, and the last case proves weight is summed
per choice across multiple votes, not just taken from a single vote's
weight.

Create `consensus_test.go`:

```go
package consensus

import "testing"

func TestAggregate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		votes      []Vote
		quorum     float64
		wantWinner string
		wantOK     bool
	}{
		{
			name:       "clear winner above quorum",
			votes:      []Vote{{"promote", 3}, {"promote", 2}, {"hold", 1}},
			quorum:     5,
			wantWinner: "promote",
			wantOK:     true,
		},
		{
			name:       "below quorum never decides regardless of margin",
			votes:      []Vote{{"promote", 3}, {"promote", 2}, {"hold", 1}},
			quorum:     7,
			wantWinner: "",
			wantOK:     false,
		},
		{
			name:       "total weight exactly at quorum boundary decides",
			votes:      []Vote{{"promote", 5}},
			quorum:     5,
			wantWinner: "promote",
			wantOK:     true,
		},
		{
			name:       "tie at the top is not a decision",
			votes:      []Vote{{"promote", 3}, {"hold", 3}},
			quorum:     1,
			wantWinner: "",
			wantOK:     false,
		},
		{
			name:       "three-way tie is not a decision",
			votes:      []Vote{{"a", 2}, {"b", 2}, {"c", 2}},
			quorum:     1,
			wantWinner: "",
			wantOK:     false,
		},
		{
			name:       "no votes never reaches quorum",
			votes:      nil,
			quorum:     0,
			wantWinner: "",
			wantOK:     false,
		},
		{
			name:       "many small weighted sources outweigh one large source",
			votes:      []Vote{{"a", 10}, {"b", 4}, {"b", 4}, {"b", 4}},
			quorum:     10,
			wantWinner: "b",
			wantOK:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			winner, ok := Aggregate(tc.votes, tc.quorum)
			if winner != tc.wantWinner || ok != tc.wantOK {
				t.Fatalf("Aggregate(%v, %v) = (%q, %v), want (%q, %v)",
					tc.votes, tc.quorum, winner, ok, tc.wantWinner, tc.wantOK)
			}
		})
	}
}
```

## Review

`Aggregate` is correct because the quorum gate and the tie gate are
checked as two genuinely separate questions in sequence, not folded into
one comparison — a common bug in ad hoc "majority wins" code is to skip
the quorum check when there is a clear leader, which quietly turns "not
enough people voted" into "the loudest three voters decided for
everyone." The `ties` counter is the other subtlety: initializing `max`
to zero and counting `weight == max` on the very first iteration works
here only because `Vote.Weight` is documented as always positive — with
zero or negative weights allowed, the very first choice would spuriously
tie against the untouched `max`, which is exactly why that assumption is
stated on the type instead of left implicit. `total < quorum` (not
`<=`) is what makes a total exactly at the boundary count as reaching
quorum; get that comparison backward and every boundary case in the
table silently flips.

## Resources

- [Go spec: For statements](https://go.dev/ref/spec#For_statements) — the single-pass fold building `tally` and `total` together.
- [maps package](https://pkg.go.dev/maps) — utilities for working with the per-choice tally.
- [Raft consensus algorithm](https://raft.github.io/) — a production consensus protocol built on the same "quorum of votes decides" foundation, generalized to leader election and log replication.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-router-priority-chain-with-fallback.md](29-router-priority-chain-with-fallback.md) | Next: [31-result-wrapper-timing-and-metadata.md](31-result-wrapper-timing-and-metadata.md)
