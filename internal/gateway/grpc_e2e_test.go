package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/relayops/apim/internal/store"
)

// rawCodec carries []byte messages, so the test needs no generated code.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) { return *(v.(*[]byte)), nil }
func (rawCodec) Unmarshal(b []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), b...)
	return nil
}
func (rawCodec) Name() string { return "raw" }

func init() { encoding.RegisterCodec(rawCodec{}) }

// echoService implements test.Echo with all four call shapes.
var echoDesc = grpclib.ServiceDesc{
	ServiceName: "test.Echo",
	HandlerType: (*any)(nil),
	Methods: []grpclib.MethodDesc{
		{MethodName: "Unary", Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpclib.UnaryServerInterceptor) (any, error) {
			var in []byte
			if err := dec(&in); err != nil {
				return nil, err
			}
			if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("x-fail")) > 0 {
				return nil, status.Error(codes.Unavailable, "backend overloaded")
			}
			out := append([]byte("echo:"), in...)
			return &out, nil
		}},
	},
	Streams: []grpclib.StreamDesc{
		{StreamName: "ServerStream", ServerStreams: true, Handler: func(_ any, s grpclib.ServerStream) error {
			var in []byte
			if err := s.RecvMsg(&in); err != nil {
				return err
			}
			for i := 0; i < 5; i++ {
				msg := append([]byte{byte('0' + i), ':'}, in...)
				if err := s.SendMsg(&msg); err != nil {
					return err
				}
				time.Sleep(20 * time.Millisecond)
			}
			return nil
		}},
		{StreamName: "ClientStream", ClientStreams: true, Handler: func(_ any, s grpclib.ServerStream) error {
			var all []byte
			for {
				var in []byte
				err := s.RecvMsg(&in)
				if errors.Is(err, io.EOF) {
					return s.SendMsg(&all)
				}
				if err != nil {
					return err
				}
				all = append(all, in...)
			}
		}},
		{StreamName: "Bidi", ServerStreams: true, ClientStreams: true, Handler: func(_ any, s grpclib.ServerStream) error {
			for {
				var in []byte
				err := s.RecvMsg(&in)
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				out := bytes.ToUpper(in)
				if err := s.SendMsg(&out); err != nil {
					return err
				}
			}
		}},
		{StreamName: "Big", ServerStreams: true, Handler: func(_ any, s grpclib.ServerStream) error {
			var in []byte
			if err := s.RecvMsg(&in); err != nil {
				return err
			}
			small := []byte("ok")
			if err := s.SendMsg(&small); err != nil {
				return err
			}
			big := bytes.Repeat([]byte("x"), 64<<10)
			return s.SendMsg(&big)
		}},
	},
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpclib.NewServer(grpclib.MaxRecvMsgSize(16 << 20))
	srv.RegisterService(&echoDesc, nil)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// startH2CGateway serves g over cleartext HTTP/2, as the proxy listener does
