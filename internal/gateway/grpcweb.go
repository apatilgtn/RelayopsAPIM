package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/relayops/apim/internal/grpc"
)

// gRPC-Web (browsers, HTTP/1.1) and JSON transcoding are translated to
// native gRPC towards the service and back on the way out, so the service
// only ever speaks gRPC and every gRPC policy applies unchanged.

const (
	webBinary = "binary"
	webText   = "text"
)

// grpcWebMode recognises a gRPC-Web request ("binary" or "text").
func grpcWebMode(contentType string) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "application/grpc-web-text"):
		return webText
	case strings.HasPrefix(ct, "application/grpc-web"):
		return webBinary
	}
	return ""
}

// toGRPCContentType maps application/grpc-web[-text][+proto] to
// application/grpc[+proto].
func toGRPCContentType(ct string) string {
	suffix := ""
	if i := strings.Index(ct, "+"); i >= 0 {
		suffix = ct[i:]
	}
	return "application/grpc" + suffix
}

// setupWeb rewrites a gRPC-Web request into a gRPC one.
func (x *grpcExchange) setupWeb(r *http.Request, mode string) {
	x.web = mode
	x.webContentType = r.Header.Get("Content-Type")
	r.Header.Set("Content-Type", toGRPCContentType(x.webContentType))
	r.Header.Set("TE", "trailers")
	r.Header.Del("Content-Length")
	r.ContentLength = -1
	if mode == webText && r.Body != nil && r.Body != http.NoBody {
		r.Body = readCloser{&base64Chunks{src: r.Body}, r.Body}
	}
}

// base64Chunks decodes base64 that may arrive as several padded chunks
// (gRPC-Web text clients may encode each message separately).
type base64Chunks struct {
	src  io.Reader
	in   []byte
	out  bytes.Buffer
	done bool
}

func (b *base64Chunks) Read(p []byte) (int, error) {
	for b.out.Len() == 0 {
		if b.done {
			return 0, io.EOF
		}
		buf := make([]byte, 16<<10)
		n, err := b.src.Read(buf)
		for _, c := range buf[:n] {
			if c != '\r' && c != '\n' {
				b.in = append(b.in, c)
			}
		}
		whole := len(b.in) / 4 * 4
		for i := 0; i < whole; i += 4 {
			dst := make([]byte, 3)
			m, derr := base64.StdEncoding.Decode(dst, b.in[i:i+4])
			if derr != nil {
				return 0, fmt.Errorf("invalid grpc-web-text body: %w", derr)
			}
			b.out.Write(dst[:m])
		}
		b.in = b.in[whole:]
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return 0, err
			}
			if len(b.in) != 0 {
				return 0, errors.New("invalid grpc-web-text body: truncated base64")
			}
			b.done = true
		}
	}
	return b.out.Read(p)
}

// trailerFrame encodes gRPC-Web trailers as the final body frame (flag 0x80).
func trailerFrame(code int, msg string, extra http.Header) []byte {
	var t bytes.Buffer
	fmt.Fprintf(&t, "grpc-status: %d\r\n", code)
	if msg != "" {
		fmt.Fprintf(&t, "grpc-message: %s\r\n", grpc.EncodeMessage(msg))
	}
	for k, vv := range extra {
		lk := strings.ToLower(k)
		if lk == "grpc-status" || lk == "grpc-message" {
			continue
		}
		for _, v := range vv {
			fmt.Fprintf(&t, "%s: %s\r\n", lk, v)
		}
	}
	frame := make([]byte, 5, 5+t.Len())
	frame[0] = 0x80
	binary.BigEndian.PutUint32(frame[1:], uint32(t.Len()))
	return append(frame, t.Bytes()...)
}

// webContentTypeOut is the response content type for the client's mode.
func (x *grpcExchange) webContentTypeOut(upstream string) string {
	suffix := ""
	if i := strings.Index(upstream, "+"); i >= 0 {
		suffix = upstream[i:]
	}
	if x.web == webText {
		return "application/grpc-web-text" + suffix
	}
	return "application/grpc-web" + suffix
}

// grpcWebBody streams the service's frames to the client and appends the
// trailers as a final frame (base64 for text clients).
type grpcWebBody struct {
	src   io.ReadCloser
	resp  *http.Response
	x     *grpcExchange
	out   bytes.Buffer
	pend  []byte // text mode: bytes not yet base64-encoded (< 3)
	ended bool
}

func (b *grpcWebBody) Read(p []byte) (int, error) {
	for b.out.Len() == 0 {
		if b.ended {
			return 0, io.EOF
		}
		buf := make([]byte, 32<<10)
		n, err := b.src.Read(buf)
		b.emit(buf[:n])
		if err != nil {
			code, msg := grpc.StatusOK, ""
			trailers := http.Header{}
			switch {
			case b.x.limitErr() != nil:
				code, msg = grpc.StatusResourceExhausted, b.x.limitErr().Error()
			case !errors.Is(err, io.EOF):
				code, msg = grpc.StatusUnavailable, "upstream stream failed: "+err.Error()
			default:
				for k, vv := range b.resp.Trailer {
					trailers[k] = vv
				}
				if v := trailers.Get("Grpc-Status"); v != "" {
					code, _ = strconv.Atoi(v)
				} else {
					code, msg = grpc.StatusInternal, "service sent no grpc-status"
				}
				if m := trailers.Get("Grpc-Message"); m != "" {
					msg = decodeGRPCMessage(m)
				}
			}
			// The trailers travel in the body; none are sent as HTTP trailers.
			for k := range b.resp.Trailer {
				delete(b.resp.Trailer, k)
			}
			b.x.finalStatus = &code
			b.emit(trailerFrame(code, msg, trailers))
			b.flushText()
			b.ended = true
		}
	}
	return b.out.Read(p)
}

