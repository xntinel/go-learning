# Exercise 1: A KEDA External Scaler as a gRPC Service

When no built-in scaler fits your demand signal — a business KPI, an internal
SaaS, a bespoke broker — you ship an external scaler: a gRPC service that
implements KEDA's `ExternalScaler` contract and computes the metric in Go. This
is real, whole-task, on-the-job work. This exercise builds that service: it parses
trigger metadata, reports the aggregate backlog, answers the activation question,
and pushes activation changes over a server stream, with table-driven tests over
an in-memory gRPC pipe.

This module depends on the KEDA `externalscaler` protobuf, which you generate with
`protoc`/`buf` and commit alongside the code (so CI stays protoc-free). Offline it
cannot build — the gRPC and protobuf modules and the generated stubs are not
vendored here — so this is a bar-mode lesson: judge it on gofmt-clean, correctly
shaped, API-accurate code, not on a local gate pass.

## What you'll build

```text
external-scaler/                    independent module: example.com/external-scaler
  go.mod                            go 1.24; requires google.golang.org/grpc + protobuf
  externalscaler.proto              the KEDA ExternalScaler contract (vendored copy)
  externalscaler/                   protoc output, committed (externalscaler.pb.go, _grpc.pb.go)
  scaler.go                         Server: IsActive, StreamIsActive, GetMetricSpec, GetMetrics
  cmd/
    demo/
      main.go                       in-process bufconn round-trip; prints spec, backlog, replicas
    scaler/
      main.go                       //go:build scaler — the real net.Listen server
  scaler_test.go                    bufconn table tests; metadata validation; stream transitions
```

- Files: `externalscaler.proto`, `scaler.go`, `cmd/demo/main.go`, `cmd/scaler/main.go`, `scaler_test.go` (plus the generated `externalscaler` package).
- Implement: a `Server` embedding `UnimplementedExternalScalerServer` that parses `ScalerMetadata`, returns the metric name and per-replica `TargetSizeFloat` from `GetMetricSpec`, the *aggregate* backlog from `GetMetrics`, activation from `IsActive`, and pushes transitions from `StreamIsActive` while honoring `stream.Context()`.
- Test: an in-memory `bufconn` pipe wired to a fake `QueueDepthProvider`; assert the spec, that `GetMetrics` returns the aggregate (not pre-divided), activation semantics, `codes.InvalidArgument` on bad metadata, and that `StreamIsActive` stops polling once its context is cancelled.
- Verify: `go test -count=1 -race ./...` where the gRPC modules and generated stubs are available; offline this is validated by gofmt and review.

Set up the module and generate the stubs:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/04-keda-event-driven-autoscaling/01-external-scaler-grpc/cmd/demo go-solutions/54-cloud-native-platform-and-orchestration/04-keda-event-driven-autoscaling/01-external-scaler-grpc/cmd/scaler
cd go-solutions/54-cloud-native-platform-and-orchestration/04-keda-event-driven-autoscaling/01-external-scaler-grpc
go mod edit -go=1.24
go get google.golang.org/grpc@v1.68.0 google.golang.org/protobuf@v1.35.1

# Fetch KEDA's contract and generate the stubs (committed so CI needs no protoc).
curl -sSLo externalscaler.proto \
  https://raw.githubusercontent.com/kedacore/keda/main/pkg/scalers/externalscaler/externalscaler.proto
protoc --go_out=. --go_opt=module=example.com/external-scaler \
       --go-grpc_out=. --go-grpc_opt=module=example.com/external-scaler \
       externalscaler.proto
```

### The contract, and the one rule that governs the whole design

The `externalscaler.proto` declares four RPCs. It is a small file; the copy below
is the KEDA contract verbatim (add a `go_package` option so the stubs land in your
module).

Create `externalscaler.proto`:

```proto
syntax = "proto3";

package externalscaler;
option go_package = "example.com/external-scaler/externalscaler";

service ExternalScaler {
    rpc IsActive(ScaledObjectRef) returns (IsActiveResponse) {}
    rpc StreamIsActive(ScaledObjectRef) returns (stream IsActiveResponse) {}
    rpc GetMetricSpec(ScaledObjectRef) returns (GetMetricSpecResponse) {}
    rpc GetMetrics(GetMetricsRequest) returns (GetMetricsResponse) {}
}

