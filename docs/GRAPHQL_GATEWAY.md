# GraphQL policy

For APIs with `protocol: "graphql"`, the gateway parses every request into a
GraphQL syntax tree with [gqlparser](https://github.com/vektah/gqlparser).
Policy is applied to the operation that will actually run, before anything
reaches the upstream.

```json
{
  "protocol": "graphql",
  "graphql_schema": "type Query { user(id: ID!): User ... }",
  "graphql_policy": {
    "validate_against_schema": true,
    "max_depth": 8,
    "max_cost": 2000,
    "max_aliases": 20,
    "max_batch_size": 5,
    "allow_mutations": true,
    "allow_introspection": false,
    "list_size_arguments": ["first", "last", "limit", "pageSize"],
    "operation_allowlist": ["sha256:9f8e…", "GetUser"]
  }
}
```

| Setting | How it is measured |
|---|---|
| `validate_against_schema` | Runs the specification's validation rules against `graphql_schema`: fields exist, required arguments are present, arguments and variables have the right types, and fragments are valid and used. Invalid requests are refused with the validation messages. The schema is checked when the API is saved. |
| `max_depth` | The deepest field nesting of the selected operation, following fragment spreads. Braces inside argument values do not count. |
| `max_cost` | Each field costs 1 plus its children's cost. A field with a list-size argument (`first: 100`, or a variable holding 100) multiplies its children's cost by that size, so `users(first: 100) { friends(first: 10) { name } }` costs 1 + 100 × (1 + 10). |
| `max_aliases` | Fields renamed with an alias, across the operation and the fragments it uses. This limits field-duplication attacks. |
| `max_batch_size` | Operations allowed in a JSON-array batch. 0 (the default) refuses batches. Every operation in a batch is checked. |
| `allow_mutations` / `allow_introspection` | Based on the operation's real type, and on `__schema` / `__type` fields in the tree (`__typename` is allowed). |
| `operation_allowlist` | `sha256:<hex>` entries pin an exact document: the SHA-256 of the query text, the persisted-query id clients already use. Plain names match the operation's name. A name is chosen by the client, so only hashes are enforceable. |

Also enforced:
- Mutations and subscriptions are refused over `GET` (HTTP 405), which
  closes off cross-site request forgery.
- With several operations in one document, `operationName` must pick one.
- Fragment cycles are refused.
- Fragment fan-out is measured in linear time. Each fragment is measured
  once, so a document that would expand to 2³⁰ fields is analysed instantly,
  and refused by `max_cost`.

Refusals are GraphQL error responses (`{"errors": [...]}`) with HTTP 400. The
decision trail (`policy_evaluations.graphql`) records the operation name,
type, depth, cost, alias count, query hash and any validation errors. A
named operation also becomes the request's route, so analytics break down
per operation.

## Changes from the previous analyzer

The earlier version scanned the raw text with regular expressions and brace
counting. These bypasses are fixed:

- A mutation after a fragment definition was read as a query, so it passed
  `allow_mutations: false`.
- Nesting hidden in fragments did not count towards `max_depth`.
- Object literals in arguments counted as depth, and a string containing
  `__schema` counted as introspection.
- The allowlist trusted the client-supplied operation name.

`max_cost` now uses the field-and-list-size model above instead of counting
tokens. Review existing limits after upgrading.

## Persisted queries (APQ)

Clients using automatic persisted queries send
`extensions.persistedQuery.sha256Hash`. Policy is never skipped for them:

1. **Query and hash** together: the gateway checks that the hash matches
   the query text. It then applies every check above, forwards the request,
   and caches the query for this API. Only queries that pass are cached.
2. **Hash only**: the gateway looks the query up in its cache and checks it
   again before forwarding. If the hash is unknown, for example on a node that
   has not seen it, the gateway answers with the standard
   `PersistedQueryNotFound` error (HTTP 200), and APQ clients resend the full
   query.
3. A hash that doesn't match the query is refused with 400.

A `sha256:` allowlist entry uses the same hash, so a deployment can allow
only its persisted operations.

## Subscriptions over WebSocket

WebSocket upgrades on GraphQL APIs are proxied for both
`graphql-transport-ws` and the older `subscriptions-transport-ws`
(`graphql-ws`). The gateway reads the client's messages. Every operation
started with `subscribe` (or `start`) goes through the same parsing,
schema validation and policy as an HTTP request. A refused operation never
reaches the server: the client gets an `error` message for that operation
id, and the connection stays open for others. WebSocket compression is not
offered on these connections, so the messages can be read. Messages larger
than 1 MiB close the connection.
