# Quickstart: RelayOps in 5 minutes

Run RelayOps on your machine, publish an API, get a key from the developer portal, then ship a broken release and watch RelayOps roll it back on its own.

You need **Docker** (Docker Desktop on Windows or macOS) and **git**. Nothing else: RelayOps is built from source inside Docker.

## 1. Start RelayOps

```bash
git clone https://github.com/apatilgtn/RelayopsAPIM.git
cd RelayopsAPIM
docker compose up -d --build
```

The first build takes a minute or two. When it finishes:

| What | Where |
|---|---|
| Console (sign in with the administrator token `relayops-admin`) | http://localhost:9090 |
| Developer portal | http://localhost:9090/portal |
| API gateway | http://localhost:8080 |

Check it is up:

```bash
curl http://localhost:9090/healthz
# {"config_revision":1,"database":"healthy","gateway":"ready",...,"status":"ok"}
```

## 2. Publish an API

[`examples/quickstart/relayops.yaml`](../examples/quickstart/relayops.yaml) defines a **Demo API** at `/demo`, protected by an API key and served by a small mock backend. Apply it with `relayopsctl`, which ships inside the container:

```bash
docker compose exec -T gateway relayopsctl apply -f - --auto-approve --token relayops-admin < examples/quickstart/relayops.yaml
# + api "Demo API" (create)
# Declarative configuration applied atomically as revision 2.
```

Without a key, the gateway refuses the call:

```bash
curl -i http://localhost:8080/demo/hello
# HTTP/1.1 401 Unauthorized
# {"error":"missing_api_key","message":"provide an API key via the X-API-Key header or Bearer token",...}
```

## 3. Get an API key from the developer portal

1. Open http://localhost:9090/portal and choose **Get access**.
2. Enter an application name and your email, tick **Demo API**, and select **Register application**.
3. Copy the key it shows. It is shown **once**.

Call the API with it:

```bash
export KEY=<the key you copied>
curl -i -H "X-API-Key: $KEY" http://localhost:8080/demo/hello
# HTTP/1.1 200 OK
```

The mock backend echoes the request it received. RelayOps tells the backend who is calling (`X-Consumer-Name`) and never forwards the key itself. Try `/demo/status/503` or `/demo/delay/300` too.

## 4. Ship a broken release, and watch it roll back

[`examples/quickstart/bad-release.yaml`](../examples/quickstart/bad-release.yaml) is the same API pointed at a port nothing listens on, so every request it serves fails. Release it as a **canary**: only requests carrying `X-Canary: 1` (plus a small share of other callers) see the new version.

```bash
docker compose exec -T gateway relayopsctl apply -f - --canary --traffic-percent 10 \
  --canary-header X-Canary --canary-header-value 1 --auto-approve --token relayops-admin < examples/quickstart/bad-release.yaml
```

Now send traffic to both versions for half a minute:

```bash
for i in $(seq 1 30); do
  stable=$(curl -s -o /dev/null -w '%{http_code}' -H "X-API-Key: $KEY" http://localhost:8080/demo/hello)
  canary=$(curl -s -o /dev/null -w '%{http_code}' -H "X-API-Key: $KEY" -H "X-Canary: 1" http://localhost:8080/demo/hello)
  echo "stable=$stable canary=$canary"; sleep 1
done
```

What you will see:

```
stable=200 canary=502
stable=200 canary=502
...
stable=200 canary=200    <- the canary has been rolled back
```

Callers on the stable version never saw an error. Within seconds of the canary's error rate passing the threshold (5% over at least 5 requests, by default), RelayOps withdrew it. The revision history records who did it:

```bash
docker compose exec -T gateway relayopsctl revisions --token relayops-admin
# rev_4  active       ...  auto_rollback_supervisor  Rollback to revision 2
# rev_3  rolled_back  ...  admin@relayops.local      Declarative apply: ... (canary)
```

Open **Releases & fleet** in the console to see the same release, the rollback and the traffic on each version.

> **Why the header?** Canary traffic is assigned per caller, so one person testing from one machine always lands on the same side. The header makes the demo deterministic. In production you would normally rely on the percentage alone.

## Clean up

```bash
docker compose down -v
```

## Next steps

- **Promote a good release.** Promotion requires a passing release gate: a Test Studio suite run against the canary. See [`examples/gitops-quickstart`](../examples/gitops-quickstart/README.md).
- **Configuration as code.** `relayopsctl plan` shows what a change would do before you apply it; `relayopsctl export` writes your live configuration to YAML.
- **Plans for developers.** Only plans marked *Developers can choose this plan in the developer portal* are offered at self-registration (Console, **Plans**).
- **The full feature tour.** See the [README](../README.md).