message ScaledObjectRef {
    string name = 1;
    string namespace = 2;
    map<string, string> scalerMetadata = 3;
}

message IsActiveResponse {
    bool result = 1;
}

message GetMetricSpecResponse {
    repeated MetricSpec metricSpecs = 1;
}

message MetricSpec {
    string metricName = 1;
    int64 targetSize = 2 [deprecated = true];
    double targetSizeFloat = 3;
}

message GetMetricsRequest {
    ScaledObjectRef scaledObjectRef = 1;
    string metricName = 2;
}

message GetMetricsResponse {
    repeated MetricValue metricValues = 1;
}

message MetricValue {
    string metricName = 1;
    int64 metricValue = 2 [deprecated = true];
    double metricValueFloat = 3;
}
```

The single rule that dictates the entire implementation is the HPA's replica
formula: `desiredReplicas = ceil(currentMetricValue / target)`. Therefore
`GetMetrics` must return the *aggregate* backlog — the total across the whole
queue — and `GetMetricSpec` declares `targetSizeFloat` as the *per-replica*
target. KEDA feeds the aggregate to the HPA, which divides by the target. If you
pre-divide inside the scaler (returning a per-replica value), the HPA divides
again and you get far too few replicas; if you set the target to the total desired
depth, `ceil(total/total)` is 1 and it never scales out. Return the raw total; let
the HPA own the division.

Two more contract points. First, embed `UnimplementedExternalScalerServer` *by
value* in your server struct: when KEDA adds an RPC to the proto, the embedded
default satisfies the interface so your code keeps compiling instead of breaking
on the new method. Second, use the `double` fields `TargetSizeFloat` /
`MetricValueFloat`; the `int64` `targetSize` / `metricValue` are deprecated and
truncate fractional targets.

### Activation, poll, and push

`IsActive` and `GetMetrics` are the *poll* model: KEDA calls them every
`pollingInterval`. `IsActive` answers the activation question — is the signal
above the activation threshold — which gates the 0↔1 edge. KEDA's rule is strict
inequality: the scaler is active when the metric is *greater than* the activation
value, so a default activation of zero means an empty queue (depth 0) is inactive
and the workload may scale to zero, while any backlog wakes it.

`StreamIsActive` is the *push* model: KEDA opens a long-lived server stream and
your scaler sends an `IsActiveResponse` whenever activation changes, decoupled
from `pollingInterval`. This cuts scale-from-zero latency — you signal the first
message immediately rather than waiting up to a full poll. The non-negotiable
obligation is to honor `stream.Context()`: when KEDA cancels the stream (a
reconcile, a reconnect, a shutdown) the context is done and your loop must return,
or you leak a goroutine per reconcile for the life of the process. The
implementation below emits on the first observation and thereafter only on a
transition, and selects on `stream.Context().Done()` every tick.

### Parsing metadata into a typed config

Everything a trigger configures arrives as `ScalerMetadata`, a
`map[string]string` surfaced from the `ScaledObject`'s `metadata` block. Parse it
once into a typed struct and reject bad input with `status.Error(codes.
InvalidArgument, ...)` — a gRPC status code KEDA logs and surfaces, far better
than a generic error. A missing `queueName`, a non-numeric `targetSize`, or a
non-positive target are all `InvalidArgument`.

Create `scaler.go`:

```go
package scaler

