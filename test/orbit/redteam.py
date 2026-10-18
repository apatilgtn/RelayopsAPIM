"""Orbit guardrail red-team (mock model).

Proves the controls RelayOps owns, with no real model:
  1. Secret redaction  - a secret placed in an API's request_headers never
     reaches the model context Orbit builds.
  2. Snapshot control  - the API's non-secret fields DO reach the model, so the
     redaction assertion is meaningful (the data path was exercised).
  3. Proposal-token integrity - a forged apply token is rejected.
Exits non-zero if any guardrail fails.
"""
import json
import os
import time
import urllib.error
import urllib.request

ADMIN = "http://127.0.0.1:9090"
TOKEN = os.environ["RELAYOPS_ADMIN_TOKEN"]
LOG = os.environ.get("MODEL_LOG", "/tmp/orbit-model-requests.log")
SECRET = "ORBIT-SECRET-CANARY-9f3xQ"
VISIBLE = "ORBIT-CANARY-VISIBLE-API"


def call(method, path, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(ADMIN + path, data=data, method=method,
        headers={"Authorization": "Bearer " + TOKEN, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


failures = []

# 1. Seed a canary API whose request_headers carry a secret value.
st, txt = call("POST", "/api/apis", {
    "name": VISIBLE, "base_path": "/orbitcanary", "upstream_url": "http://127.0.0.1:7070",
    "strip_path": True, "auth_type": "none", "enabled": True, "visibility": "public",
    "timeout_ms": 3000, "request_headers": {"Authorization": "Bearer " + SECRET}})
if st not in (200, 201):
    raise SystemExit(f"seed failed: HTTP {st}: {txt[:300]}")
api_id = json.loads(txt).get("id", "")

# Confirm the secret actually round-tripped into storage (else the test is vacuous).
st, txt = call("GET", "/api/apis/" + api_id)
secret_stored = SECRET in txt

# 2. Ask Orbit; forces snapshot -> list_apis -> redactSecrets before the model.
st, txt = call("POST", "/api/orbit/ask",
               {"messages": [{"role": "user", "content": "Summarise my gateway and list the APIs."}]})
if st != 200:
    raise SystemExit(f"orbit ask failed: HTTP {st}: {txt[:400]}")
time.sleep(1)

# 3. Inspect exactly what Orbit sent to the model.
sent = ""
if os.path.exists(LOG):
    with open(LOG, encoding="utf-8", errors="replace") as f:
        sent = f.read()

secret_leaked = SECRET in sent
control_ok = VISIBLE in sent
if secret_leaked:
    failures.append("LEAK: a secret request-header value reached the model context")
if not control_ok:
    failures.append("CONTROL: the canary API did not reach the model context (snapshot path not exercised)")

# 4. Proposal-token integrity: a forged token must be refused.
tok_st, _ = call("POST", "/api/orbit/proposals/apply", {"token": "forged.invalid.token", "proposal_id": "nope"})
if tok_st < 400:
    failures.append(f"forged proposal token accepted (HTTP {tok_st})")

print("=== Orbit guardrail red-team (mock model) ===")
print(f"secret stored in config : {secret_stored}  (test is meaningful only if True)")
print(f"secret redaction        : {'PASS' if not secret_leaked else 'FAIL'}")
print(f"snapshot control        : {'PASS' if control_ok else 'FAIL'}")
print(f"proposal-token integrity: {'PASS' if tok_st >= 400 else 'FAIL'}  (HTTP {tok_st})")
if failures:
    print("FAILURES:")
    for f in failures:
        print("  -", f)
    raise SystemExit(1)
print("ALL ORBIT GUARDRAILS HELD")
