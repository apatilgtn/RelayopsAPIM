package mcpgw

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// ProtocolVersion is the MCP revision the control plane speaks when it
// lists an upstream server's tools.
const ProtocolVersion = "2025-06-18"

const maxDiscoverPages = 20

// Discovered is what an MCP server offers.
type Discovered struct {
	Tools   []json.RawMessage
	Prompts []json.RawMessage
	// Warnings are optional lists the server advertised but could not
	// serve (some servers announce prompts without implementing them).
	Warnings []string
}

// Discover connects to an MCP server over Streamable HTTP, initialises a
// session and returns every tool definition it lists.
func Discover(ctx context.Context, client *http.Client, endpoint string, headers map[string]string) ([]json.RawMessage, error) {
	d, err := DiscoverAll(ctx, client, endpoint, headers)
	return d.Tools, err
}

// DiscoverAll returns the server's tools and prompts.
func DiscoverAll(ctx context.Context, client *http.Client, endpoint string, headers map[string]string) (Discovered, error) {
	var out Discovered
	c := &rpcClient{client: client, endpoint: endpoint, headers: headers}
	init, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "relayops-control-plane", "version": "1"},
	})
	if err != nil {
		return out, fmt.Errorf("initialize: %w", err)
	}
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools   json.RawMessage `json:"tools"`
			Prompts json.RawMessage `json:"prompts"`
		} `json:"capabilities"`
	}
	_ = json.Unmarshal(init, &initRes)
	if initRes.ProtocolVersion != "" {
		c.version = initRes.ProtocolVersion
	}
	defer c.close()
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return out, fmt.Errorf("initialized notification: %w", err)
	}
	if len(initRes.Capabilities.Tools) > 0 {
		if out.Tools, err = c.list(ctx, "tools/list", "tools"); err != nil {
			return out, err
		}
	}
	if len(initRes.Capabilities.Prompts) > 0 {
		if out.Prompts, err = c.list(ctx, "prompts/list", "prompts"); err != nil {
			out.Prompts = nil
			out.Warnings = append(out.Warnings, "prompts were advertised but could not be listed: "+err.Error())
		}
	}
	if out.Tools == nil {
		out.Tools = []json.RawMessage{}
	}
	return out, nil
}

// list pages through a list method.
func (c *rpcClient) list(ctx context.Context, method, key string) ([]json.RawMessage, error) {
	var items []json.RawMessage
	cursor := ""
	for page := 0; page < maxDiscoverPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.call(ctx, method, params)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(res, &obj); err != nil {
			return nil, fmt.Errorf("%s result: %w", method, err)
		}
		var got []json.RawMessage
		if raw, ok := obj[key]; ok {
			if err := json.Unmarshal(raw, &got); err != nil {
				return nil, fmt.Errorf("%s result: %w", method, err)
			}
		}
		items = append(items, got...)
		var next string
		_ = json.Unmarshal(obj["nextCursor"], &next)
		if cursor = next; cursor == "" {
			return items, nil
		}
	}
	return nil, fmt.Errorf("%s returned more than %d pages", method, maxDiscoverPages)
}

type rpcClient struct {
	client   *http.Client
	endpoint string
	headers  map[string]string
	session  string
	version  string
	nextID   int
}

func (c *rpcClient) request(ctx context.Context, method string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	return c.client.Do(req)
}

// call sends a request and returns its result.
func (c *rpcClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	resp, err := c.request(ctx, http.MethodPost, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	msg, err := readResponse(resp, fmt.Sprint(id))
	if err != nil {
		return nil, err
	}
	if len(msg.Error) > 0 {
		var e struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(msg.Error, &e)
		return nil, fmt.Errorf("server error %d: %s", e.Code, e.Message)
	}
	return msg.Result, nil
}

func (c *rpcClient) notify(ctx context.Context, method string) error {
	resp, err := c.request(ctx, http.MethodPost, map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// close ends the session (best effort; servers may not support it).
func (c *rpcClient) close() {
	if c.session == "" {
		return
	}
	if resp, err := c.request(context.Background(), http.MethodDelete, nil); err == nil {
		resp.Body.Close()
	}
}

// readResponse finds the response with the given id in a JSON body or an SSE
// stream.
func readResponse(resp *http.Response, id string) (Message, error) {
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	body := io.LimitReader(resp.Body, maxMCPResponse)
	match := func(data []byte) (Message, bool) {
		msgs, _, err := ParseMessages(data)
		if err != nil {
			return Message{}, false
		}
		for _, m := range msgs {
			if !m.IsRequest() && m.IDKey() == id {
				return m, true
			}
		}
		return Message{}, false
	}
	if ct == "text/event-stream" {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64<<10), maxMCPResponse)
		var data []byte
		for sc.Scan() {
			line := sc.Bytes()
			if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				data = append(data, bytes.TrimPrefix(v, []byte(" "))...)
				continue
			}
			if len(line) == 0 && len(data) > 0 {
				if m, ok := match(data); ok {
					return m, nil
				}
				data = data[:0]
			}
		}
		if m, ok := match(data); ok {
			return m, nil
		}
		return Message{}, errors.New("no response in the event stream")
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return Message{}, err
	}
	if m, ok := match(b); ok {
		return m, nil
	}
	return Message{}, errors.New("response did not contain the expected JSON-RPC result")
}

const maxMCPResponse = 16 << 20

// CatalogEntry is one tool or prompt as discovered, compared with the
// API's pins.
type CatalogEntry struct {
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Fingerprint string          `json:"fingerprint"`
	Status      string          `json:"status"` // pinned, changed, new
	Definition  json.RawMessage `json:"definition"`
}

// Catalog compares discovered tools with pins. Removed lists pinned tools the
// server no longer offers.
func Catalog(tools []json.RawMessage, pins map[string]string) (entries []CatalogEntry, removed []string) {
	return CatalogOf(KindTool, tools, pins)
}

// CatalogOf compares discovered definitions of one kind with pins.
func CatalogOf(kind string, defs []json.RawMessage, pins map[string]string) (entries []CatalogEntry, removed []string) {
	seen := map[string]bool{}
	for _, t := range defs {
		name, fp, err := FingerprintOf(kind, t)
		if err != nil {
			continue
		}
		var meta struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(t, &meta)
		e := CatalogEntry{Kind: kind, Name: name, Title: meta.Title, Description: meta.Description, Fingerprint: fp, Status: "new", Definition: t}
		if pin, ok := pins[name]; ok {
			e.Status = "pinned"
			if pin != fp {
				e.Status = "changed"
			}
		}
		seen[name] = true
		entries = append(entries, e)
	}
	for name := range pins {
		if !seen[name] {
			removed = append(removed, name)
		}
	}
	return entries, removed
}
