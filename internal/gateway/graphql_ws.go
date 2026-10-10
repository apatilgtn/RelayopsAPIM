package gateway

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/relayops/apim/internal/graphql"
	"github.com/relayops/apim/internal/store"
)

// GraphQL over WebSocket (graphql-transport-ws and the older
// subscriptions-transport-ws). The proxied connection is wrapped so every
// operation a client starts is checked with the same analysis and policy as
// HTTP requests before it reaches the server; a refused operation is
// answered with an error message for its id and never forwarded.

const maxGQLWSMessage = 1 << 20

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// gqlWSCheck is the policy applied to each operation on a connection.
type gqlWSCheck struct {
	g      *Gateway
	api    *store.API
	route  *Route
	st     *reqState
	legacy bool // subscriptions-transport-ws ("start"/"error" payload object)
}

// check returns a refusal message, or "" to forward the operation.
func (c *gqlWSCheck) check(req *graphql.Request) string {
	pol := c.api.GraphQLPolicy
	if err := graphql.ResolvePersisted(req, c.g.persistedCache(c.api.ID)); err != nil {
		return err.Error()
	}
	analysis, err := graphql.AnalyzeOperation(req.Query, req.OperationName, req.Variables, pol.ListSizeArguments)
	if err != nil {
		return err.Error()
	}
	if err := graphql.ValidatePolicy(req, analysis, pol); err != nil {
		return err.Error()
	}
	if c.route.graphqlSchema != nil {
		if errs := graphql.ValidateAgainstSchema(c.route.graphqlSchema, req.Query); len(errs) > 0 {
			return strings.Join(errs, "; ")
		}
	}
	c.g.persistedCache(c.api.ID).Remember(req)
	return ""
}

// gqlWSConn wraps the upstream side of an upgraded connection. Write carries
// client frames to the server; Read carries server frames to the client.
type gqlWSConn struct {
	up    io.ReadWriteCloser
	check *gqlWSCheck

	in  bytes.Buffer // client bytes not yet forming a complete frame
	msg bytes.Buffer // payload of the client message being reassembled
	raw bytes.Buffer // raw frames of that message (forwarded if allowed)
	op  byte         // opcode of the message being reassembled

	pr      *io.PipeReader
	pw      *io.PipeWriter
	writeMu sync.Mutex // serialises whole frames into pw
	refused int
}

func newGQLWSConn(up io.ReadWriteCloser, check *gqlWSCheck) *gqlWSConn {
	c := &gqlWSConn{up: up, check: check}
	c.pr, c.pw = io.Pipe()
	go c.pumpServer()
	return c
}

// pumpServer copies server frames to the client side whole, so injected
// error frames never land inside a server frame.
func (c *gqlWSConn) pumpServer() {
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, err := c.up.Read(chunk)
		buf.Write(chunk[:n])
		for {
			size, ok := frameSize(buf.Bytes())
			if !ok {
				break
			}
			c.writeMu.Lock()
			_, werr := c.pw.Write(buf.Next(size))
			c.writeMu.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
			c.writeMu.Lock()
			if buf.Len() > 0 {
				_, _ = c.pw.Write(buf.Bytes())
			}
			c.writeMu.Unlock()
			_ = c.pw.CloseWithError(err)
			return
		}
	}
}

func (c *gqlWSConn) Read(p []byte) (int, error) { return c.pr.Read(p) }

func (c *gqlWSConn) Close() error {
	_ = c.pw.Close()
	return c.up.Close()
}

