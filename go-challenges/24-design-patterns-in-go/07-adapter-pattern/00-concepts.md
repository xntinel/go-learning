# 7. Adapter Pattern — Concepts

Third-party libraries and external services expose the interfaces that are convenient for them. Your domain defines the interface that is convenient for you. The adapter pattern is the bridge between the two: a small type that holds a foreign client and implements your domain interface by translating each call across the gap. The payoff is isolation. When the vendor renames a method, switches from cents to a decimal type, deprecates an endpoint, or changes how it reports failure, exactly one file moves; every caller keeps invoking the same method on the same interface. This file is the conceptual foundation. Read it once and you will have the reasoning behind each exercise, which builds an adapter from a different angle as an independent, self-contained Go module: a multi-channel notifier over three incompatible SDKs, an anti-corruption layer over a payment gateway whose error model is alien to Go, and a pair of adapters that bolt your domain stream types onto the standard `io.Reader` and `io.Writer` seams.

## Concepts

### The Domain Interface Is The Boundary

The lesson always starts from the domain, never from the vendor. The first type you write is the port: the interface your own code will depend on, phrased entirely in your own vocabulary.

```go
type Notifier interface {
	Send(ctx context.Context, recipient, message string) error
}
```

`Send(ctx, recipient, message) error` says nothing about SendGrid, Twilio, or Slack. That is the whole point. Because the caller depends on `Notifier` and not on any concrete client, you can swap `SendGridClient` for `MailgunClient` without touching a single call site. This is hexagonal architecture expressed in Go's interface syntax: the domain owns the port, and an adapter owns the plug that fits the port on one side and the vendor on the other. Go has no inheritance, so every Go adapter is an *object adapter*: it holds the foreign value as a field and composes, rather than extending a foreign base class. Composition is the only tool, and it is the better one, because it lets a single adapter wrap clients from several vendors or even wrap other adapters.

### An Adapter Holds The Foreign Client And Implements The Port

The mechanical shape of every adapter in this lesson is identical: a struct with the third-party client as an unexported field, a constructor that validates its dependencies, and one method per port operation whose body translates the call.

```go
type EmailAdapter struct {
	client    *sendgrid.SendGridClient
	fromEmail string
}

func (a *EmailAdapter) Send(ctx context.Context, recipient, message string) error {
	if recipient == "" {
		return ErrEmptyRecipient
	}
	if _, err := a.client.SendEmail(ctx, a.fromEmail, recipient, "Notification", message); err != nil {
		return fmt.Errorf("send email to %s: %w", recipient, err)
	}
	return nil
}
```

The adapter is the *only* place in the program that knows the vendor's argument order, its naming, its return shape, and its quirks. `SendEmail` takes a subject the domain never mentions; the adapter supplies a constant. A mistake about the vendor's API cannot leak past this method, because nothing else imports the vendor package. That containment is the property you are buying.

### Translating The Failure Model, Not Just The Method Name

The shallowest form of adaptation is renaming: the vendor calls it `CreateMessage`, you call it `Send`. The deeper and more valuable work is reconciling *failure models*, because different SDKs report failure in incompatible ways. One returns an idiomatic `error`. Another returns a custom error type with a numeric `Code` field. A third performs an HTTP POST and hands back a status integer. A fourth — common in older or non-Go-native SDKs — never returns an `error` at all and instead packs the outcome into a result struct with a `StatusCode` field that the caller is expected to inspect.

The adapter's job is to collapse all of those into Go's one true failure model: a returned `error` that the domain can interrogate with `errors.Is` and `errors.As`. Three techniques cover every case.

Wrap with `%w` to preserve the chain. When the vendor already returns an `error`, wrap it with `fmt.Errorf("...: %w", err)` rather than returning it verbatim or flattening it with `%v`. The `%w` verb keeps the original reachable, so a test or a caller can still do `errors.Is(err, sendgrid.ErrInvalidFrom)` or `errors.As(err, &twilioErr)` even though the error has travelled through your wrapper. The wrapper adds context (which recipient, which channel) without erasing the cause.

Map status codes to sentinels. When the vendor reports failure as a number, the adapter owns a `switch` that turns each meaningful code into a domain sentinel: `4020` becomes a declined-payment error, `5000` becomes a gateway-unavailable error, and an unrecognized code falls through to a catch-all so a new vendor status can never be silently swallowed. The numbers stop at the adapter; the rest of the program reasons in named errors.

