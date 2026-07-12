# Exercise 2: Wildcard Topic Subscriptions

Flat topic names force an awkward choice: either pour everything into one stream and make every consumer filter by hand, or mint a separate topic per concrete name and call `Subscribe` once for each. Hierarchical dotted subjects with wildcard patterns dissolve the dilemma. A publisher sends to a fully-specified subject like `orders.eu.created`; a subscriber registers a *pattern* like `orders.*` or `orders.#`, and the router fans each published subject out to every pattern it matches. This exercise builds that router and the matcher at its core as a self-contained module — it imports nothing from the other exercises, defines its own types, and ships its own demo and tests.

## What you'll build

```text
pubsub.go            Router, Subscription, Message; pattern matcher (* and #)
cmd/
  demo/
    main.go          subscribe three patterns, publish three subjects, show fan-out
pubsub_test.go       exhaustive match table, invalid-pattern rejection, concurrent publish
```

- Files: `pubsub.go`, `cmd/demo/main.go`, `pubsub_test.go`.
- Implement: `Router` (`Subscribe`, `Publish`, `Unsubscribe`), `Subscription` (`Collect`, `Pattern`), and the segment matcher behind them.
- Test: a table of subject/pattern pairs pins `*` (exactly one segment) and `#` (zero or more trailing segments); invalid patterns are rejected; concurrent publishers lose nothing.
- Verify: `go test -race ./...`

### Subjects, segments, and the meaning of the two wildcards

A subject is a dotted string — `orders.eu.created` — which the router treats as an ordered list of segments: `["orders", "eu", "created"]`. A pattern is the same shape, except two of its segments may be wildcards, and the two wildcards mean precisely different things.

The single-segment wildcard, written `*`, matches exactly one segment, no more and no fewer. `orders.*` matches `orders.created` and `orders.shipped` but not `orders` (too few segments) and not `orders.eu.created` (too many). It is the tool for "I care about every immediate child of `orders` but not their descendants."

The multi-segment wildcard, written `#`, matches zero or more trailing segments. `orders.#` matches `orders` itself, `orders.created`, and `orders.eu.created` alike — anything that begins with `orders`, including `orders` with nothing after it. Because "zero or more trailing" is only well-defined at the end of a pattern, `#` is legal *only* as the final segment; a pattern like `orders.#.created` has no coherent meaning and the router rejects it at subscription time. These are the AMQP topic-exchange and MQTT topic-filter conventions, and pinning down the "zero or more" boundary is the whole substance of the matcher.

### The matcher, segment by segment

The matcher walks the pattern segments in order against the subject segments. Four cases cover everything:

- The current pattern segment is `#`. Since `#` is guaranteed trailing, it absorbs whatever remains of the subject — including nothing — so the match succeeds immediately: `return true`.
- The subject has run out but the pattern has not (and the current segment is not `#`). The pattern demands a segment the subject cannot supply, so the match fails.
- The current pattern segment is `*`. It matches whatever single subject segment sits opposite it; advance both.
- Otherwise the pattern segment is a literal and must equal the subject segment exactly; if not, the match fails.

If the loop consumes every pattern segment without failing, one condition remains: the subject must also be fully consumed. A pattern of `orders.*` against `orders.eu.created` consumes both pattern segments but leaves `created` dangling in the subject, which is a non-match — `*` matched `eu`, and nothing is left in the pattern to match `created`. The final `len(pattern) == len(subject)` check captures exactly that "no leftovers on either side" requirement, and it is the line beginners most often forget, producing a matcher that says `orders.*` matches `orders.a.b.c`.

Create `pubsub.go`:

