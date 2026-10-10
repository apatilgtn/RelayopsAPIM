package dataplane

import (
	"context"

	"github.com/relayops/apim/internal/store"
)

// PathMCPObservations receives MCP tool and prompt definitions a
// gateway-only node saw that need review.
const PathMCPObservations = "/dataplane/v1/mcp/observations"

// MCPReporter sends a gateway-only node's MCP observations to the control
// plane (gateway.MCPReporter).
type MCPReporter struct{ c *Client }

func NewMCPReporter(c *Client) *MCPReporter { return &MCPReporter{c: c} }

func (m *MCPReporter) ReportMCPObservations(ctx context.Context, obs []store.MCPObservation) error {
	return m.c.postJSON(ctx, PathMCPObservations, obs, nil, true)
}
