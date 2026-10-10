package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

type session struct {
	id     string
	caller CallerContext
	msgCh  chan []byte
}

// Handler provides HTTP endpoints for MCP over Server-Sent Events (SSE) and HTTP POST.
type Handler struct {
	engine   *Engine
	sessions sync.Map // map[string]*session
}

// NewHandler creates a new MCP HTTP handler.
func NewHandler(engine *Engine) *Handler {
	return &Handler{
		engine: engine,
	}
}

// HandleSSE handles the GET /mcp/sse endpoint.
func (h *Handler) HandleSSE(w http.ResponseWriter, r *http.Request) {
	// CORS support
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Extract credentials
	tokenOrKey := h.extractAuth(r)
	caller := h.engine.AuthenticateCaller(r.Context(), tokenOrKey)

	// Generate session ID
	randBytes := make([]byte, 16)
	_, _ = rand.Read(randBytes)
	sessionID := hex.EncodeToString(randBytes)

	sess := &session{
		id:     sessionID,
		caller: caller,
		msgCh:  make(chan []byte, 64),
	}
	h.sessions.Store(sessionID, sess)
	defer h.sessions.Delete(sessionID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Standard MCP protocol: emit endpoint event indicating where to send messages
	endpointURI := fmt.Sprintf("/mcp/message?sessionId=%s", sessionID)
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpointURI)
	flusher.Flush()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case msg, ok := <-sess.msgCh:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", string(msg))
			flusher.Flush()
		}
	}
}

// HandleMessage handles the POST /mcp/message endpoint.
func (h *Handler) HandleMessage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	sessionID := r.URL.Query().Get("sessionId")
	var sess *session
	if sessionID != "" {
		if val, ok := h.sessions.Load(sessionID); ok {
			sess = val.(*session)
		}
	}

	// Resolve caller
	var caller CallerContext
	if sess != nil {
		caller = sess.caller
	} else {
		tokenOrKey := h.extractAuth(r)
		caller = h.engine.AuthenticateCaller(r.Context(), tokenOrKey)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		errResp := JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   &JSONRPCError{Code: CodeParseError, Message: "Parse error: " + err.Error()},
		}
		data, _ := json.Marshal(errResp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	resp := h.engine.HandleRequest(r.Context(), caller, req)
	respBytes, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "Internal error serializing response", http.StatusInternalServerError)
		return
	}

	// If client is listening on an SSE stream session, stream the response over SSE
	if sess != nil {
		select {
		case sess.msgCh <- respBytes:
			w.WriteHeader(http.StatusAccepted)
			return
		default:
		}
	}

	// Fallback to direct HTTP response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

func (h *Handler) extractAuth(r *http.Request) string {
	// 1. Authorization: Bearer <token_or_key>
	if auth := r.Header.Get("Authorization"); auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
		return strings.TrimSpace(auth)
	}

	// 2. X-API-Key header
	if key := r.Header.Get("X-API-Key"); key != "" {
		return strings.TrimSpace(key)
	}

	// 3. Query params: ?key=... or ?token=...
	if key := r.URL.Query().Get("key"); key != "" {
		return strings.TrimSpace(key)
	}
	if token := r.URL.Query().Get("token"); token != "" {
		return strings.TrimSpace(token)
	}

	return ""
}