// Write receives client bytes, forwards complete frames, and holds back the
// frames of any operation the policy refuses.
func (c *gqlWSConn) Write(p []byte) (int, error) {
	c.in.Write(p)
	for {
		size, ok := frameSize(c.in.Bytes())
		if !ok {
			if c.in.Len() > maxGQLWSMessage+14 {
				return 0, errors.New("websocket frame too large")
			}
			return len(p), nil
		}
		frame := c.in.Next(size)
		fin, opcode, payload := decodeFrame(frame)
		if opcode >= 0x8 { // control frames: close, ping, pong
			if _, err := c.up.Write(frame); err != nil {
				return 0, err
			}
			continue
		}
		if opcode != 0 {
			c.op = opcode
		}
		c.raw.Write(frame)
		c.msg.Write(payload)
		if c.msg.Len() > maxGQLWSMessage {
			return 0, errors.New("websocket message too large")
		}
		if !fin {
			continue
		}
		forward := true
		if c.op == 0x1 {
			forward = c.inspect(c.msg.Bytes())
		}
		if forward {
			if _, err := c.up.Write(c.raw.Bytes()); err != nil {
				return 0, err
			}
		}
		c.raw.Reset()
		c.msg.Reset()
	}
}

// inspect checks one client text message and reports whether to forward it.
func (c *gqlWSConn) inspect(data []byte) bool {
	var m struct {
		ID      string          `json:"id"`
		Type    string          `json:"type"`
		Payload graphql.Request `json:"payload"`
	}
	if json.Unmarshal(data, &m) != nil || (m.Type != "subscribe" && m.Type != "start") {
		return true // connection_init, ping, complete, ...
	}
	reason := c.check.check(&m.Payload)
	if reason == "" {
		return true
	}
	c.refused++
	if ev, ok := c.check.st.policyEvaluations["graphql"].(map[string]any); ok {
		ev["websocket_refused"] = c.refused
		ev["last_refusal"] = reason
	}
	var reply []byte
	if c.check.legacy {
		reply, _ = json.Marshal(map[string]any{"id": m.ID, "type": "error", "payload": map[string]string{"message": reason}})
	} else {
		reply, _ = json.Marshal(map[string]any{"id": m.ID, "type": "error", "payload": []map[string]string{{"message": reason}}})
	}
	c.writeMu.Lock()
	_, _ = c.pw.Write(encodeServerFrame(0x1, reply))
	c.writeMu.Unlock()
	return false
}

// frameSize returns the length of the first complete frame in b.
func frameSize(b []byte) (int, bool) {
	if len(b) < 2 {
		return 0, false
	}
	n := 2
	l := uint64(b[1] & 0x7f)
	switch l {
	case 126:
		if len(b) < 4 {
			return 0, false
		}
		l, n = uint64(binary.BigEndian.Uint16(b[2:4])), 4
	case 127:
		if len(b) < 10 {
			return 0, false
		}
		l, n = binary.BigEndian.Uint64(b[2:10]), 10
	}
	if b[1]&0x80 != 0 {
		n += 4
	}
	if l > maxGQLWSMessage*2 {
		return 0, false
	}
	total := n + int(l)
	if len(b) < total {
		return 0, false
	}
	return total, true
}

// decodeFrame returns a complete frame's FIN bit, opcode and unmasked payload.
func decodeFrame(f []byte) (fin bool, opcode byte, payload []byte) {
	fin, opcode = f[0]&0x80 != 0, f[0]&0x0f
	n := 2
	switch f[1] & 0x7f {
	case 126:
		n = 4
	case 127:
		n = 10
	}
	if f[1]&0x80 != 0 {
		key := f[n : n+4]
		n += 4
		payload = make([]byte, len(f)-n)
		for i := range payload {
			payload[i] = f[n+i] ^ key[i%4]
		}
		return fin, opcode, payload
	}
	return fin, opcode, f[n:]
}

// encodeServerFrame builds an unmasked (server-to-client) frame.
func encodeServerFrame(opcode byte, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteByte(0x80 | opcode)
	switch l := len(payload); {
	case l < 126:
		b.WriteByte(byte(l))
	case l < 1<<16:
		b.WriteByte(126)
		_ = binary.Write(&b, binary.BigEndian, uint16(l))
	default:
		b.WriteByte(127)
		_ = binary.Write(&b, binary.BigEndian, uint64(l))
	}
	b.Write(payload)
	return b.Bytes()
}