func (b *grpcWebBody) emit(data []byte) {
	if b.x.web != webText {
		b.out.Write(data)
		return
	}
	b.pend = append(b.pend, data...)
	whole := len(b.pend) / 3 * 3
	if whole > 0 {
		enc := make([]byte, base64.StdEncoding.EncodedLen(whole))
		base64.StdEncoding.Encode(enc, b.pend[:whole])
		b.out.Write(enc)
		b.pend = append(b.pend[:0], b.pend[whole:]...)
	}
}

func (b *grpcWebBody) flushText() {
	if b.x.web == webText && len(b.pend) > 0 {
		b.out.WriteString(base64.StdEncoding.EncodeToString(b.pend))
		b.pend = nil
	}
}

func (b *grpcWebBody) Close() error { return b.src.Close() }

func decodeGRPCMessage(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				out.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// writeGRPCWebError answers a gRPC-Web request the gateway refused: status in
// the headers and a trailer frame in the body.
func writeGRPCWebError(w http.ResponseWriter, x *grpcExchange, code int, msg string) {
	frame := trailerFrame(code, msg, nil)
	if x.web == webText {
		frame = []byte(base64.StdEncoding.EncodeToString(frame))
	}
	h := w.Header()
	h.Set("Content-Type", x.webContentTypeOut(""))
	h.Set("Grpc-Status", strconv.Itoa(code))
	h.Set("Grpc-Message", grpc.EncodeMessage(msg))
	h.Set("Content-Length", strconv.Itoa(len(frame)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(frame)
}

// --- JSON transcoding --------------------------------------------------------

// setupJSON turns a JSON request for a unary method into a gRPC request.
func (x *grpcExchange) setupJSON(r *http.Request, method protoreflect.MethodDescriptor, limit int) error {
	x.json = method
	if r.Method != http.MethodPost {
		return errors.New("JSON calls to gRPC methods use POST")
	}
	if method.IsStreamingClient() || method.IsStreamingServer() {
		return fmt.Errorf("JSON transcoding supports unary methods; %s is streaming", method.FullName())
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	r.Body.Close()
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) > limit {
		return fmt.Errorf("request body exceeds %d bytes", limit)
	}
	msg := dynamicpb.NewMessage(method.Input())
	if len(bytes.TrimSpace(body)) > 0 {
		if err := protojson.Unmarshal(body, msg); err != nil {
			return fmt.Errorf("request does not match %s: %v", method.Input().FullName(), err)
		}
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return fmt.Errorf("message of %d bytes exceeds the %d byte limit", len(raw), limit)
	}
	setRequestBody(r, grpc.EncodeFrame(raw))
	r.Header.Set("Content-Type", "application/grpc+proto")
	r.Header.Set("TE", "trailers")
	r.Header.Del("Accept-Encoding")
	return nil
}

// transcodeResponse turns the service's gRPC answer into JSON.
func (x *grpcExchange) transcodeResponse(resp *http.Response, limit int) {
	code, msg := grpc.StatusInternal, "service sent no grpc-status"
	var payload []byte
	readStatus := func(h http.Header) bool {
		v := h.Get("Grpc-Status")
		if v == "" {
			return false
		}
		code, _ = strconv.Atoi(v)
		msg = decodeGRPCMessage(h.Get("Grpc-Message"))
		return true
	}
	if !readStatus(resp.Header) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+5+1))
		resp.Body.Close()
		switch {
		case err != nil:
			code, msg = grpc.StatusUnavailable, "upstream stream failed: "+err.Error()
		case len(body) > limit+5:
			code, msg = grpc.StatusResourceExhausted, fmt.Sprintf("response exceeds the %d byte limit", limit)
		default:
			if !readStatus(resp.Trailer) {
				code, msg = grpc.StatusInternal, "service sent no grpc-status"
			}
			if code == grpc.StatusOK {
				p, err := grpc.DecodeFrame(bytes.NewReader(body))
				if err != nil {
					code, msg = grpc.StatusInternal, "service sent no response message"
				} else {
					payload = p
				}
			}
		}
	} else {
		resp.Body.Close()
	}
	status := grpc.HTTPStatus(code)
	var out []byte
	if code == grpc.StatusOK {
		m := dynamicpb.NewMessage(x.json.Output())
		if err := proto.Unmarshal(payload, m); err != nil {
			status, out = http.StatusBadGateway, mustJSON(map[string]any{"code": grpc.StatusInternal, "status": "INTERNAL", "message": "response is not a " + string(x.json.Output().FullName())})
		} else {
			out, _ = protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(m)
		}
	} else {
		out = mustJSON(map[string]any{"code": code, "status": grpc.StatusText(code), "message": msg})
	}
	c := code
	x.finalStatus = &c
	for k := range resp.Trailer {
		delete(resp.Trailer, k)
	}
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	keep := http.Header{}
	for k, vv := range resp.Header {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if strings.HasPrefix(ck, "Grpc-") || ck == "Content-Type" || ck == "Content-Length" || ck == "Trailer" {
			continue
		}
		keep[k] = vv
	}
	keep.Set("Content-Type", "application/json")
	keep.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header = keep
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
