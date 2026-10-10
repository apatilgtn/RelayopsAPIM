package gateway

import (
	"errors"
	"net/http"
	"strconv"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/relayops/apim/internal/grpc"
)

// defaultGRPCMaxMessage matches gRPC's default receive limit.
const defaultGRPCMaxMessage = 4 << 20

// grpcExchange tracks one gRPC call's message streams.
type grpcExchange struct {
	req, resp *grpc.MessageReader
	// web is set for gRPC-Web clients ("binary" or "text").
	web, webContentType string
	// wantJSON is set for JSON clients of a transcoding API; json is the
	// method being transcoded.
	wantJSON bool
	json     protoreflect.MethodDescriptor
	// finalStatus is the call's grpc-status when the gateway moved it out of
	// the HTTP trailers (gRPC-Web, JSON).
	finalStatus *int
}

// limitErr returns the message-size violation that ended the call, if any.
func (x *grpcExchange) limitErr() error {
	if x == nil {
		return nil
	}
	for _, m := range []*grpc.MessageReader{x.req, x.resp} {
		if m == nil {
			continue
		}
		var tooLarge *grpc.ErrMessageTooLarge
		if err := m.Err(); errors.As(err, &tooLarge) {
			return err
		}
	}
	return nil
}

// serveGRPC proxies a gRPC call. When a message limit stops the call midway,
// the stream is ended with RESOURCE_EXHAUSTED trailers rather than reset, so
// the client gets a status instead of a transport error.
func (g *Gateway) serveGRPC(rec *recorder, r *http.Request, st *reqState) {
	defer func() {
		if p := recover(); p != nil {
			err := st.grpc.limitErr()
			if p != http.ErrAbortHandler || err == nil {
				panic(p)
			}
			g.endGRPCWithLimit(rec, st, err)
		}
	}()
	g.proxy.ServeHTTP(rec, r)
	if st.grpc.web != "" || st.grpc.wantJSON {
		return // their bodies carry the final status
	}
	if err := st.grpc.limitErr(); err != nil && rec.Header().Get(http.TrailerPrefix+"Grpc-Status") == "" && rec.Header().Get("Grpc-Status") == "" {
		g.endGRPCWithLimit(rec, st, err)
	}
}

func (g *Gateway) endGRPCWithLimit(rec *recorder, st *reqState, err error) {
	st.decisionPolicy, st.decisionReason, st.err = "grpc", "message_too_large", err.Error()
	if !rec.wroteHeader {
		grpc.WriteGRPCError(rec, grpc.StatusResourceExhausted, err.Error())
		return
	}
	h := rec.Header()
	h.Set(http.TrailerPrefix+"Grpc-Status", strconv.Itoa(grpc.StatusResourceExhausted))
	h.Set(http.TrailerPrefix+"Grpc-Message", grpc.EncodeMessage(err.Error()))
}

// grpcStatusOf finds the call's final grpc-status: in trailers (normal
// responses) or headers (trailers-only responses and gateway refusals).
func grpcStatusOf(h http.Header) (int, bool) {
	for _, k := range []string{http.TrailerPrefix + "Grpc-Status", "Grpc-Status"} {
		if v := h.Get(k); v != "" {
			if code, err := strconv.Atoi(v); err == nil {
				return code, true
			}
		}
	}
	return 0, false
}

func grpcStatusFromHTTP(status int) int {
	switch status {
	case http.StatusUnauthorized:
		return grpc.StatusUnauthenticated
	case http.StatusForbidden:
		return grpc.StatusPermissionDenied
	case http.StatusNotFound:
		return grpc.StatusNotFound
	case http.StatusTooManyRequests, http.StatusRequestEntityTooLarge:
		return grpc.StatusResourceExhausted
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		// The gateway could not reach the service: clients may retry.
		return grpc.StatusUnavailable
	case http.StatusGatewayTimeout:
		return grpc.StatusDeadlineExceeded
	case http.StatusBadRequest:
		return grpc.StatusInvalidArgument
	case http.StatusNotImplemented:
		return grpc.StatusUnimplemented
	case 499:
		return grpc.StatusCancelled
	default:
		return grpc.StatusInternal
	}
}

// trailersOnly carries an upstream gRPC "Trailers-Only" response (status in
// the headers, no body) from ModifyResponse to the error handler. Streaming
// it through ReverseProxy would race its immediate header flush against the
// end of the handler: when the flush wins, the headers frame goes out
// without END_STREAM and the client reports "server closed the stream
// without sending trailers" instead of the real status. Writing it directly
// ends the stream with the headers frame.
type trailersOnly struct{ resp *http.Response }

func (t *trailersOnly) Error() string { return "grpc trailers-only response" }

func isTrailersOnly(resp *http.Response) bool {
	return resp.Header.Get("Grpc-Status") != "" && (resp.Body == nil || resp.Body == http.NoBody || resp.ContentLength == 0)
}

func writeTrailersOnly(w http.ResponseWriter, t *trailersOnly) {
	h := w.Header()
	for k, vv := range t.resp.Header {
		h[k] = vv
	}
	w.WriteHeader(t.resp.StatusCode)
}