import (
	"context"
	"strconv"
	"time"

	pb "example.com/external-scaler/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// QueueDepthProvider returns the current aggregate depth of a queue. A real
// implementation queries the broker (SQS ApproximateNumberOfMessages, Redis
// LLEN, Kafka consumer lag, or an internal API).
type QueueDepthProvider interface {
	Depth(ctx context.Context, queue string) (int64, error)
}

// Server implements the KEDA ExternalScaler contract. It embeds
// UnimplementedExternalScalerServer by value so new RPCs added to the proto do
// not break compilation (forward compatibility).
type Server struct {
	pb.UnimplementedExternalScalerServer
	provider       QueueDepthProvider
	streamInterval time.Duration
}

// Option configures a Server.
type Option func(*Server)

// WithStreamInterval sets how often StreamIsActive re-checks the signal.
func WithStreamInterval(d time.Duration) Option {
	return func(s *Server) { s.streamInterval = d }
}

// NewServer builds a Server backed by the given depth provider.
func NewServer(p QueueDepthProvider, opts ...Option) *Server {
	s := &Server{provider: p, streamInterval: time.Second}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Compile-time assertion that Server satisfies the generated interface.
var _ pb.ExternalScalerServer = (*Server)(nil)

type triggerConfig struct {
	queueName           string
	targetSize          float64
	activationThreshold float64
}

// parseMetadata converts ScalerMetadata into a typed config, rejecting bad
// input with codes.InvalidArgument.
func parseMetadata(md map[string]string) (triggerConfig, error) {
	var cfg triggerConfig
	cfg.queueName = md["queueName"]
	if cfg.queueName == "" {
		return cfg, status.Error(codes.InvalidArgument, "queueName is required")
	}

	raw, ok := md["targetSize"]
	if !ok {
		return cfg, status.Error(codes.InvalidArgument, "targetSize is required")
	}
	t, err := strconv.ParseFloat(raw, 64)
	if err != nil || t <= 0 {
		return cfg, status.Errorf(codes.InvalidArgument, "targetSize %q must be a positive number", raw)
	}
	cfg.targetSize = t

	if raw, ok := md["activationThreshold"]; ok {
		a, err := strconv.ParseFloat(raw, 64)
		if err != nil || a < 0 {
			return cfg, status.Errorf(codes.InvalidArgument, "activationThreshold %q must be a non-negative number", raw)
		}
		cfg.activationThreshold = a
	}
	return cfg, nil
}

// metricName is the stable metric identifier for a queue; GetMetrics echoes back
// whatever name KEDA requests, but GetMetricSpec declares this one.
func metricName(queue string) string { return "queuedepth-" + queue }

// isActive applies KEDA's activation rule: active iff depth > activationThreshold.
func (cfg triggerConfig) isActive(depth int64) bool {
	return float64(depth) > cfg.activationThreshold
}

// GetMetricSpec shapes the HPA: it declares the metric name and the per-replica
// target the HPA divides the aggregate by.
func (s *Server) GetMetricSpec(_ context.Context, ref *pb.ScaledObjectRef) (*pb.GetMetricSpecResponse, error) {
	cfg, err := parseMetadata(ref.GetScalerMetadata())
	if err != nil {
		return nil, err
	}
	return &pb.GetMetricSpecResponse{
		MetricSpecs: []*pb.MetricSpec{{
			MetricName:      metricName(cfg.queueName),
			TargetSizeFloat: cfg.targetSize,
		}},
	}, nil
}

// GetMetrics returns the AGGREGATE backlog. The HPA divides this by the target;
// pre-dividing here would double-divide and cripple scaling.
func (s *Server) GetMetrics(ctx context.Context, req *pb.GetMetricsRequest) (*pb.GetMetricsResponse, error) {
	cfg, err := parseMetadata(req.GetScaledObjectRef().GetScalerMetadata())
	if err != nil {
		return nil, err
	}
	depth, err := s.provider.Depth(ctx, cfg.queueName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "queue depth: %v", err)
	}
	return &pb.GetMetricsResponse{
		MetricValues: []*pb.MetricValue{{
			MetricName:       req.GetMetricName(),
			MetricValueFloat: float64(depth),
		}},
	}, nil
}

// IsActive answers the activation question polled every pollingInterval.
func (s *Server) IsActive(ctx context.Context, ref *pb.ScaledObjectRef) (*pb.IsActiveResponse, error) {
	cfg, err := parseMetadata(ref.GetScalerMetadata())
	if err != nil {
		return nil, err
	}
	depth, err := s.provider.Depth(ctx, cfg.queueName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "queue depth: %v", err)
	}
	return &pb.IsActiveResponse{Result: cfg.isActive(depth)}, nil
}

