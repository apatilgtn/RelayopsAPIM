package gateway

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/relayops/apim/internal/grpc"
	"github.com/relayops/apim/internal/mcpgw"
	"github.com/relayops/apim/internal/store"
)

// maxMCPResponseBytes bounds a JSON (non-streamed) MCP response the gateway
// inspects, and one server-sent event.
const maxMCPResponseBytes = 16 << 20

// mcpAccess is one governed request in an MCP message: a tool call, a
// prompt fetch or a resource read/subscription.
type mcpAccess struct {
	kind, name string
	args       json.RawMessage // tools/call arguments
}

// mcpExchange is one MCP JSON-RPC request in flight.
type mcpExchange struct {
	msgs    []mcpgw.Message
	batch   bool
	firstID json.RawMessage
	listIDs map[string]string    // list request id -> method (tools/list, prompts/list, ...)
	access  map[string]mcpAccess // request id -> governed access
	calls   map[string]string    // tools/call request id -> tool name
	policy  store.MCPPolicy
	apiID   string
	state   *mcpRouteState
	caller  mcpgw.Caller
	eval    map[string]any // the "mcp" entry of the decision trail
	mu      sync.Mutex
}

// parseMCP reads and parses a JSON-RPC request to an MCP API. The body is
// put back for the upstream.
func (g *Gateway) parseMCP(w http.ResponseWriter, r *http.Request, st *reqState, api *store.API) bool {
	ex := &mcpExchange{policy: api.MCPPolicy, apiID: api.ID, state: st.route.mcp, listIDs: map[string]string{},
		access: map[string]mcpAccess{}, calls: map[string]string{}, eval: map[string]any{}}
	st.mcp = ex
	st.policyEvaluations["mcp"] = ex.eval
	limit := api.MCPPolicy.MaxBodyBytes
	if limit <= 0 {
		limit = store.DefaultMCPMaxBody
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	r.Body.Close()
	st.decisionPolicy = "mcp"
	if err != nil {
		g.reject(w, st, http.StatusBadRequest, "mcp_body_unreadable", "could not read the request body")
		return false
	}
	if len(body) > limit {
		g.reject(w, st, http.StatusRequestEntityTooLarge, "mcp_body_too_large", fmt.Sprintf("JSON-RPC body exceeds %d bytes", limit))
		return false
	}
	setRequestBody(r, body)
	msgs, batch, err := mcpgw.ParseMessages(body)
	if err != nil {
		g.reject(w, st, http.StatusBadRequest, "mcp_parse_error", err.Error())
		return false
	}
	ex.msgs, ex.batch, ex.firstID = msgs, batch, msgs[0].ID
	var methods []string
	var audit []map[string]any
	for _, m := range msgs {
		if !m.IsRequest() {
			continue // a client's response to a server request
		}
		methods = append(methods, m.Method)
		id := m.IDKey()
		if mcpgw.IsListMethod(m.Method) {
			ex.listIDs[id] = m.Method
			continue
		}
		switch m.Method {
		case "tools/call":
			c, err := m.ToolCall()
			if err != nil {
				g.reject(w, st, http.StatusBadRequest, "mcp_invalid_tool_call", err.Error())
				return false
			}
			ex.calls[id] = c.Name
			ex.access[id] = mcpAccess{kind: mcpgw.KindTool, name: c.Name, args: c.Arguments}
			sum := sha256.Sum256(c.Arguments)
			// Audit the call without recording argument values.
			audit = append(audit, map[string]any{"tool": c.Name, "arguments_bytes": len(c.Arguments),
				"arguments_sha256": hex.EncodeToString(sum[:])})
		case "prompts/get":
			var p struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(m.Params, &p) != nil || p.Name == "" {
				g.reject(w, st, http.StatusBadRequest, "mcp_invalid_request", "prompts/get needs a prompt name")
				return false
			}
			ex.access[id] = mcpAccess{kind: mcpgw.KindPrompt, name: p.Name}
			audit = append(audit, map[string]any{"prompt": p.Name})
		case "resources/read", "resources/subscribe":
			var p struct {
				URI string `json:"uri"`
			}
			if json.Unmarshal(m.Params, &p) != nil || p.URI == "" {
				g.reject(w, st, http.StatusBadRequest, "mcp_invalid_request", m.Method+" needs a resource uri")
				return false
			}
			ex.access[id] = mcpAccess{kind: mcpgw.KindResource, name: p.URI}
			audit = append(audit, map[string]any{"resource": p.URI})
		}
	}
	ex.eval["methods"] = methods
	if len(audit) > 0 {
		ex.eval["tool_calls"] = audit
	}
	if len(methods) == 1 {
		st.matchedRoute = api.BasePath + " " + methods[0]
		for _, a := range ex.access {
			st.matchedRoute += " " + a.name
		}
	}
	st.decisionPolicy = ""
	return true
}

func mcpDriftKey(apiID, kind, name, pin string) string {
	return apiID + "|" + kind + "|" + name + "|" + pin
}

func (ex *mcpExchange) pin(kind, name string) string {
	switch kind {
	case mcpgw.KindTool:
		return ex.policy.PinnedTools[name]
	case mcpgw.KindPrompt:
		return ex.policy.PinnedPrompts[name]
	}
	return ""
}

// authorizeMCP applies rules, the approved catalog (pins and pending
// changes, fleet-wide), argument schemas and per-tool limits to every
// governed request in the message. If any is refused the gateway answers
// the whole message with JSON-RPC errors and nothing reaches the server.
func (g *Gateway) authorizeMCP(w http.ResponseWriter, r *http.Request, st *reqState, api *store.API, plan string) bool {
	ex := st.mcp
	ex.caller = mcpgw.Caller{ConsumerID: st.consumerID, PlanName: plan}
	if len(ex.access) == 0 {
		return true
	}
	var refused []mcpgw.Message
	refusedIDs := map[string]bool{}
	reason := ""
	refuse := func(m mcpgw.Message, d mcpgw.Decision) {
		refused = append(refused, mcpgw.ErrorResponse(m.ID, d.Code, d.Message, d.Reason))
		refusedIDs[m.IDKey()] = true
		reason = d.Reason
	}
	var toCharge []mcpgw.Message
	for _, m := range ex.msgs {
		a, ok := ex.access[m.IDKey()]
		if !ok || !m.IsRequest() {
			continue
		}
		d := mcpgw.AuthorizeKind(ex.policy, ex.caller, a.kind, a.name)
		if pin := ex.pin(a.kind, a.name); d.Allowed && pin != "" {
			_, driftedHere := g.mcpDrift.Load(mcpDriftKey(api.ID, a.kind, a.name, pin))
			if driftedHere || ex.state.isBlocked(a.kind, a.name, pin) {
				d = mcpgw.Decision{Code: mcpgw.CodeToolNotPinned, Reason: a.kind + "_definition_changed",
					Message: fmt.Sprintf("%s %q changed since it was approved; it is held for review", a.kind, a.name)}
			}
		}
		if d.Allowed && a.kind == mcpgw.KindTool && ex.policy.ValidateArguments {
			if msg := validateToolArguments(ex.state.schema(a.name), a.args); msg != "" {
				d = mcpgw.Decision{Code: mcpgw.CodeInvalidParams, Reason: "invalid_tool_arguments",
					Message: fmt.Sprintf("arguments for tool %q do not match its approved input schema: %s", a.name, msg)}
			}
		}
		if !d.Allowed {
			refuse(m, d)
			continue
		}
		if a.kind == mcpgw.KindTool {
			toCharge = append(toCharge, m)
		}
	}
	// Per-tool limits are charged only when nothing is refused by policy, so
	// a refused request does not use up the caller's allowance.
	if len(refused) == 0 && ex.policy.ToolCallsPerMinute > 0 {
		who := st.consumerID
		if who == "" {
			who = "ip:" + clientIP(r)
		}
		lim := &LimiterAdapter{gw: g}
		for _, m := range toCharge {
			tool := ex.calls[m.IDKey()]
			if allowed, _, retry, _ := lim.CheckRateLimit(r.Context(), "mcp|"+api.ID+"|"+who+"|"+tool, ex.policy.ToolCallsPerMinute); !allowed {
				refuse(m, mcpgw.Decision{Code: mcpgw.CodeToolRateLimited, Reason: "tool_rate_limited",
					Message: fmt.Sprintf("call limit for tool %q reached; retry in %ds", tool, int(retry.Seconds())+1)})
			}
		}
	}
	if len(refused) == 0 {
		return true
	}
	if ex.batch {
		for _, m := range ex.msgs {
			if m.IsRequest() && len(m.ID) > 0 && !refusedIDs[m.IDKey()] {
				refused = append(refused, mcpgw.ErrorResponse(m.ID, mcpgw.CodeToolNotAllowed,
					"batch refused: another request in it was not permitted", "batch_refused"))
			}
		}
	}
	ex.eval["refused"] = reason
	st.decisionPolicy, st.decisionReason = "mcp", reason
	st.err = reason
	w.Header().Set("X-RelayOps-Decision-Policy", "mcp")
	w.Header().Set("X-RelayOps-Decision-Reason", reason)
	w.Header().Set("Content-Type", "application/json")
	// A refused call is a JSON-RPC error, which MCP clients surface to the
	// model; the HTTP exchange itself succeeded.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(mcpgw.Encode(refused, ex.batch))
	return false
}

// validateToolArguments checks arguments against an approved schema. With
// no schema available (the tool was approved before schemas were recorded,
// or its schema did not compile) the arguments are not checked.
func validateToolArguments(sch *jsonschema.Schema, args json.RawMessage) string {
	if sch == nil {
		return ""
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return "arguments are not valid JSON"
	}
	err = sch.Validate(inst)
	if err == nil {
		return ""
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	var msgs []string
	for _, c := range ve.BasicOutput().Errors {
		if c.Error == nil {
			continue
		}
		loc := c.InstanceLocation
		if loc == "" {
			loc = "/"
		}
		msgs = append(msgs, loc+": "+c.Error.String())
		if len(msgs) == 3 {
			break
		}
	}
	if len(msgs) == 0 {
		return ve.Error()
	}
	return strings.Join(msgs, "; ")
}

// writeMCPError answers a request the gateway refused before tool policy
// (authentication, rate limits, malformed input) as a JSON-RPC error with
// the HTTP status the refusal carries.
func writeMCPError(w http.ResponseWriter, status int, ex *mcpExchange, code, msg string) {
	rpcCode := -32000
	switch code {
	case "mcp_parse_error":
		rpcCode = mcpgw.CodeParseError
	case "mcp_invalid_tool_call":
		rpcCode = mcpgw.CodeInvalidRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(mcpgw.ErrorResponse(ex.firstID, rpcCode, msg, code))
	_, _ = w.Write(b)
}

// modifyResponse enforces gRPC response message limits, filters MCP
// tools/list results and observes tools/call results; other responses pass
// through untouched.
func (g *Gateway) modifyResponse(resp *http.Response) error {
	st, _ := resp.Request.Context().Value(ctxKey{}).(*reqState)
	if st != nil && st.grpc != nil {
		x := st.grpc
		limit := st.route.API.GRPCPolicy.MaxMessageSizeBytes
		if limit <= 0 {
			limit = defaultGRPCMaxMessage
		}
		if x.json != nil {
			x.transcodeResponse(resp, limit)
			return nil
		}
		if isTrailersOnly(resp) {
			if x.web != "" {
				code, _ := strconv.Atoi(resp.Header.Get("Grpc-Status"))
				frame := trailerFrame(code, decodeGRPCMessage(resp.Header.Get("Grpc-Message")), nil)
				if x.web == webText {
					frame = []byte(base64.StdEncoding.EncodeToString(frame))
				}
				x.finalStatus = &code
				resp.Header.Set("Content-Type", x.webContentTypeOut(resp.Header.Get("Content-Type")))
				resp.Header.Set("Content-Length", strconv.Itoa(len(frame)))
				resp.Body = io.NopCloser(bytes.NewReader(frame))
				resp.ContentLength = int64(len(frame))
				return nil
			}
			// Answered by proxyError as one headers frame; see trailersOnly.
			return &trailersOnly{resp: resp}
		}
		x.resp = grpc.NewMessageReader(resp.Body, limit)
		resp.Body = x.resp
		if x.web != "" {
			resp.Header.Set("Content-Type", x.webContentTypeOut(resp.Header.Get("Content-Type")))
			resp.Header.Del("Content-Length")
			resp.ContentLength = -1
			resp.Body = &grpcWebBody{src: x.resp, resp: resp, x: x}
		}
		return nil
	}
	if st != nil && st.gqlWS != nil && resp.StatusCode == http.StatusSwitchingProtocols {
		if rwc, ok := resp.Body.(io.ReadWriteCloser); ok {
			// "graphql-ws" is the older subscriptions-transport-ws protocol.
			st.gqlWS.legacy = resp.Header.Get("Sec-WebSocket-Protocol") == "graphql-ws"
			resp.Body = newGQLWSConn(rwc, st.gqlWS)
		}
		return nil
	}
	if st != nil && st.route != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		// Response-phase WASM plugins run last, on what the client will get.
		defer g.runWasmResponse(resp, st)
	}
	if st == nil || st.mcp == nil || (len(st.mcp.listIDs) == 0 && len(st.mcp.calls) == 0) {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch ct {
	case "application/json":
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponseBytes+1))
		if err != nil {
			return err
		}
		if len(body) > maxMCPResponseBytes {
			if len(st.mcp.listIDs) > 0 {
				return errors.New("MCP tools/list response too large to filter")
			}
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
			return nil
		}
		resp.Body.Close()
		out := g.processMCPResponse(st, body)
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	case "text/event-stream":
		resp.Body = newSSERewriter(resp.Body, func(data []byte) []byte { return g.processMCPResponse(st, data) })
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
	}
	return nil
}

// processMCPResponse rewrites the server messages in one JSON body or SSE
// event: list results are filtered, tools/call results are observed. Data
// that is not JSON-RPC is returned unchanged.
func (g *Gateway) processMCPResponse(st *reqState, data []byte) []byte {
	ex := st.mcp
	msgs, batch, err := mcpgw.ParseMessages(data)
	if err != nil {
		return data
	}
	ex.mu.Lock()
	defer ex.mu.Unlock()
	changed := false
	for i, m := range msgs {
		if m.IsRequest() || len(m.Result) == 0 {
			continue
		}
		id := m.IDKey()
		if method, ok := ex.listIDs[id]; ok {
			filtered, statuses, err := mcpgw.FilterList(method, m.Result, ex.policy, ex.caller)
			if err != nil {
				// Unparseable list: hide everything rather than pass it on.
				filtered = emptyList(method)
			}
			msgs[i].Result, changed = filtered, true
			g.recordMCPCatalog(ex, method, statuses)
		}
		if tool, ok := ex.calls[id]; ok {
			var res struct {
				IsError bool `json:"isError"`
			}
			if json.Unmarshal(m.Result, &res) == nil && res.IsError {
				errs, _ := ex.eval["tool_errors"].([]string)
				ex.eval["tool_errors"] = append(errs, tool)
			}
		}
	}
	if !changed {
		return data
	}
	return mcpgw.Encode(msgs, batch)
}

func emptyList(method string) json.RawMessage {
	switch method {
	case "prompts/list":
		return json.RawMessage(`{"prompts":[]}`)
	case "resources/list":
		return json.RawMessage(`{"resources":[]}`)
	case "resources/templates/list":
		return json.RawMessage(`{"resourceTemplates":[]}`)
	}
	return json.RawMessage(`{"tools":[]}`)
}

// recordMCPCatalog updates drift memory, reports definitions that need
// review to the control plane, and fills the decision trail.
func (g *Gateway) recordMCPCatalog(ex *mcpExchange, method string, statuses []mcpgw.ItemStatus) {
	var hidden, unpinned, changed []string
	for _, s := range statuses {
		pin := ex.pin(s.Kind, s.Name)
		switch s.Status {
		case "changed":
			changed = append(changed, s.Name)
			g.mcpDrift.Store(mcpDriftKey(ex.apiID, s.Kind, s.Name, pin), s.Fingerprint)
		case "unpinned":
			unpinned = append(unpinned, s.Name)
		case "hidden":
			hidden = append(hidden, s.Name)
		}
		if pin != "" && s.Status != "changed" {
			g.mcpDrift.Delete(mcpDriftKey(ex.apiID, s.Kind, s.Name, pin))
		}
		// With an approved catalog, anything new or changed needs review.
		if (s.Status == "changed" || s.Status == "unpinned") && s.Fingerprint != "" {
			g.observeMCP(store.MCPObservation{APIID: ex.apiID, Kind: s.Kind, Name: s.Name, Fingerprint: s.Fingerprint,
				Definition: s.Definition, Status: "pending", Source: "observed"})
		}
	}
	cat := map[string]any{"listed": len(statuses)}
	if len(hidden) > 0 {
		cat["hidden"] = hidden
	}
	if len(unpinned) > 0 {
		cat["unpinned"] = unpinned
	}
	if len(changed) > 0 {
		cat["changed"] = changed
		g.mcpDriftEvents.Add(uint64(len(changed)))
	}
	key := "catalog"
	if method != "tools/list" {
		key = strings.TrimSuffix(strings.ReplaceAll(method, "/", "_"), "_list") + "_catalog"
	}
	ex.eval[key] = cat
}

// sseRewriter transforms each server-sent event's data, one event at a time,
// so streaming responses stay streaming.
type sseRewriter struct {
	src  io.ReadCloser
	br   *bufio.Reader
	fn   func([]byte) []byte
	out  bytes.Buffer
	done error
}

func newSSERewriter(src io.ReadCloser, fn func([]byte) []byte) *sseRewriter {
	return &sseRewriter{src: src, br: bufio.NewReaderSize(src, 64<<10), fn: fn}
}

func (s *sseRewriter) Read(p []byte) (int, error) {
	for s.out.Len() == 0 {
		if s.done != nil {
			return 0, s.done
		}
		s.done = s.nextEvent()
	}
	return s.out.Read(p)
}

func (s *sseRewriter) Close() error { return s.src.Close() }

// nextEvent reads one event (up to a blank line) into out.
func (s *sseRewriter) nextEvent() error {
	var other []string
	var data [][]byte
	size := 0
	for {
		line, err := s.br.ReadBytes('\n')
		size += len(line)
		if size > maxMCPResponseBytes {
			return errors.New("MCP event too large")
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 && len(line) > 0 {
			break // end of event
		}
		if len(trimmed) > 0 {
			if v, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
				data = append(data, bytes.TrimPrefix(v, []byte(" ")))
			} else {
				other = append(other, string(trimmed))
			}
		}
		if err != nil {
			if len(data) == 0 && len(other) == 0 {
				return err
			}
			s.emit(other, data)
			return err
		}
	}
	s.emit(other, data)
	return nil
}

func (s *sseRewriter) emit(other []string, data [][]byte) {
	for _, l := range other {
		s.out.WriteString(l)
		s.out.WriteByte('\n')
	}
	if len(data) > 0 {
		payload := s.fn(bytes.Join(data, []byte("\n")))
		for _, l := range bytes.Split(payload, []byte("\n")) {
			s.out.WriteString("data: ")
			s.out.Write(l)
			s.out.WriteByte('\n')
		}
	}
	s.out.WriteByte('\n')
}
