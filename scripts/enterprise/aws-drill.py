#!/usr/bin/env python3
"""Opt-in failure drills. Dry-run by default. Only exact RelayOpsLab-tagged resources.
Credentials are read from environment, never persisted in reports or commands.
"""
import argparse
import concurrent.futures as futures
import datetime as dt
import ipaddress
import json
import os
import re
import statistics
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path

SCENARIOS = ["database-interruption", "redis-loss", "control-plane-isolation",
             "gateway-replacement", "rolling-upgrade", "complete-recovery", "database-provider-failover"]

def aws(*args):
    r = subprocess.run(["aws", *args, "--output", "json"], capture_output=True, text=True)
    if r.returncode:
        raise RuntimeError("AWS operation failed: " + " ".join(args[:2]))
    return json.loads(r.stdout or "{}")

def require_lab(resource, kind, lab):
    if kind == "instance":
        data = aws("ec2", "describe-instances", "--instance-ids", resource)
        tags = data["Reservations"][0]["Instances"][0].get("Tags", [])
    elif kind == "volume":
        tags = aws("ec2", "describe-volumes", "--volume-ids", resource)["Volumes"][0].get("Tags", [])
    else:
        tags = aws("elbv2", "describe-tags", "--resource-arns", resource)["TagDescriptions"][0].get("Tags", [])
    if not any(t["Key"] == "RelayOpsLab" and t["Value"] == lab for t in tags):
        raise ValueError("refusing resource without the exact RelayOpsLab tag")

def remote(node, command):
    x = aws("ssm", "send-command", "--instance-ids", node, "--document-name", "AWS-RunShellScript",
            "--timeout-seconds", "600", "--parameters", json.dumps({"commands": ["set -eu; " + command]}))
    cid = x["Command"]["CommandId"]
    for _ in range(330):
        time.sleep(2)
        x = aws("ssm", "get-command-invocation", "--command-id", cid, "--instance-id", node)
        if x["Status"] == "Success":
            return x.get("StandardOutputContent", "")
        if x["Status"] in ["Failed", "Cancelled", "TimedOut"]:
            raise RuntimeError("lab command failed; inspect authorized SSM logs (output withheld)")
    raise RuntimeError("SSM command timeout")

def block(node, endpoints, seconds):
    rules = []
    for endpoint in endpoints:
        ip = str(ipaddress.ip_address(endpoint["ip"]))
        port = int(endpoint["port"])
        if not 1 <= port <= 65535:
            raise ValueError("invalid endpoint port")
        rules.append(f"iptables -I OUTPUT -p tcp -d {ip} --dport {port} -j REJECT")
    undo = "; ".join(r.replace("-I OUTPUT", "-D OUTPUT") + " || true" for r in rules)
    # Out-of-process undo also runs if SSM or the connection dies.
    timer = f"systemd-run --unit=relayops-undo-{time.time_ns()} --on-active={seconds+10}s /bin/sh -c '{undo}'"
    remote(node, timer + "; trap '" + undo + "' EXIT; " + "; ".join(rules) + f"; sleep {seconds}")

def perform(m, scenario):
    if scenario in SCENARIOS[:3]:
        key = dict(zip(SCENARIOS[:3], ["database_endpoints", "redis_endpoints", "control_endpoints"]))[scenario]
        nodes = m["control_planes"] if scenario == "database-interruption" else m["gateways"]
        with futures.ThreadPoolExecutor(max_workers=4) as pool:
            list(pool.map(lambda n: block(n, m[key], m["fault_seconds"]), nodes))
        return {"fault": "connection interruption; not provider failover"}
    if scenario == "rolling-upgrade":
        image = m["upgrade_image"]
        if not re.fullmatch(r"[a-zA-Z0-9./:_-]+@sha256:[a-f0-9]{64}", image):
            raise ValueError("a signature-verified image digest is required")
        for node in m["gateways"]:
            remote(node, f"cd /opt/relayops; RELAYOPS_IMAGE={image} docker compose -f compose.production.yaml --profile gateway run --rm gateway preflight; RELAYOPS_IMAGE={image} docker compose -f compose.production.yaml --profile gateway up -d --no-deps gateway")
            time.sleep(10)
        return {"upgrade_image": image}
    if scenario == "gateway-replacement":
        old, new = m["gateways"][0], m["replacement_instance"]
        volume, group = m["recovery_volume"], m["gateway_target_group"]
        # Fence old host before attaching the retained single-AZ volume to its spare.
        aws("ec2", "stop-instances", "--instance-ids", old)
        aws("ec2", "wait", "instance-stopped", "--instance-ids", old)
        aws("ec2", "detach-volume", "--volume-id", volume, "--instance-id", old)
        aws("ec2", "wait", "volume-available", "--volume-ids", volume)
        aws("ec2", "attach-volume", "--volume-id", volume, "--instance-id", new, "--device", "/dev/sdf")
        aws("ec2", "wait", "volume-in-use", "--volume-ids", volume)
        remote(new, "mountpoint -q /var/lib/relayops || mount /dev/disk/by-label/relayops-data /var/lib/relayops; /opt/relayops/restore-node-secrets; cd /opt/relayops; docker compose -f compose.production.yaml --profile gateway run --rm gateway preflight; docker compose -f compose.production.yaml --profile gateway up -d gateway")
        aws("elbv2", "deregister-targets", "--target-group-arn", group, "--targets", json.dumps([{"Id": old}]))
        aws("elbv2", "register-targets", "--target-group-arn", group, "--targets", json.dumps([{"Id": new}]))
        return {"fenced_instance": old, "replacement_instance": new, "manual_cleanup_required": True}
    if scenario == "complete-recovery":
        # Authorized provider owners install this signed runbook on a FRESH lab host.
        proof = json.loads(remote(m["restore_instance"], "/opt/relayops/recover-lab --json"))
        required = ["fresh_database", "secrets_restored", "authorized_upstream_success"]
        if not all(proof.get(k) is True for k in required):
            raise ValueError("database-only restore is incomplete; secrets and upstream access required")
        return {k: proof[k] for k in required} | {"rto_seconds": proof.get("rto_seconds"), "rpo_seconds": proof.get("rpo_seconds")}
    raise NotImplementedError("provider-specific failover requires the existing provider's authorized procedure; a TCP block does not prove it")

