# MCP tool gateway

RelayOps can sit in front of [Model Context Protocol](https://modelcontextprotocol.io)
servers, the tool servers AI agents call. You get the same governance as for
APIs, applied per tool: who may call which tool, an approved tool catalog, call
limits, an audit trail, and release safety for every policy change.

```
agent (MCP client) ──► RelayOps gateway ──► MCP server (GitHub, Jira, internal tools…)
                        auth · tool rules · approved catalog · per-tool limits · audit
```

## Why put a gateway in front of MCP servers

| Risk | What the gateway does |
|---|---|
| Agents can call every tool a server exposes | Rules allow or deny tools (exact names or globs) per consumer or plan. Denied tools are also removed from `tools/list`, so the model never sees them. |
| **Tool poisoning / rug pull**: a server changes a tool's description or schema after you trusted it (for example, by adding hidden instructions) | Approved tools are **pinned** by a SHA-256 fingerprint of their definition (name, title, description, input/output schema, annotations). A tool whose definition changes is hidden from `tools/list` and its calls are refused (`tool_definition_changed`) until someone reviews the new definition and approves it again. Tools that were never approved are not exposed. |
| Runaway agents | `tool_calls_per_minute` limits each caller's calls to each tool (distributed through Redis like other rate limits). |
| No record of what agents did | Every request records the JSON-RPC methods, the tool called, the argument size and SHA-256 (not the values), tools hidden or changed in `tools/list`, and tool-reported errors. The tool name is the request's route, so analytics break down by tool. |
| Server credentials spread to every agent | The API's `request_headers` inject the server's credentials (`${secret:NAME}` references allowed). Agents authenticate to RelayOps with their own API key, JWT or OIDC token. |
| Unsafe policy changes | Rules and approvals are API configuration. They go through revisions, approvals, canaries and auto-rollback like any other change. |

## Configure

Create an API with `protocol: "mcp"`. Its upstream URL is the MCP server's
Streamable HTTP endpoint. A request to the exact base path is sent to the
upstream URL unchanged.

```json
{
  "name": "github-tools",
  "base_path": "/mcp/github",
  "upstream_url": "https://mcp.example.com/mcp",
  "protocol": "mcp",
  "auth_type": "api_key",
  "request_headers": {"Authorization": "Bearer ${secret:GITHUB_MCP_TOKEN}"},
  "mcp_policy": {
    "default_action": "allow",
    "rules": [
      {"tools": ["delete_*", "admin_*"], "plans": ["Enterprise"], "action": "allow"},
      {"tools": ["delete_*", "admin_*"], "action": "deny"}
    ],
    "pinned_tools": {
      "search_issues": "sha256:…",
      "create_issue":  "sha256:…"
    },
    "tool_calls_per_minute": 60
  }
}
```

| `mcp_policy` field | Meaning |
|---|---|
| `default_action` | `allow` (default) or `deny` when no rule matches |
| `rules` | Evaluated in order, first match wins. `tools` holds names or globs. Empty `consumers` or `plans` match any caller. |
| `pinned_tools` | Approved catalog: tool name to definition fingerprint. When it is non-empty, only pinned tools with an unchanged definition are exposed. |
| `pinned_prompts` | The same for prompts. Prompt templates reach the model, so a changed prompt is held as well. |
| `resource_rules`, `prompt_rules` | `[{match, consumers, plans, action}]` for resource URIs and prompt names. In `match`, `*` matches any run of characters, including `/`. Denied items are removed from `resources/list`, `resources/templates/list` and `prompts/list`. Denied `resources/read`, `resources/subscribe` and `prompts/get` are refused. |
| `validate_arguments` | Checks `tools/call` arguments against the approved tool's `inputSchema` (JSON Schema). Calls that don't match are refused with `-32602 invalid_tool_arguments` before they reach the server. |
| `tool_calls_per_minute` | Per caller, per tool. 0 turns the limit off. |
| `max_body_bytes` | JSON-RPC request size limit (default 1 MiB) |

### The catalog, reviews and alerts

The control plane keeps a catalog of every tool and prompt definition seen
on each MCP API. Entries come from gateways observing `tools/list` and
`prompts/list`, and from **Discover**.

1. A gateway sees a definition that is not in the approved catalog: a new
   tool, or an approved tool whose definition changed. The gateway hides it
   and refuses calls to it right away. It then reports the definition to the
   control plane, through the database or, for gateway-only nodes, the node
   API. Each definition is reported at most once an hour per node.
2. The control plane records the definition as **pending**. It writes an
   audit entry, shows a live alert in open consoles, and posts the alert to
   `RELAYOPS_ALERT_WEBHOOK_URL` if set (Slack and Teams compatible).
3. Pending and rejected definitions are part of every gateway's
   configuration, so **every node** refuses the tool, including nodes that
   never listed it.
4. A reviewer opens **Approvals → MCP tools & prompts held for review**. The
   approved definition and the new one are shown side by side. The reviewer
   then chooses one of:
   - **Approve**: pins the new fingerprint and publishes a new API revision.
   - **Keep blocked**: rejects it, and it stays refused.
   - **Resolved**: the server went back to the approved definition, so the
     hold is cleared.

API: `GET /api/mcp/catalog?status=pending&api_id=…` and
`POST /api/mcp/catalog/{id}/approve|reject|resolve`.

Approved entries also store the definition's `inputSchema`, which gateways
use for `validate_arguments`.

### Approving tools

In the console, **APIs → MCP tools** connects to the server. It lists every
tool with its status (approved, changed since approval, not approved, no
longer offered) and its full definition. Check the tools to expose and save.

The same flow through the API:

```sh
curl -X POST $ADMIN/api/apis/$ID/mcp/discover -H "Authorization: Bearer $TOKEN" -d '{}'
# → {"tools":[{"name":"search_issues","fingerprint":"sha256:…","status":"new","definition":{…}}], "removed":[]}
curl -X PUT $ADMIN/api/apis/$ID -H "Authorization: Bearer $TOKEN" \
  -d '{"mcp_policy":{"pinned_tools":{"search_issues":"sha256:…"}}}'
```

## What clients see

- `tools/list` contains only tools the caller may use.
- A refused `tools/call` is answered by the gateway with a JSON-RPC error and
  HTTP 200, which MCP clients pass to the model. The server never receives it.
  If a batch contains a refused call, the gateway refuses the whole batch.

| Code | `error.data.reason` |
|---|---|
| -32001 | `tool_not_allowed` (a rule) or `batch_refused` |
| -32002 | `tool_rate_limited` |
| -32003 | `tool_not_pinned`, `tool_definition_changed` |
| -32700 | malformed JSON-RPC (HTTP 400) |

- Authentication and API-level rate limits work as for any API (HTTP 401/429),
  with a JSON-RPC error body.
- `initialize`, notifications, `GET` streams and session `DELETE` pass
  through. JSON and `text/event-stream` responses are both inspected, event by
  event, so streaming is preserved.

## Limits of this version

- Only `tools/call` arguments are validated. Prompt arguments are not
  checked.
- An approval made by editing `pinned_tools` directly (API or APIOps)
  records the definition's schema only if the catalog already holds that
  definition, that is, if a gateway or Discover has seen it.