// with RELAYOPS_H2C_ENABLED.
func startH2CGateway(t *testing.T, g *Gateway) string {
	t.Helper()
	srv := httptest.NewServer(h2c.NewHandler(g, &http2.Server{}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func dialGateway(t *testing.T, addr string) *grpclib.ClientConn {
	t.Helper()
	conn, err := grpclib.NewClient(addr, grpclib.WithTransportCredentials(insecure.NewCredentials()),
		grpclib.WithDefaultCallOptions(grpclib.ForceCodec(rawCodec{})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func grpcAPI(upstream string) store.API {
	a := testAPI("echo", "/test.Echo", upstream)
	a.Protocol = "grpc"
	a.StripPath = false
	return a
}

func TestGRPCThroughGatewayAllCallTypes(t *testing.T) {
	backend := startEchoServer(t)
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{grpcAPI("h2c://" + backend)}})
	conn := dialGateway(t, startH2CGateway(t, g))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Unary
	in, out := []byte("hi"), []byte(nil)
	if err := conn.Invoke(ctx, "/test.Echo/Unary", &in, &out); err != nil || string(out) != "echo:hi" {
		t.Fatalf("unary: %q %v", out, err)
	}

	// Server streaming: messages arrive one at a time.
	ss, err := conn.NewStream(ctx, &echoDesc.Streams[0], "/test.Echo/ServerStream")
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.SendMsg(&in); err != nil {
		t.Fatal(err)
	}
	ss.CloseSend()
	var got []string
	for {
		var m []byte
		if err := ss.RecvMsg(&m); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("server stream: %v", err)
			}
			break
		}
		got = append(got, string(m))
	}
	if len(got) != 5 || got[4] != "4:hi" {
		t.Fatalf("server stream got %v", got)
	}

	// Client streaming
	cs, err := conn.NewStream(ctx, &echoDesc.Streams[1], "/test.Echo/ClientStream")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "b", "c"} {
		b := []byte(p)
		if err := cs.SendMsg(&b); err != nil {
			t.Fatal(err)
		}
	}
	cs.CloseSend()
	var joined []byte
	if err := cs.RecvMsg(&joined); err != nil || string(joined) != "abc" {
		t.Fatalf("client stream: %q %v", joined, err)
	}

	// Bidirectional: each reply arrives before the next request is sent.
	bs, err := conn.NewStream(ctx, &echoDesc.Streams[2], "/test.Echo/Bidi")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one", "two", "three"} {
		b := []byte(p)
		if err := bs.SendMsg(&b); err != nil {
			t.Fatal(err)
		}
		var r []byte
		if err := bs.RecvMsg(&r); err != nil || string(r) != strings.ToUpper(p) {
			t.Fatalf("bidi %s: %q %v", p, r, err)
		}
	}
	bs.CloseSend()
	var tail []byte
	if err := bs.RecvMsg(&tail); !errors.Is(err, io.EOF) {
		t.Fatalf("bidi end: %v", err)
	}

	// Upstream gRPC errors keep their status and message, every time (a
	// trailers-only response used to race the proxy's header flush).
	fctx := metadata.AppendToOutgoingContext(ctx, "x-fail", "1")
	for i := 0; i < 100; i++ {
		err = conn.Invoke(fctx, "/test.Echo/Unary", &in, &out)
		if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "backend overloaded") {
			t.Fatalf("upstream error, call %d: %v", i, err)
		}
	}

	// Failed calls are logged with the equivalent HTTP status (UNAVAILABLE ->
	// 503), so error rates and auto-rollback see them.
	var metrics bytes.Buffer
	g.Collector().WritePrometheusMetrics(&metrics)
	if !strings.Contains(metrics.String(), `relayops_requests_total{status="503"} 100`) {
		t.Fatalf("gRPC failures not counted as 503: %s", grepLines(metrics.String(), "requests_total"))
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "; ")
}

func TestGRPCMessageLimits(t *testing.T) {
	backend := startEchoServer(t)
	api := grpcAPI("h2c://" + backend)
	api.GRPCPolicy.MaxMessageSizeBytes = 1024
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	conn := dialGateway(t, startH2CGateway(t, g))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A request message over the limit never reaches the service.
	big := bytes.Repeat([]byte("a"), 2048)
	var out []byte
	err := conn.Invoke(ctx, "/test.Echo/Unary", &big, &out)
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "exceeds the 1024 byte limit") {
		t.Fatalf("oversized unary request: %v", err)
	}

	// Client streaming: small messages pass, the oversized one ends the call.
	cs, err := conn.NewStream(ctx, &echoDesc.Streams[1], "/test.Echo/ClientStream")
	if err != nil {
		t.Fatal(err)
	}
	small := []byte("ok")
	_ = cs.SendMsg(&small)
	_ = cs.SendMsg(&big)
	cs.CloseSend()
	if err := cs.RecvMsg(&out); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized message mid-stream: %v", err)
	}

	// Server streaming: the client gets the messages before the oversized one,
	// then RESOURCE_EXHAUSTED trailers (not a reset stream).
	ss, err := conn.NewStream(ctx, &echoDesc.Streams[3], "/test.Echo/Big")
	if err != nil {
		t.Fatal(err)
	}
	_ = ss.SendMsg(&small)
	ss.CloseSend()
	var first []byte
	if err := ss.RecvMsg(&first); err != nil || string(first) != "ok" {
		t.Fatalf("first streamed message: %q %v", first, err)
	}
	if err := ss.RecvMsg(&out); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized response message: %v", err)
	}

	// Within the limit, the same API works.
	in := []byte("fine")
	if err := conn.Invoke(ctx, "/test.Echo/Unary", &in, &out); err != nil || string(out) != "echo:fine" {
		t.Fatalf("small unary after limits: %q %v", out, err)
	}
}