```go
package pubsub

import (
	"errors"
	"strings"
	"sync"
)

// ErrInvalidPattern is returned by Subscribe for a malformed subscription pattern:
// an empty pattern, an empty segment, or a '#' anywhere but the final segment.
var ErrInvalidPattern = errors.New("pubsub: invalid subscription pattern")

// defaultBuffer is the per-subscription channel capacity. Overflow handling is the
// subject of the slow-consumer exercise; here the buffer is sized so the demo and
// tests never fill it.
const defaultBuffer = 1024

// Message is a published record: a concrete dotted Subject and its Payload.
type Message struct {
	Subject string
	Payload []byte
}

// Subscription is a registered interest in subjects matching a pattern. Matching
// messages are delivered to its buffered channel; Collect drains them.
type Subscription struct {
	pattern  string
	segments []string
	ch       chan Message
}

// Pattern returns the pattern the subscription was registered with.
func (s *Subscription) Pattern() string { return s.pattern }

// Collect drains and returns every message currently buffered for the
// subscription, in delivery order, without blocking.
func (s *Subscription) Collect() []Message {
	var out []Message
	for {
		select {
		case m := <-s.ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// Router fans published subjects out to every subscription whose pattern matches.
type Router struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewRouter returns an empty Router.
func NewRouter() *Router {
	return &Router{subs: make(map[*Subscription]struct{})}
}

// Subscribe registers interest in subjects matching pattern. It returns
// ErrInvalidPattern if the pattern is empty, has an empty segment, or places '#'
// anywhere but the final segment.
func (r *Router) Subscribe(pattern string) (*Subscription, error) {
	segs, err := compilePattern(pattern)
	if err != nil {
		return nil, err
	}
	sub := &Subscription{
		pattern:  pattern,
		segments: segs,
		ch:       make(chan Message, defaultBuffer),
	}
	r.mu.Lock()
	r.subs[sub] = struct{}{}
	r.mu.Unlock()
	return sub, nil
}

// Unsubscribe removes a subscription from the router. Later publishes ignore it.
func (r *Router) Unsubscribe(sub *Subscription) {
	r.mu.Lock()
	delete(r.subs, sub)
	r.mu.Unlock()
}

// Publish delivers a message on subject to every matching subscription and returns
// how many subscriptions matched. A delivery to a subscription whose buffer is
// full is dropped (see the slow-consumer exercise for overflow policies).
func (r *Router) Publish(subject string, payload []byte) int {
	subjSegs := strings.Split(subject, ".")
	msg := Message{Subject: subject, Payload: payload}

	r.mu.RLock()
	defer r.mu.RUnlock()

	matched := 0
	for sub := range r.subs {
		if matchSegments(sub.segments, subjSegs) {
			matched++
			select {
			case sub.ch <- msg:
			default: // buffer full: drop rather than block the publisher
			}
		}
	}
	return matched
}

// compilePattern splits and validates a pattern into its segments.
func compilePattern(pattern string) ([]string, error) {
	if pattern == "" {
		return nil, ErrInvalidPattern
	}
	segs := strings.Split(pattern, ".")
	for i, seg := range segs {
		if seg == "" {
			return nil, ErrInvalidPattern
		}
		if seg == "#" && i != len(segs)-1 {
			return nil, ErrInvalidPattern // '#' is legal only as the final segment
		}
	}
	return segs, nil
}

// matchSegments reports whether the compiled pattern matches the subject segments.
//
//	'*' matches exactly one segment.
//	'#' matches zero or more trailing segments (it is always the final segment).
//
// A literal segment must be equal. With no '#', the lengths must be equal.
func matchSegments(pattern, subject []string) bool {
	for i, p := range pattern {
		if p == "#" {
			return true // absorbs the rest of the subject, including nothing
		}
		if i >= len(subject) {
			return false // pattern wants a segment the subject does not have
		}
		if p == "*" {
			continue // matches subject[i], whatever it is
		}
		if p != subject[i] {
			return false
		}
	}
	return len(pattern) == len(subject)
}
```

### The runnable demo

The demo registers three patterns of increasing specificity and publishes three subjects, printing the match count for each publish and then what each subscription actually collected. Trace it against the matcher: `orders.#` matches all three subjects; `orders.*` matches only the two-segment `orders.created`; `orders.eu.*` matches only `orders.eu.created`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pubsub"
)

