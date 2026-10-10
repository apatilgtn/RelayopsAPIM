# gRPC through the gateway

RelayOps proxies all four gRPC call types (unary, server streaming, client
streaming and bidirectional streaming) with the same authentication, plans,
rate limits, revisions and canaries as HTTP APIs. Messages stream through as
they arrive: the gateway never buffers a stream.

## Setup

Clients connect over HTTP/2: over TLS, or over cleartext when
`RELAYOPS_H2C_ENABLED=true` is set on the proxy listener.

```json
{
  "name": "orders-grpc",
  "base_path": "/orders.v1.OrderService",
  "strip_path": false,
  "upstream_url": "h2c://orders.internal:50051",
  "protocol": "grpc",
  "auth_type": "api_key",
  "grpc_descriptor_set": "<base64 FileDescriptorSet>",
  "grpc_policy": {
    "max_message_size_bytes": 1048576,
    "allow_reflection": false,
    "default_action": "allow",
    "rules": [
      {"methods": ["orders.v1.OrderService/Delete*"], "plans": ["Enterprise"], "action": "allow"},
      {"methods": ["orders.v1.OrderService/Delete*"], "action": "deny"}
    ]
  }
}
```

gRPC paths are `/package.Service/Method`. Use the service name as the base
path and set `strip_path: false`, so the upstream receives the full method
path.

| Upstream URL scheme | Connection to the service |
|---|---|
| `h2c://` or `grpc://` | Cleartext HTTP/2 (prior knowledge), as plaintext gRPC servers expect |
| `http://` on a `grpc` API | Same as `h2c://` |
| `https://` or `grpcs://` | HTTP/2 over TLS |

## Policies

| Setting | Behaviour |
|---|---|
| `max_message_size_bytes` (default 4 MiB, max 64 MiB) | Checked on **every message in both directions**, including on streams, as frames pass through. An oversized request message never reaches the service. An oversized message mid-stream ends the call with `RESOURCE_EXHAUSTED` trailers. Messages sent before it are still delivered. |
| `grpc_descriptor_set` | A `FileDescriptorSet` from `protoc --include_imports --descriptor_set_out=…`. Methods not in it are refused with `UNIMPLEMENTED` at the gateway. The decision trail records the call type (unary or streaming). |
| `rules` / `default_action` | Allow or deny methods (`package.Service/Method`, globs such as `package.Service/*`) per consumer or plan. Refusals return `PERMISSION_DENIED`. |
| `allow_reflection` | Server reflection (`grpc.reflection.*`) is refused unless enabled |

Gateway refusals are proper gRPC statuses (`UNAUTHENTICATED`,
`PERMISSION_DENIED`, `RESOURCE_EXHAUSTED`, `UNAVAILABLE`, …), and
`grpc-message` is percent-encoded as the gRPC spec requires. When the service
is unreachable, the status is `UNAVAILABLE`, so clients can retry.

## Browsers: gRPC-Web

Browser clients (grpc-web, Connect and similar libraries) can call any gRPC
API over plain HTTP/1.1, in binary (`application/grpc-web`) or base64 text
(`application/grpc-web-text`). No setting is needed. The gateway translates
each call to native gRPC towards the service and back. It moves the
service's trailers into the final body frame that gRPC-Web clients read,
and streams text responses as base64. Server streaming works. Gateway
refusals, message limits and upstream failures reach the browser as
ordinary gRPC-Web statuses. With `cors_enabled` on the API, the
`Grpc-Status` and `Grpc-Message` headers are exposed to cross-origin pages.

## HTTP clients: JSON transcoding

With `grpc_policy.json_transcoding` and a descriptor set, an HTTP client can
call a unary method with JSON:

```sh
curl -X POST https://gateway/orders.v1.OrderService/GetOrder   -H 'Content-Type: application/json' -d '{"id": "o-1"}'
# -> 200 {"id": "o-1", "status": "SHIPPED"}
```

The gateway converts the JSON to the method's request message (proto3 JSON
mapping; unknown fields are a 400), calls the service over gRPC, and returns
the response message as JSON. gRPC errors become HTTP statuses (for example
`NOT_FOUND` → 404, `UNAVAILABLE` → 503), with
`{"code", "status", "message"}` in the body. Streaming methods are not
transcoded (501). REST-style paths from `google.api.http` annotations are
not supported yet: the path is the gRPC method path.

## Observability and release safety

A failed call still has HTTP status 200: the outcome is the `grpc-status`
trailer. The gateway reads that trailer and logs the equivalent HTTP status
(`UNAVAILABLE` → 503, `INTERNAL` → 500, `DEADLINE_EXCEEDED` → 504,
`PERMISSION_DENIED` → 403, …). So error rates, canary comparison and
automatic rollback treat a failing gRPC service like a failing HTTP one. The
decision trail keeps the original code (`policy_evaluations.grpc.status`), and
the request's route is the full method name.

gRPC calls are not retried by the gateway: a stream cannot be replayed, and
gRPC clients have their own retry policies.

## Tested

`internal/gateway/grpc_e2e_test.go` runs a real grpc-go client and server
through the gateway. It covers all four call types, upstream error statuses
(100 calls in a row), message limits in both directions, descriptor
enforcement, method rules and status logging.