Carry structure where the caller needs it. Sometimes a category sentinel is not enough — a declined charge needs both "this matched the declined category" (for `errors.Is`) and "the reason code was 4020" (for logging or retry logic). A custom error type can serve both at once by implementing `Is`:

```go
type DeclineError struct {
	Code   int
	Reason string
}

func (e *DeclineError) Is(target error) bool { return target == ErrDeclined }
```

Now `errors.Is(err, ErrDeclined)` succeeds for any `*DeclineError`, and `errors.As(err, &de)` recovers the structured `Code`. The domain gets a clean sentinel to branch on and the operator gets the detail, all from one value the adapter constructed.

### Translating Data And Units

Failure is not the only mismatch. Vendors disagree about how to represent the same value. A payment gateway works in integer minor units (cents); your domain may model money as a whole part plus a 0..99 minor part, or as a decimal type, or as a currency-tagged struct. The conversion — `whole*100 + cents`, with a bounds check that rejects a nonsensical 150-cent value before any network call — belongs in the adapter, never in the caller. Pushing the conversion into the adapter means a single, tested place owns the arithmetic, and a caller can never accidentally send dollars where the vendor expected cents. The same applies to currency codes, timestamps (epoch seconds versus `time.Time`), and IDs (the vendor's opaque reference versus your `TransactionID`). The adapter is a translation membrane in both directions: domain values in, vendor values out on the way down; vendor values in, domain values out on the way back.

### The io.Reader / io.Writer Seam

The most reused interfaces in all of Go are `io.Reader` and `io.Writer`. Satisfy them and your type instantly composes with `io.Copy`, `bufio.Scanner`, `compress/gzip`, `encoding/json`, `net/http` bodies, and hundreds of other functions that speak bytes. Writing an adapter *toward* these standard interfaces — rather than toward a vendor — is one of the highest-leverage moves in the language, because the "other side" of the adapter is the entire standard library.

Two directions matter. To let the standard library *read* from a domain producer, wrap a pull-based source (something with a `Next() (line, ok)` method) in a type whose `Read([]byte)` pulls lines, appends newlines, and buffers the unread tail between calls. To let the standard library *write* into a domain consumer, wrap a push-based sink (something with `WriteLine(string)`) in a type whose `Write([]byte)` splits on `'\n'` and forwards each complete line, holding a trailing partial line until the next newline or an explicit `Flush`.

These two interfaces come with contracts that the adapter must honor, and the contracts are exactly where the subtlety lives.

`io.Reader.Read` must return `io.EOF` only when the stream is genuinely exhausted, and it may return a short read (fewer bytes than `len(p)`) with a `nil` error; callers are required to handle that. A correct adapter therefore loops to fill `p` as far as it can, returns `n, nil` while data remains, and returns `0, io.EOF` only once the source is drained and no buffered bytes are left. Crucially, the adapter must keep the leftover bytes of a line that did not fit in `p`, because the next `Read` will be called with a fresh buffer and must continue where it stopped.

`io.Writer.Write` must process every byte of `p` or return an error, and it must not retain `p` after returning (copy what you need). The documented rule is one-directional: if `Write` returns `n < len(p)` it must return a non-nil error. The reverse is not forbidden, so an adapter that copies all of `p` into an internal buffer may legitimately report `n == len(p)` and still surface a downstream sink error — the bytes were accepted into the buffer even though a later line failed to flush. Stating that choice explicitly is part of writing the adapter honestly.

### Composition Adapters Wrap Other Adapters

Because an adapter is just a type that implements the port, an adapter can hold *other* adapters. A `MultiNotifier` implements `Notifier` by holding a slice of `Notifier` and fanning a single `Send` out to all of them.

```go
func (m *MultiNotifier) Send(ctx context.Context, recipient, message string) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.Send(ctx, recipient, message); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

`errors.Join` returns `nil` when every element is `nil`, so a fully successful fan-out is still `nil` and the happy path stays clean. A partial failure returns a single joined error that `errors.Is` can still walk to find any one channel's sentinel. The composite is indistinguishable from a leaf adapter at the call site, which is what makes the pattern compose without limit.

### The Anti-Corruption Layer

"Anti-corruption layer" is the architectural name for what a disciplined set of adapters achieves: external change does not propagate inward. When a vendor deprecates `SendEmail` in favor of `SendEmailV2`, the edit is confined to the adapter body. When the gateway adds a new decline code, the edit is confined to the `switch`. When you replace one vendor with a competitor, you write a new adapter that satisfies the same port and change one line of wiring. The domain — the part of the system that encodes what your business actually does — never learns that any of this happened. The cost is real and visible (a struct, a constructor, a method, a test) and you pay it deliberately. The benefit is also real: the boundary holds.

### When To Use An Adapter Versus A Direct Call

The decision rule is about reach. If a foreign type would otherwise cross one of your package boundaries — appear in a function signature, a struct field, a returned value that other packages consume — write an adapter so the foreign type stops at the edge. If a foreign value is used in exactly one place and never escapes it, a direct call is fine and an adapter is ceremony. The trade-off is concrete: an adapter costs a file and a method and a test; it pays off only when there is at least one consumer on the far side of the boundary it protects. Prefer the standard library inside the adapter, too: reach for `strconv.FormatInt`, `http.StatusText`, and the `io` interfaces rather than re-implementing conversions a vendor or the runtime already provides.

## Common Mistakes

### Defining The Port Around The Vendor

Wrong: declaring `type Notifier interface { SendEmail(from, to, subject, body string) (Response, error) }` so the interface matches `SendGridClient` method-for-method.

What happens: the interface is now shaped like one vendor. The day you switch vendors or the vendor renames a method, every caller breaks, and the adapter you wrote to insulate them insulates nothing.

Fix: derive the port from the domain's vocabulary (`Send(ctx, recipient, message) error`). The adapter, and only the adapter, knows the vendor's names and argument order.

### Letting The Foreign Error Or Type Escape

Wrong: `return c.SendEmail(...)` straight through, or storing a `*twilio.Error` in a domain struct.

What happens: now every package that wants to react to the failure must import the vendor package to name its error type. The vendor has leaked across the boundary the adapter was supposed to enforce, and removing the vendor later means touching all of those packages.

Fix: wrap the vendor error with `%w` so it stays reachable through `errors.Is`/`errors.As`, but keep the vendor's *type* inside the adapter package. Callers branch on your sentinels, not on the vendor's types.

### Ignoring A Non-Idiomatic Failure Signal

Wrong: calling a vendor whose method returns a result struct with a `StatusCode` field and treating the absence of a Go `error` as success, so a `4020` "insufficient funds" outcome is read as an approved charge.

What happens: failures masquerade as successes. A declined payment looks settled; a 5xx outage looks like a completed request. The bug is silent and expensive.

Fix: the adapter must inspect the vendor's actual success signal (here, an `Accepted` flag and a `StatusCode`) and translate every failure code into a domain error, with a catch-all sentinel so an unrecognized code is surfaced rather than swallowed.

### Violating The io.Reader / io.Writer Contract

Wrong: a `Read` that returns `0, nil` when the source is temporarily empty (spinning the caller forever), or that returns the final bytes together with `io.EOF` and then forgets them, or that drops the unread tail of a line that did not fit in `p`.

What happens: `io.Copy` and `bufio.Scanner` either loop forever, lose the last record, or interleave garbage, because they rely on the documented contract precisely.

Fix: loop to fill `p`, return `n, nil` while bytes remain, return `0, io.EOF` only when truly drained, and persist the leftover bytes of a partially copied line in a buffer field so the next `Read` resumes correctly. On the write side, split on `'\n'`, buffer the trailing partial line, copy out of `p` (never retain it), and expose a `Flush` for the final unterminated line.

### Wrapping A Single Adapter In A Composite For No Reason

Wrong: `multi, _ := NewMultiNotifier(email)` and then calling `multi.Send` for a lone channel.

What happens: an extra allocation, an `errors.Join` that always joins zero or one error, and a stack frame that obscures the real call site, all for a fan-out of one.

Fix: use the single adapter directly. The composite exists for fanning out to many sinks; for one sink it is pure overhead.

---

Next: [01-notifier-adapters.md](01-notifier-adapters.md)
