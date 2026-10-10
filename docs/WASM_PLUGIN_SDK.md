# WASM policy plugins: SDK guide

RelayOps gateways run WebAssembly policy plugins on the request path. A plugin
sees each request of the APIs that list it, after authentication, subscription
checks and rate limits, and before the upstream call (and before an AI route's
budget reservation and model call). It can **allow** the request, **deny** it
with a status of its choice, or **modify** the headers and body sent upstream.

Plugins are sandboxed by [wazero](https://wazero.io), a pure-Go runtime with no
cgo. A plugin has no network or file system access. It gets the request as
JSON and returns a decision as JSON.

## Plugins in this repository

| Plugin | What it does | Needs body |
|---|---|---|
| [`header-rewrite`](../plugins/wasm/header-rewrite/main.go) | Set, rename and remove upstream headers | no |
| [`ip-allowlist`](../plugins/wasm/ip-allowlist/main.go) | Admit or refuse by client address/CIDR (deny wins) | no |
| [`request-validator`](../plugins/wasm/request-validator/main.go) | Methods, required headers/query, content type, required JSON fields, size | for body checks |
| [`pii-redactor`](../plugins/wasm/pii-redactor/main.go) | Redact (or block) e-mail, card numbers (Luhn-checked), US SSN, phone and IPv4 in the body, for example before a prompt reaches a model provider | yes |
| [`hmac-auth`](../plugins/wasm/hmac-auth/main.go) | Verify HMAC-SHA256 request signatures (key id, timestamp, body hash), then strip them and pass `X-Authenticated-Key-Id` | yes |

Build them all into `plugins/wasm/dist/`:

```sh
plugins/wasm/build.sh          # or: pwsh plugins/wasm/build.ps1
```

## Using a plugin on an API

1. Put the `.wasm` files on every gateway, in the directory named by
   `RELAYOPS_WASM_PLUGINS_DIR` (bake them into the image or mount a volume).
   Each file registers under its name without `.wasm`. Plugins are code and
   ship with the gateway. They are never part of the API configuration.
2. Reference them from the API's `traffic_policy`:

```json
"traffic_policy": {
  "wasm_plugins": ["ip-allowlist", "pii-redactor"],
  "wasm_config": {
    "ip-allowlist": {"allow": ["10.0.0.0/8"]},
    "pii-redactor": {"detect": ["email", "credit_card"], "mode": "redact"}
  },
  "wasm_body_limit_bytes": 262144
}
```

| Field | Meaning |
|---|---|
| `wasm_plugins` | Up to 8 plugin names, run in order. The first deny stops the chain. |
| `wasm_config` | Per-plugin settings for this API: a JSON object of at most 16 KiB, passed to the plugin as `config`. Keys must be listed in `wasm_plugins`. |
| `wasm_body_limit_bytes` | 0 (default) streams the body and plugins see none. A value from 1 up to 1 MiB buffers the body as text for the plugins. A larger body is refused with 413. |

Plugin settings are versioned, reviewed and rolled out like the rest of the
API: they go through revisions, canaries, approvals and auto-rollback.

### Failure behaviour (fail closed)

| Situation | Response |
|---|---|
| A listed plugin is not loaded on this gateway | 503 `wasm_plugin_unavailable` |
| Plugin traps, exceeds 50 ms, or returns invalid JSON | 500 `wasm_plugin_failed` / `wasm_result_invalid` |
| Plugin returns a body but the API has `wasm_body_limit_bytes: 0` | 500 (a body is never replaced blind) |
| Plugin settings unusable (SDK `Misconfigured`) | 500 `plugin_misconfigured` |

Each run is recorded in the request's decision trail
(`policy_evaluations.wasm`): plugin, action, reason and time taken.

## Writing a plugin in Go

```go
package main

import plugin "github.com/relayops/apim/sdk/wasmplugin"

type config struct {
	Tenants []string `json:"tenants"`
}

func init() { plugin.Handle(handle) } // reactor modules run init, not main
func main() {}

func handle(r plugin.Request) plugin.Result {
	cfg, err := plugin.Config[config](r, nil) // decoded once per distinct config
	if err != nil {
		return plugin.Misconfigured(err)
	}
	tenant := r.Header("X-Tenant")
	for _, t := range cfg.Tenants {
		if t == tenant {
			return plugin.Modify().SetHeader("X-Tenant-Verified", "true")
		}
	}
	return plugin.Deny(403, "tenant_not_allowed", "unknown tenant")
}
```

Build it as a WASI reactor:

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o my-plugin.wasm ./my-plugin
```

### SDK reference (`sdk/wasmplugin`)

| | |
|---|---|
| `Request` | `Method`, `Path`, `ClientIP`, `Headers`, `Query`, `Body *string` (nil without body access), `Config` |
| `r.Header(name)`, `r.QueryParam(name)` | First value, case-insensitive header lookup |
| `Allow()` / `Deny(status, reason, detail)` / `Modify()` | Decisions. A deny status outside 400-599 becomes 403. |
| `.SetHeader(k, v)`, `.RemoveHeader(k)`, `.SetBody(s)` | Changes on a `Modify()` result. Removals apply before sets. |
| `Config[T](r, build)` | Decode settings into `T` and run `build` (validate, compile patterns). The result is cached per distinct settings document. |
| `Misconfigured(err)` | A fail-closed 500 for unusable settings |
| `Log(level, msg)` | Writes to the gateway log (1 debug … 4 error) |
| `NowMS()` | Gateway clock, Unix ms |
| `Dispatch(handler, json)` | Runs a handler through the JSON boundary, for native tests |

### Testing

Keep logic in plain Go: the ABI glue is only compiled for `wasip1`. So
`go test ./plugins/wasm/...` runs natively, as for any package. The plugins
here use `plugins/wasm/internal/plugintest`, which drives `Dispatch` with the
same JSON the gateway sends. `internal/gateway/wasm_kit_test.go` builds the
real `.wasm` files and runs them through a gateway.

### Responses

A plugin can also handle responses: register `plugin.HandleResponse` in
`init`. A response handler gets the status, headers, a summary of the
request, its settings and, if the API sets
`traffic_policy.wasm_response_body_limit_bytes`, the response body as text.
It returns `Allow()`, `Modify()` (with `.SetHeader`, `.RemoveHeader`,
`.SetBody` and `.SetStatus`), or `Deny(status, reason, detail)`, which
replaces the response with an error. A response plugin that fails refuses
the response with 502 (fail closed).

```go
func init() {
	plugin.Handle(handle)                 // optional
	plugin.HandleResponse(handleResponse) // optional
}

func handleResponse(r plugin.Response) plugin.Result {
	if r.Body == nil { // not granted, too large, or streamed
		return plugin.Modify().SetHeader("X-Scanned", "no")
	}
	return plugin.Modify().SetBody(strings.ReplaceAll(*r.Body, "internal.corp", "example.com"))
}
```

When response plugins read bodies, the gateway asks the upstream for an
uncompressed response, so the plugin always sees text. The examples
`pii-redactor` (`"scan": ["request", "response"]`) and `header-rewrite`
(`"response": {"set": …, "remove": […]}`) both handle responses. Because
response plugins run after the MCP gateway, `pii-redactor` can remove
personal data from MCP tool results before they reach the model.

### Runtime model and limits

- **Instances are reused.** The gateway keeps up to 64 idle instances per plugin
  and gives each to one request at a time. Package-level state survives
  between requests, so don't keep per-request data in globals. An instance
  that failed or timed out is discarded.
- **50 ms per plugin call.** A plugin running past it is interrupted and the
  request is refused.
- **Text bodies.** The body is passed as a JSON string. Invalid UTF-8
  sequences become U+FFFD, so binary bodies cannot be inspected or rewritten
  safely.
- **Response phase.** It runs after the upstream answers and after MCP
  tool-list filtering. Response bodies over the limit, or streamed bodies
  (server-sent events), are passed through, and the plugin is told the body
  is unavailable.

## ABI (other languages)

Any language that compiles to a WASI (preview 1) reactor can implement a
plugin. The module exports:

| Export | Signature | |
|---|---|---|
| `allocate` (or `malloc`) | `(size i32) -> ptr i32` | Buffer the host writes the input JSON into |
| `execute_policy` | `(ptr i32, len i32) -> i64` | Returns `(result_ptr << 32) \| result_len` of the result JSON in linear memory |
| `execute_response` | `(ptr i32, len i32) -> i64` | Optional: the response phase (input below) |
| `relayops_phases` | `() -> i32` | Optional bitmask: 1 = request, 2 = response. Without it, a module handles requests, plus responses if it exports `execute_response`. |
| `_initialize` | `()` | Optional; run once per instance before any call |

Host imports in module `env`: `relayops_log(level i32, ptr i32, len i32)` and
`relayops_now_ms() -> i64`.

Input JSON:

```json
{"method":"POST","path":"/orders","client_ip":"203.0.113.7",
 "headers":{"Content-Type":["application/json"]},"query_params":{"v":["2"]},
 "body":"{...}","config":{...}}
```

Response-phase input JSON:

```json
{"phase":"response","status":200,"headers":{"Content-Type":["application/json"]},
 "body":"{...}","body_truncated":false,
 "request":{"method":"GET","path":"/orders/1","client_ip":"203.0.113.7","headers":{}},
 "config":{...}}
```

Result JSON:

```json
{"action":"modify","headers":{"X-A":"1"},"remove_headers":["X-B"],"body":"..."}
{"action":"deny","status":403,"reason":"ip_not_allowed","detail":"..."}
{"action":"allow"}
```

A minimal module may export only `process_request() -> i32` instead. It
returns 0 or 200 to allow, or 400-599 to deny with that status.
