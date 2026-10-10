package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	grpclib "google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/relayops/apim/internal/grpc"
	"github.com/relayops/apim/internal/store"
)

type webFrame struct {
	trailer bool
	data    []byte
}

func parseWebFrames(t *testing.T, b []byte) []webFrame {
	t.Helper()
	var out []webFrame
	for len(b) > 0 {
		if len(b) < 5 {
			t.Fatalf("truncated frame header: %q", b)
		}
		n := int(binary.BigEndian.Uint32(b[1:5]))
		if len(b) < 5+n {
			t.Fatalf("truncated frame: want %d bytes, have %d", n, len(b)-5)
		}
		out = append(out, webFrame{trailer: b[0]&0x80 != 0, data: b[5 : 5+n]})
		b = b[5+n:]
	}
	return out
}

func grpcWebCall(t *testing.T, gw, method, mode string, msgs ...[]byte) (*http.Response, []webFrame) {
	t.Helper()
	var body []byte
	for _, m := range msgs {
		body = append(body, grpc.EncodeFrame(m)...)
	}
	// +raw selects the echo service's []byte codec.
	ct := "application/grpc-web+raw"
	if mode == "text" {
		ct = "application/grpc-web-text+raw"
		body = []byte(base64.StdEncoding.EncodeToString(body))
	}
	req, _ := http.NewRequest("POST", gw+"/"+method, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Grpc-Web", "1")
	resp, err := http.DefaultClient.Do(req) // HTTP/1.1, like a browser
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if mode == "text" {
		dec, err := io.ReadAll(&base64Chunks{src: bytes.NewReader(raw)})
		if err != nil {
			t.Fatalf("decode text body %q: %v", raw, err)
		}
		raw = dec
	}
	return resp, parseWebFrames(t, raw)
}

func trailerStatus(f webFrame) string {
	for _, line := range strings.Split(string(f.data), "\r\n") {
		if v, ok := strings.CutPrefix(line, "grpc-status: "); ok {
			return v
		}
	}
	return ""
}

func TestGRPCWeb(t *testing.T) {
	backend := startEchoServer(t)
	api := grpcAPI("h2c://" + backend)
	api.GRPCPolicy.MaxMessageSizeBytes = 1024
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	gw := httptest.NewServer(g) // plain HTTP/1.1
	defer gw.Close()

	for _, mode := range []string{"binary", "text"} {
		t.Run(mode, func(t *testing.T) {
			// Unary
			resp, frames := grpcWebCall(t, gw.URL, "test.Echo/Unary", mode, []byte("hi"))
			if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/grpc-web") || len(frames) != 2 ||
				string(frames[0].data) != "echo:hi" || !frames[1].trailer || trailerStatus(frames[1]) != "0" {
				t.Fatalf("unary: %s %+v", resp.Header.Get("Content-Type"), frames)
			}
			if mode == "text" && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/grpc-web-text") {
				t.Fatalf("text content type %s", resp.Header.Get("Content-Type"))
			}
			// Server streaming: five messages then trailers.
			_, frames = grpcWebCall(t, gw.URL, "test.Echo/ServerStream", mode, []byte("x"))
			if len(frames) != 6 || string(frames[4].data) != "4:x" || trailerStatus(frames[5]) != "0" {
				t.Fatalf("server stream: %+v", frames)
			}
			// Oversized request: refused with RESOURCE_EXHAUSTED in a trailer frame.
			resp, frames = grpcWebCall(t, gw.URL, "test.Echo/Unary", mode, bytes.Repeat([]byte("a"), 2048))
			if len(frames) == 0 || trailerStatus(frames[len(frames)-1]) != "8" {
				t.Fatalf("oversized: %d %+v", resp.StatusCode, frames)
			}
			// Oversized response mid-stream: earlier message delivered, then status 8.
			_, frames = grpcWebCall(t, gw.URL, "test.Echo/Big", mode, []byte("x"))
			if len(frames) != 2 || string(frames[0].data) != "ok" || trailerStatus(frames[1]) != "8" {
				t.Fatalf("big: %+v", frames)
			}
		})
	}

	// Gateway refusals reach gRPC-Web clients as trailer frames too.
	keyed := grpcAPI("h2c://" + backend)
	keyed.AuthType = "api_key"
	g.load(t, store.SnapshotData{APIs: []store.API{keyed}, Revision: 11})
	resp, frames := grpcWebCall(t, gw.URL, "test.Echo/Unary", "binary", []byte("hi"))
	if len(frames) != 1 || trailerStatus(frames[0]) != "16" || resp.Header.Get("Grpc-Status") != "16" {
		t.Fatalf("unauthenticated: %v %+v", resp.Header, frames)
	}
}

// upperDesc is a unary service using google.protobuf.StringValue messages.
var upperDesc = grpclib.ServiceDesc{
	ServiceName: "test.Upper",
	HandlerType: (*any)(nil),
	Methods: []grpclib.MethodDesc{{MethodName: "Do", Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpclib.UnaryServerInterceptor) (any, error) {
		var v wrapperspb.StringValue // the transcoder sends application/grpc+proto
		if err := dec(&v); err != nil {
			return nil, err
		}
		return wrapperspb.String(strings.ToUpper(v.GetValue())), nil
	}}},
}

func upperDescriptorSet(t *testing.T) []byte {
	t.Helper()
	str := func(s string) *string { return &s }
	sv := ".google.protobuf.StringValue"
	svc := &descriptorpb.FileDescriptorProto{Name: str("upper.proto"), Package: str("test"), Syntax: str("proto3"),
		Dependency: []string{"google/protobuf/wrappers.proto"},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: str("Upper"), Method: []*descriptorpb.MethodDescriptorProto{
			{Name: str("Do"), InputType: &sv, OutputType: &sv}}}}}
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(wrapperspb.File_google_protobuf_wrappers_proto), svc}}
	b, err := proto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGRPCJSONTranscoding(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpclib.NewServer()
	srv.RegisterService(&upperDesc, nil)
	srv.RegisterService(&echoDesc, nil)
	go srv.Serve(lis)
	defer srv.Stop()

	api := testAPI("upper", "/test.Upper", "h2c://"+lis.Addr().String())
	api.Protocol, api.StripPath = "grpc", false
	api.GRPCDescriptorSet = upperDescriptorSet(t)
	api.GRPCPolicy.JSONTranscoding = true
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	ct := map[string]string{"Content-Type": "application/json"}

	rec := do(g, "POST", "/test.Upper/Do", ct, `"hello"`) // StringValue's JSON form is a string
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || strings.TrimSpace(rec.Body.String()) != `"HELLO"` {
		t.Fatalf("transcoded call: %d %s %s", rec.Code, rec.Header(), rec.Body)
	}
	// Invalid JSON for the message type is a 400 before the service is called.
	if rec := do(g, "POST", "/test.Upper/Do", ct, `{"nope":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid request: %d %s", rec.Code, rec.Body)
	}
	// Methods outside the descriptor are refused.
	rec = do(g, "POST", "/test.Upper/Missing", ct, `"x"`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("unknown method: %d %s", rec.Code, rec.Body)
	}
	var e map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &e) != nil {
		t.Fatalf("JSON client got a non-JSON error: %s", rec.Body)
	}
	// A service error maps to an HTTP status with the gRPC code in the body.
	if rec := do(g, "GET", "/test.Upper/Do", ct, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("GET: %d", rec.Code)
	}
}
