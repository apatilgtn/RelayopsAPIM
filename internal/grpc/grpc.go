package grpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Standard gRPC status codes (https://grpc.github.io/grpc/core/md_doc_statuscodes.html).
const (
	StatusOK                 = 0
	StatusCancelled          = 1
	StatusUnknown            = 2
	StatusInvalidArgument    = 3
	StatusDeadlineExceeded   = 4
	StatusNotFound           = 5
	StatusAlreadyExists      = 6
	StatusPermissionDenied   = 7
	StatusResourceExhausted  = 8
	StatusFailedPrecondition = 9
	StatusAborted            = 10
	StatusOutOfRange         = 11
	StatusUnimplemented      = 12
	StatusInternal           = 13
	StatusUnavailable        = 14
	StatusDataLoss           = 15
	StatusUnauthenticated    = 16
)

// StatusText returns a human-readable string representation of a gRPC status code.
func StatusText(code int) string {
	switch code {
	case StatusOK:
		return "OK"
	case StatusCancelled:
		return "CANCELLED"
	case StatusUnknown:
		return "UNKNOWN"
	case StatusInvalidArgument:
		return "INVALID_ARGUMENT"
	case StatusDeadlineExceeded:
		return "DEADLINE_EXCEEDED"
	case StatusNotFound:
		return "NOT_FOUND"
	case StatusAlreadyExists:
		return "ALREADY_EXISTS"
	case StatusPermissionDenied:
		return "PERMISSION_DENIED"
	case StatusResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	case StatusFailedPrecondition:
		return "FAILED_PRECONDITION"
	case StatusAborted:
		return "ABORTED"
	case StatusOutOfRange:
		return "OUT_OF_RANGE"
	case StatusUnimplemented:
		return "UNIMPLEMENTED"
	case StatusInternal:
		return "INTERNAL"
	case StatusUnavailable:
		return "UNAVAILABLE"
	case StatusDataLoss:
		return "DATA_LOSS"
	case StatusUnauthenticated:
		return "UNAUTHENTICATED"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", code)
	}
}

// IsGRPCRequest reports whether the incoming HTTP request is framed as gRPC.
func IsGRPCRequest(r *http.Request) bool {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, "application/grpc")
}

// ParseMethod extracts the gRPC service and method name from the URL path.
// Standard gRPC path format: /{package.Service}/{Method}
func ParseMethod(path string) (service, method string, err error) {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid gRPC path format '%s': expected /{package.Service}/{Method}", path)
	}
	return parts[0], parts[1], nil
}

// WriteGRPCError sends a trailer-only or header-trailed gRPC error response.
func WriteGRPCError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("grpc-status", strconv.Itoa(code))
	w.Header().Set("grpc-message", EncodeMessage(message))
	// Declare trailers for HTTP/2
	w.Header().Add("Trailer", "grpc-status")
	w.Header().Add("Trailer", "grpc-message")
	w.WriteHeader(http.StatusOK)
}

// EncodeFrame wraps a raw protobuf payload into a 5-byte length-prefixed gRPC frame.
func EncodeFrame(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = 0 // uncompressed
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// DecodeFrame extracts the payload from a 5-byte length-prefixed gRPC frame.
func DecodeFrame(r io.Reader) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[1:5])
	if length > 32<<20 { // 32 MiB safety guard
		return nil, errors.New("gRPC frame exceeds maximum allowed size (32 MiB)")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// EncodeMessage percent-encodes a grpc-message value as the gRPC HTTP/2
// spec requires: bytes outside printable ASCII, and '%', become %XX.
func EncodeMessage(msg string) string {
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if c >= 0x20 && c <= 0x7e && c != '%' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// HTTPStatus maps a gRPC status code to the closest HTTP status, so request
// logs, analytics and release safety treat a failed call like a failed HTTP
// request (the mapping in google.rpc.Code).
func HTTPStatus(code int) int {
	switch code {
	case StatusOK:
		return http.StatusOK
	case StatusCancelled:
		return 499
	case StatusInvalidArgument, StatusFailedPrecondition, StatusOutOfRange:
		return http.StatusBadRequest
	case StatusDeadlineExceeded:
		return http.StatusGatewayTimeout
	case StatusNotFound:
		return http.StatusNotFound
	case StatusAlreadyExists, StatusAborted:
		return http.StatusConflict
	case StatusPermissionDenied:
		return http.StatusForbidden
	case StatusResourceExhausted:
		return http.StatusTooManyRequests
	case StatusUnimplemented:
		return http.StatusNotImplemented
	case StatusUnavailable:
		return http.StatusServiceUnavailable
	case StatusUnauthenticated:
		return http.StatusUnauthorized
	default: // Unknown, Internal, DataLoss and unrecognised codes
		return http.StatusInternalServerError
	}
}

// ErrMessageTooLarge is returned by a MessageReader when a message exceeds
// its limit.
type ErrMessageTooLarge struct {
	Size, Limit int
}

func (e *ErrMessageTooLarge) Error() string {
	return fmt.Sprintf("gRPC message of %d bytes exceeds the %d byte limit", e.Size, e.Limit)
}

// MessageReader passes a gRPC message stream through unchanged while
// tracking frame boundaries: it counts messages and fails with
// ErrMessageTooLarge as soon as a frame header announces a message over
// MaxSize, before the payload is forwarded.
type MessageReader struct {
	src     io.ReadCloser
	MaxSize int
	// Messages counts the frame headers seen so far.
	Messages int
	hdr      [5]byte
	hdrN     int
	payload  uint64
	err      error
}

func NewMessageReader(src io.ReadCloser, maxSize int) *MessageReader {
	return &MessageReader{src: src, MaxSize: maxSize}
}

func (m *MessageReader) Read(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	n, err := m.src.Read(p)
	for i := 0; i < n; {
		if m.payload > 0 {
			skip := uint64(n - i)
			if skip > m.payload {
				skip = m.payload
			}
			m.payload -= skip
			i += int(skip)
			continue
		}
		m.hdr[m.hdrN] = p[i]
		m.hdrN++
		i++
		if m.hdrN == 5 {
			m.hdrN = 0
			m.Messages++
			size := binary.BigEndian.Uint32(m.hdr[1:5])
			if m.MaxSize > 0 && int64(size) > int64(m.MaxSize) {
				m.err = &ErrMessageTooLarge{Size: int(size), Limit: m.MaxSize}
				// Deliver the complete messages before this header; the
				// next Read reports the error.
				if start := i - 5; start > 0 {
					return start, nil
				}
				return 0, m.err
			}
			m.payload = uint64(size)
		}
	}
	return n, err
}

func (m *MessageReader) Close() error { return m.src.Close() }

// Err is the error that stopped the stream, if the reader stopped it.
func (m *MessageReader) Err() error { return m.err }