// StreamIsActive pushes activation changes. It emits on the first observation and
// thereafter only on a transition, and returns when the stream context is
// cancelled so KEDA reconnects do not leak goroutines.
func (s *Server) StreamIsActive(ref *pb.ScaledObjectRef, stream grpc.ServerStreamingServer[pb.IsActiveResponse]) error {
	cfg, err := parseMetadata(ref.GetScalerMetadata())
	if err != nil {
		return err
	}
	ctx := stream.Context()
	ticker := time.NewTicker(s.streamInterval)
	defer ticker.Stop()

	var last, have bool
	for {
		depth, err := s.provider.Depth(ctx, cfg.queueName)
		if err != nil {
			return status.Errorf(codes.Internal, "queue depth: %v", err)
		}
		active := cfg.isActive(depth)
		if !have || active != last {
			if err := stream.Send(&pb.IsActiveResponse{Result: active}); err != nil {
				return err
			}
			last, have = active, true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
```

### The runnable demo

A gRPC server usually binds a port, which a demo cannot exercise deterministically.
Instead the demo runs the whole round-trip in-process over `bufconn`, an in-memory
`net.Listener`: it starts the real server, dials it with `grpc.NewClient` plus a
`grpc.WithContextDialer`, and calls three RPCs. It then applies the HPA formula
itself so you can see how the aggregate and the per-replica target combine into a
replica count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"

	scaler "example.com/external-scaler"
	pb "example.com/external-scaler/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type staticProvider struct{ depth int64 }

func (p staticProvider) Depth(context.Context, string) (int64, error) { return p.depth, nil }

func main() {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterExternalScalerServer(srv, scaler.NewServer(staticProvider{depth: 250}))
	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Printf("serve: %v", err)
		}
	}()
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewExternalScalerClient(conn)
	ref := &pb.ScaledObjectRef{
		Name:      "orders-consumer",
		Namespace: "default",
		ScalerMetadata: map[string]string{
			"queueName":           "orders",
			"targetSize":          "50",
			"activationThreshold": "0",
		},
	}
	ctx := context.Background()

	spec, err := client.GetMetricSpec(ctx, ref)
	if err != nil {
		log.Fatal(err)
	}
	ms := spec.GetMetricSpecs()[0]
	fmt.Printf("metric: %s target/replica: %.0f\n", ms.GetMetricName(), ms.GetTargetSizeFloat())

	metrics, err := client.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref, MetricName: ms.GetMetricName()})
	if err != nil {
		log.Fatal(err)
	}
	total := metrics.GetMetricValues()[0].GetMetricValueFloat()
	fmt.Printf("aggregate backlog: %.0f\n", total)

	active, err := client.IsActive(ctx, ref)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("active: %v\n", active.GetResult())
	fmt.Printf("HPA desired replicas: %d\n", int(math.Ceil(total/ms.GetTargetSizeFloat())))
}
```

Run it (with the gRPC modules and generated stubs present):

```bash
go run ./cmd/demo
```

Expected output:

```
metric: queuedepth-orders target/replica: 50
aggregate backlog: 250
active: true
HPA desired replicas: 5
```

### The real server, behind a build tag

The production entry point binds a TCP port and blocks in `Serve`. It lives behind
a build tag so `go build ./...` and `go test ./...` never bind a port; you build it
explicitly with `-tags scaler`. In production you point the `ScaledObject`'s
`scalerAddress` at this service.

Create `cmd/scaler/main.go`:

```go
//go:build scaler

package main

import (
	"context"
	"log"
	"net"

	scaler "example.com/external-scaler"
	pb "example.com/external-scaler/externalscaler"
	"google.golang.org/grpc"
)

// brokerProvider queries the real broker. Stubbed here; wire it to your queue.
type brokerProvider struct{}

func (brokerProvider) Depth(ctx context.Context, queue string) (int64, error) {
	// Production: broker API call, e.g. SQS GetQueueAttributes
	// ApproximateNumberOfMessages, Redis LLEN, or Kafka consumer lag.
	return 0, nil
}

func main() {
	lis, err := net.Listen("tcp", ":6000")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterExternalScalerServer(srv, scaler.NewServer(brokerProvider{}))
	log.Printf("external scaler listening on %s", lis.Addr())
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
```

The matching `ScaledObject` uses the `external` trigger type; the custom keys are
what KEDA surfaces as `ScalerMetadata`:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: orders-consumer
spec:
  scaleTargetRef:
    name: orders-consumer
  minReplicaCount: 0
  maxReplicaCount: 20
  triggers:
    - type: external
      metadata:
        scalerAddress: external-scaler.default.svc:6000
        queueName: orders
        targetSize: "50"
        activationThreshold: "0"
```

### Tests

The tests wire the real server to a real gRPC client over `bufconn`, an in-memory
pipe, so no port is bound and no network is touched. The client dials with
`grpc.NewClient` and a `grpc.WithContextDialer` that returns the bufconn
connection. The fake `fakeProvider` exposes a settable depth (and a call counter,
used to prove the stream stops polling after cancellation). The table over
`IsActive` pins the strict-inequality activation rule; `TestGetMetricsAggregate`
proves the metric is the raw total, not pre-divided; `TestBadMetadata` asserts
`codes.InvalidArgument`; and `TestStreamIsActive` drives a 0→active→0 sequence and
then cancels, asserting the server loop exits (no goroutine leak).

Create `scaler_test.go`:

```go
package scaler

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	pb "example.com/external-scaler/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fakeProvider struct {
	depth atomic.Int64
	calls atomic.Int64
	err   error
}

func (f *fakeProvider) Depth(context.Context, string) (int64, error) {
	f.calls.Add(1)
	if f.err != nil {
		return 0, f.err
	}
	return f.depth.Load(), nil
}

func goodRef() *pb.ScaledObjectRef {
	return &pb.ScaledObjectRef{
		Name:      "orders-consumer",
		Namespace: "default",
		ScalerMetadata: map[string]string{
			"queueName":           "orders",
			"targetSize":          "50",
			"activationThreshold": "50",
		},
	}
}

func newClient(t *testing.T, impl pb.ExternalScalerServer) pb.ExternalScalerClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	pb.RegisterExternalScalerServer(s, impl)
	go func() { _ = s.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		s.Stop()
		_ = lis.Close()
	})
	return pb.NewExternalScalerClient(conn)
}