// echoDescriptorSet describes test.Echo, as protoc --descriptor_set_out would.
func echoDescriptorSet(t *testing.T) []byte {
	t.Helper()
	str := func(s string) *string { return &s }
	yes := func() *bool { b := true; return &b }
	msg := ".test.Msg"
	fd := &descriptorpb.FileDescriptorProto{
		Name: str("echo.proto"), Package: str("test"), Syntax: str("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: str("Msg")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: str("Echo"),
			Method: []*descriptorpb.MethodDescriptorProto{
				{Name: str("Unary"), InputType: &msg, OutputType: &msg},
				{Name: str("ServerStream"), InputType: &msg, OutputType: &msg, ServerStreaming: yes()},
				{Name: str("ClientStream"), InputType: &msg, OutputType: &msg, ClientStreaming: yes()},
				{Name: str("Bidi"), InputType: &msg, OutputType: &msg, ClientStreaming: yes(), ServerStreaming: yes()},
			},
		}},
	}
	b, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGRPCDescriptorAndMethodRules(t *testing.T) {
	backend := startEchoServer(t)
	api := grpcAPI("h2c://" + backend)
	api.GRPCDescriptorSet = echoDescriptorSet(t) // "Big" is not in it
	api.GRPCPolicy = store.GRPCPolicy{Rules: []store.GRPCMethodRule{
		{Methods: []string{"test.Echo/Bidi"}, Plans: []string{"Enterprise"}, Action: "allow"},
		{Methods: []string{"test.Echo/Bidi"}, Action: "deny"},
	}}
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	conn := dialGateway(t, startH2CGateway(t, g))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	in, out := []byte("x"), []byte(nil)
	if err := conn.Invoke(ctx, "/test.Echo/Unary", &in, &out); err != nil {
		t.Fatalf("described method: %v", err)
	}
	ss, err := conn.NewStream(ctx, &echoDesc.Streams[3], "/test.Echo/Big")
	if err != nil {
		t.Fatal(err)
	}
	_ = ss.SendMsg(&in)
	ss.CloseSend()
	if err := ss.RecvMsg(&out); status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "descriptor set") {
		t.Fatalf("method missing from the descriptor: %v", err)
	}
	bs, err := conn.NewStream(ctx, &echoDesc.Streams[2], "/test.Echo/Bidi")
	if err != nil {
		t.Fatal(err)
	}
	_ = bs.SendMsg(&in)
	if err := bs.RecvMsg(&out); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("method denied by rule: %v", err)
	}

	// A malformed descriptor set fails the API at snapshot build.
	bad := grpcAPI("h2c://" + backend)
	bad.GRPCDescriptorSet = []byte("not a descriptor")
	if _, errs := buildSnapshot(store.SnapshotData{APIs: []store.API{bad}}, 1, newUpstreamRegistry()); len(errs) == 0 {
		t.Fatal("malformed descriptor set accepted")
	}

	p := store.GRPCPolicy{Rules: []store.GRPCMethodRule{{Methods: []string{"NoSlash"}, Action: "allow"}}}
	p.Normalize()
	if p.Validate() == nil {
		t.Fatal("method pattern without a service accepted")
	}
}