def probe(url, key):
    start = time.monotonic()
    try:
        req = urllib.request.Request(url, headers={"X-API-Key": key} if key else {})
        with urllib.request.urlopen(req, timeout=5) as r:
            r.read(1048576)
            status, request_id = r.status, r.headers.get("X-Request-ID")
    except urllib.error.HTTPError as e:
        status, request_id = e.code, e.headers.get("X-Request-ID")
    except (OSError, TimeoutError):
        status, request_id = 0, None
    return {"elapsed_ms": round((time.monotonic()-start)*1000, 3), "status": status, "request_id": request_id,
            "at": dt.datetime.now(dt.timezone.utc).isoformat()}

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("manifest", type=Path); p.add_argument("scenario", choices=SCENARIOS)
    p.add_argument("--execute", action="store_true"); p.add_argument("--ack-isolated-lab", action="store_true")
    p.add_argument("--output", type=Path, default=Path("drill-result.json"))
    a = p.parse_args(); m = json.loads(a.manifest.read_text())
    result = {"schema_version": 1, "scenario": a.scenario, "environment": m.get("lab_id"), "status": "not_executed",
              "measured": False, "acceptance": m.get("acceptance"), "workload": m.get("workload"),
              "rto_seconds": None, "rpo_seconds": None, "log_loss": None, "convergence_seconds": None}
    if not a.execute:
        a.output.write_text(json.dumps(result, indent=2)); print("Dry run: no AWS operations issued"); return
    if not a.ack_isolated_lab or not m.get("isolated_lab") or not m.get("lab_id"):
        p.error("explicit isolated-lab acknowledgement and manifest required")
    if not 1 <= m["fault_seconds"] <= 120 or not 15 <= m["duration_seconds"] <= 600 or not m.get("acceptance"):
        p.error("record acceptance criteria; fault must be 1..120s and observation 15..600s")
    for node in set(m["gateways"] + m["control_planes"] + [m[k] for k in ["replacement_instance", "restore_instance"] if m.get(k)]):
        require_lab(node, "instance", m["lab_id"])
    if a.scenario == "gateway-replacement":
        require_lab(m["recovery_volume"], "volume", m["lab_id"]); require_lab(m["gateway_target_group"], "target", m["lab_id"])
    if not m["probe_url"].startswith("https://"):
        p.error("use an HTTPS probe")
    samples = []; key = os.environ.get("RELAYOPS_DRILL_API_KEY", "")
    with futures.ThreadPoolExecutor(max_workers=1) as pool:
        samples.append(probe(m["probe_url"], key))
        task = pool.submit(perform, m, a.scenario); deadline = time.monotonic() + m["duration_seconds"]
        while time.monotonic() < deadline:
            samples.append(probe(m["probe_url"], key)); time.sleep(m.get("interval_seconds", .25))
        try:
            result["fault_result"] = task.result(); result["status"] = "measured_pending_review"
        except NotImplementedError as e:
            result["status"] = "unsupported"; result["reason"] = str(e)
        except Exception as e:
            result["status"] = "failed"; result["reason"] = str(e)
    values = sorted(x["elapsed_ms"] for x in samples)
    result.update(measured=True, requests=len(samples), errors=sum(x["status"] < 200 or x["status"] >= 300 for x in samples),
                  latency_ms={"mean": statistics.mean(values), "p50": values[int((len(values)-1)*.5)],
                              "p95": values[int((len(values)-1)*.95)], "p99": values[int((len(values)-1)*.99)]}, samples=samples)
    # No blanket pass until convergence, log loss and provider recovery are measured.
    a.output.write_text(json.dumps(result, indent=2)); print(result["status"])

if __name__ == "__main__":
    main()