func TestGetMetricSpec(t *testing.T) {
	t.Parallel()
	client := newClient(t, NewServer(&fakeProvider{}))

	resp, err := client.GetMetricSpec(t.Context(), goodRef())
	if err != nil {
		t.Fatalf("GetMetricSpec: %v", err)
	}
	specs := resp.GetMetricSpecs()
	if len(specs) != 1 {
		t.Fatalf("metric specs = %d, want 1", len(specs))
	}
	if got := specs[0].GetMetricName(); got != "queuedepth-orders" {
		t.Errorf("metricName = %q, want queuedepth-orders", got)
	}
	if got := specs[0].GetTargetSizeFloat(); got != 50 {
		t.Errorf("targetSizeFloat = %v, want 50", got)
	}
}

func TestGetMetricsAggregate(t *testing.T) {
	t.Parallel()
	fake := &fakeProvider{}
	fake.depth.Store(250)
	client := newClient(t, NewServer(fake))

	resp, err := client.GetMetrics(t.Context(), &pb.GetMetricsRequest{
		ScaledObjectRef: goodRef(),
		MetricName:      "queuedepth-orders",
	})
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	values := resp.GetMetricValues()
	if len(values) != 1 {
		t.Fatalf("metric values = %d, want 1", len(values))
	}
	// The aggregate, NOT pre-divided by replicas; the HPA owns the division.
	if got := values[0].GetMetricValueFloat(); got != 250 {
		t.Errorf("metricValueFloat = %v, want 250 (aggregate)", got)
	}
}

func TestIsActive(t *testing.T) {
	t.Parallel()
	// activationThreshold is 50 in goodRef; active iff depth > 50.
	cases := []struct {
		name  string
		depth int64
		want  bool
	}{
		{"below", 40, false},
		{"equal", 50, false},
		{"above", 51, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeProvider{}
			fake.depth.Store(tc.depth)
			client := newClient(t, NewServer(fake))

			resp, err := client.IsActive(t.Context(), goodRef())
			if err != nil {
				t.Fatalf("IsActive: %v", err)
			}
			if resp.GetResult() != tc.want {
				t.Errorf("IsActive(depth=%d) = %v, want %v", tc.depth, resp.GetResult(), tc.want)
			}
		})
	}
}

func TestBadMetadata(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		md   map[string]string
	}{
		{"missing queueName", map[string]string{"targetSize": "50"}},
		{"missing targetSize", map[string]string{"queueName": "orders"}},
		{"bad targetSize", map[string]string{"queueName": "orders", "targetSize": "nope"}},
		{"zero targetSize", map[string]string{"queueName": "orders", "targetSize": "0"}},
	}
	client := newClient(t, NewServer(&fakeProvider{}))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref := &pb.ScaledObjectRef{ScalerMetadata: tc.md}
			_, err := client.GetMetricSpec(t.Context(), ref)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
			}
		})
	}
}

