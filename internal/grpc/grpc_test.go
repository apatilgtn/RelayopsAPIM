package grpc

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"
)

func TestParseMethod(t *testing.T) {
	svc, mth, err := ParseMethod("/relayops.orders.OrderService/CreateOrder")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc != "relayops.orders.OrderService" || mth != "CreateOrder" {
		t.Fatalf("unexpected svc=%s mth=%s", svc, mth)
	}

	_, _, err = ParseMethod("/invalid-format")
	if err == nil {
		t.Fatal("expected error on invalid path format")
	}
}

func TestWriteGRPCError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteGRPCError(rec, StatusUnauthenticated, "missing API key")

	if rec.Header().Get("Content-Type") != "application/grpc" {
		t.Fatalf("expected application/grpc, got %s", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("grpc-status") != "16" {
		t.Fatalf("expected grpc-status 16, got %s", rec.Header().Get("grpc-status"))
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for gRPC status header, got %d", rec.Code)
	}
}

func TestEncodeAndDecodeFrame(t *testing.T) {
	payload := []byte("hello protobuf")
	framed := EncodeFrame(payload)

	if len(framed) != 5+len(payload) {
		t.Fatalf("expected frame length %d, got %d", 5+len(payload), len(framed))
	}

	decoded, err := DecodeFrame(bytes.NewReader(framed))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if string(decoded) != string(payload) {
		t.Fatalf("decoded payload mismatch: %s != %s", string(decoded), string(payload))
	}
}

func TestMessageReaderLimits(t *testing.T) {
	frames := append(EncodeFrame([]byte("ok")), EncodeFrame(bytes.Repeat([]byte("x"), 100))...)
	r := NewMessageReader(io.NopCloser(bytes.NewReader(frames)), 10)
	got, err := io.ReadAll(r)
	var tooLarge *ErrMessageTooLarge
	if !errors.As(err, &tooLarge) || tooLarge.Size != 100 {
		t.Fatalf("error %v", err)
	}
	if !bytes.Equal(got, EncodeFrame([]byte("ok"))) {
		t.Fatalf("messages before the oversized one not delivered: %q", got)
	}
	// Byte-at-a-time delivery counts messages across reads.
	ok := NewMessageReader(io.NopCloser(iotest.OneByteReader(bytes.NewReader(append(EncodeFrame([]byte("a")), EncodeFrame([]byte("bc"))...)))), 10)
	if b, err := io.ReadAll(ok); err != nil || len(b) != 13 || ok.Messages != 2 {
		t.Fatalf("one byte reads: %d bytes, %d messages, %v", len(b), ok.Messages, err)
	}
	if EncodeMessage("ünïcode 100%\n") != "%C3%BCn%C3%AFcode 100%25%0A" {
		t.Fatalf("EncodeMessage: %s", EncodeMessage("ünïcode 100%\n"))
	}
}
