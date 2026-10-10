package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// RunStdio executes the MCP server over standard input and output streams.
func RunStdio(ctx context.Context, engine *Engine, caller CallerContext, in io.Reader, out io.Writer, log io.Writer) error {
	scanner := bufio.NewScanner(in)
	// Allow large messages (e.g. OpenAPI specs up to 10MB)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	if log != nil {
		fmt.Fprintf(log, "[relayops-mcp] Server started (role=%s, consumer=%s)\n", caller.Role, caller.ConsumerName)
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			errResp := JSONRPCResponse{
				JSONRPC: "2.0",
				Error: &JSONRPCError{
					Code:    CodeParseError,
					Message: "parse error: " + err.Error(),
				},
			}
			data, _ := json.Marshal(errResp)
			_, _ = fmt.Fprintf(out, "%s\n", data)
			continue
		}

		resp := engine.HandleRequest(ctx, caller, req)
		data, err := json.Marshal(resp)
		if err != nil {
			errResp := JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &JSONRPCError{
					Code:    CodeInternalError,
					Message: "failed to serialize response: " + err.Error(),
				},
			}
			data, _ = json.Marshal(errResp)
		}

		if _, err := fmt.Fprintf(out, "%s\n", data); err != nil {
			return err
		}
	}

	return scanner.Err()
}