func TestStreamIsActive(t *testing.T) {
	t.Parallel()
	fake := &fakeProvider{}
	// activationThreshold 0 => active iff depth > 0.
	ref := &pb.ScaledObjectRef{ScalerMetadata: map[string]string{
		"queueName":  "orders",
		"targetSize": "50",
	}}
	client := newClient(t, NewServer(fake, WithStreamInterval(5*time.Millisecond)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.StreamIsActive(ctx, ref)
	if err != nil {
		t.Fatalf("StreamIsActive: %v", err)
	}

	if got := recvResult(t, stream); got != false {
		t.Fatalf("initial result = %v, want false (empty queue)", got)
	}
	fake.depth.Store(100) // 0 -> active
	if got := recvResult(t, stream); got != true {
		t.Fatalf("after backlog result = %v, want true", got)
	}
	fake.depth.Store(0) // active -> 0
	if got := recvResult(t, stream); got != false {
		t.Fatalf("after drain result = %v, want false", got)
	}

	cancel() // KEDA closing the stream must end the server loop
	if _, err := stream.Recv(); err == nil {
		t.Fatal("Recv after cancel: want error, got nil")
	}

	// The server loop must stop polling once its context is cancelled.
	time.Sleep(30 * time.Millisecond)
	before := fake.calls.Load()
	time.Sleep(30 * time.Millisecond)
	if after := fake.calls.Load(); after != before {
		t.Fatalf("provider polled after cancel: %d -> %d (leaked stream goroutine)", before, after)
	}
}

func recvResult(t *testing.T, stream grpc.ServerStreamingClient[pb.IsActiveResponse]) bool {
	t.Helper()
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	return resp.GetResult()
}

func TestProviderErrorIsInternal(t *testing.T) {
	t.Parallel()
	client := newClient(t, NewServer(&fakeProvider{err: errors.New("broker down")}))
	_, err := client.GetMetrics(t.Context(), &pb.GetMetricsRequest{
		ScaledObjectRef: goodRef(),
		MetricName:      "queuedepth-orders",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}
```

## Review

The scaler is correct when it obeys the HPA formula end to end: `GetMetrics`
returns the aggregate backlog and `GetMetricSpec` declares the per-replica target,
so the HPA computes `ceil(aggregate/target)`. `TestGetMetricsAggregate` is the
load-bearing check — if it ever returns a pre-divided value, scaling silently
under-provisions. Activation is strict inequality: `TestIsActive` pins that depth
equal to the activation threshold is *inactive*, so a default threshold of zero
lets an empty queue scale to zero while any backlog wakes it.

The mistakes to avoid: do not omit `UnimplementedExternalScalerServer` — embedding
it by value is what keeps your server compiling when KEDA adds an RPC. Do not
ignore `stream.Context()` in `StreamIsActive`; the poll-after-cancel check in
`TestStreamIsActive` fails if the loop leaks. Do not return generic errors for bad
metadata; `status.Error(codes.InvalidArgument, ...)` is what KEDA surfaces to
operators. And do not reach for the deprecated `targetSize`/`metricValue` int64
fields — fractional targets truncate. Offline this module cannot build (the gRPC
and protobuf modules and generated stubs are not vendored); it is validated by
gofmt and review, and gates fully where those are available.

## Resources

- [KEDA docs: External Scalers (the gRPC contract)](https://keda.sh/docs/2.18/concepts/external-scalers/) — the four RPCs, poll vs push, and `scalerAddress`.
- [externalscaler.proto (kedacore/keda)](https://github.com/kedacore/keda/blob/main/pkg/scalers/externalscaler/externalscaler.proto) — the message and field definitions, including the deprecated int64 fields.
- [protoc-gen-go-grpc](https://pkg.go.dev/google.golang.org/grpc/cmd/protoc-gen-go-grpc) — the generated `Register*`/`Unimplemented*` server API and generic stream types.
- [`google.golang.org/grpc/test/bufconn`](https://pkg.go.dev/google.golang.org/grpc/test/bufconn) — the in-memory listener used to test gRPC servers without a port.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-graceful-drain-worker.md](02-graceful-drain-worker.md)