func main() {
	r := pubsub.NewRouter()

	all, _ := r.Subscribe("orders.#")
	direct, _ := r.Subscribe("orders.*")
	eu, _ := r.Subscribe("orders.eu.*")

	for _, subject := range []string{"orders.created", "orders.eu.created", "orders.us.shipped"} {
		n := r.Publish(subject, []byte("payload"))
		fmt.Printf("publish %-18s -> %d subscriber(s)\n", subject, n)
	}

	for _, s := range []*pubsub.Subscription{all, direct, eu} {
		var subjects []string
		for _, m := range s.Collect() {
			subjects = append(subjects, m.Subject)
		}
		fmt.Printf("%-12q received %d: %v\n", s.Pattern(), len(subjects), subjects)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
publish orders.created     -> 2 subscriber(s)
publish orders.eu.created  -> 2 subscriber(s)
publish orders.us.shipped  -> 1 subscriber(s)
"orders.#"   received 3: [orders.created orders.eu.created orders.us.shipped]
"orders.*"   received 1: [orders.created]
"orders.eu.*" received 1: [orders.eu.created]
```

### Tests

`TestMatchSegments` is the centerpiece: a table that pins both wildcards against the cases people get wrong — `*` rejecting too-few and too-many segments, `#` accepting zero trailing segments, a bare `#` matching everything, and literals that must align exactly. `TestInvalidPattern` checks that the empty pattern, an empty segment, and a non-trailing `#` are rejected. `TestConcurrentPublishNoLoss` runs many publishers at once against a single catch-all subscriber and asserts every message is delivered, which is the property the router's locking exists to guarantee and which only `-race` can certify.

Create `pubsub_test.go`:

```go
package pubsub

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMatchSegments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		{"orders.created", "orders.created", true},
		{"orders.created", "orders.shipped", false},
		{"orders.*", "orders.created", true},
		{"orders.*", "orders", false},            // '*' needs exactly one segment
		{"orders.*", "orders.eu.created", false}, // '*' matches one, not two
		{"orders.#", "orders", true},             // '#' matches zero trailing
		{"orders.#", "orders.created", true},     // '#' matches one trailing
		{"orders.#", "orders.eu.created", true},  // '#' matches two trailing
		{"orders.#", "billing.created", false},   // literal prefix must align
		{"#", "anything.at.all", true},           // bare '#' matches everything
		{"#", "single", true},                    // including a single segment
		{"orders.eu.*", "orders.eu.created", true},
		{"orders.eu.*", "orders.us.created", false},
		{"orders.*.created", "orders.eu.created", true},
		{"orders.*.created", "orders.eu.shipped", false},
	}

	for _, tc := range cases {
		got := matchSegments(strings.Split(tc.pattern, "."), strings.Split(tc.subject, "."))
		if got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
}

func TestInvalidPattern(t *testing.T) {
	t.Parallel()

	r := NewRouter()
	for _, bad := range []string{"", "orders..created", "orders.#.created", "#.created"} {
		if _, err := r.Subscribe(bad); !errors.Is(err, ErrInvalidPattern) {
			t.Errorf("Subscribe(%q): err = %v, want ErrInvalidPattern", bad, err)
		}
	}
}

func TestFanOut(t *testing.T) {
	t.Parallel()

	r := NewRouter()
	all, _ := r.Subscribe("orders.#")
	direct, _ := r.Subscribe("orders.*")

	r.Publish("orders.created", nil)
	r.Publish("orders.eu.created", nil)

	if got := len(all.Collect()); got != 2 {
		t.Fatalf("orders.# collected %d, want 2", got)
	}
	if got := direct.Collect(); len(got) != 1 || got[0].Subject != "orders.created" {
		t.Fatalf("orders.* collected %v, want [orders.created]", got)
	}
}

func TestConcurrentPublishNoLoss(t *testing.T) {
	t.Parallel()

	r := NewRouter()
	sink, _ := r.Subscribe("#")

	const publishers = 8
	const each = 50
	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				r.Publish(fmt.Sprintf("evt.%d.%d", p, i), nil)
			}
		}(p)
	}
	wg.Wait()

	if got := len(sink.Collect()); got != publishers*each {
		t.Fatalf("catch-all collected %d, want %d", got, publishers*each)
	}
}
```

## Review

The matcher is correct when both wildcards behave at their boundaries: `*` rejects a subject with too few or too many segments, and `#` accepts a subject with zero trailing segments as readily as one with several. The two lines that carry that correctness are the early `return true` on `#` (which only means "zero or more trailing" because `compilePattern` forbids a non-trailing `#`) and the closing `len(pattern) == len(subject)` (which rejects the leftover-subject case that a naive prefix walk would wrongly accept). Confirm `compilePattern` rejects the empty pattern, empty segments from a doubled dot, and a `#` that is not last, so a malformed pattern never reaches the hot path.

The mistakes to avoid are concentrated in the matcher. Treating `#` as "exactly one trailing segment" breaks the zero-segment case (`orders.#` should match `orders`). Forgetting the final length check makes `orders.*` wrongly match `orders.a.b`. Allowing a non-trailing `#` produces a matcher whose answer depends on subject length rather than meaning. The fan-out and concurrency tests passing under `-race` confirm the router's `RWMutex` correctly guards the subscription set while publishers run in parallel.

## Resources

- [`strings.Split`](https://pkg.go.dev/strings#Split) — splits subjects and patterns into segments; note it yields one empty element for a doubled separator, which `compilePattern` rejects.
- [RabbitMQ Topic Exchanges](https://www.rabbitmq.com/tutorials/tutorial-five-go) — the AMQP `*` (one word) and `#` (zero or more words) routing-key semantics this matcher implements.
- [MQTT Topic Wildcards](https://www.hivemq.com/blog/mqtt-essentials-part-5-mqtt-topics-best-practices/) — the single-level (`+`) and multi-level (`#`) filter conventions and why `#` must be last.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-broker-core.md](01-broker-core.md) | Next: [03-slow-consumer-policy.md](03-slow-consumer-policy.md)
