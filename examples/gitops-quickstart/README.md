# RelayOps GitOps Quickstart

This directory contains a complete, working example of managing RelayOps configuration as code.

## Files
* `relayops.yaml`: Declarative definition containing rate-limiting plans, a public weather proxy, a secured orders microservice, and an AI chat completion service.
* `test-suite.json`: Synthetic Test Studio contract verification suite used to gate releases.

## 2-Minute Walkthrough

### 1. Validate Configuration
Validate the document syntax and schema against your control plane:
```bash
relayopsctl validate -f relayops.yaml
```

### 2. Preview the Plan (Impact & Replay Analysis)
See what changes will be applied, including field diffs and traffic impact:
```bash
relayopsctl plan -f relayops.yaml
```

### 3. Apply as an Atomic Revision
Deploy the configuration fleet-wide:
```bash
relayopsctl apply -f relayops.yaml --auto-approve
```

Or deploy as a safe 10% canary:
```bash
relayopsctl apply -f relayops.yaml --canary --traffic-percent 10 --auto-approve
```

### 4. Import & Run Synthetic Test Gate
Validate the deployment with the Test Studio assertion gate:
```bash
# Import the test suite
relayopsctl test import -f test-suite.json --api "<API_ID>"

# Execute test suite against live endpoints
relayopsctl test run "<SUITE_ID>" --wait
```

### 5. Promote or Abort the Canary
Inspect fleet convergence and promote when green:
```bash
# Check rollout convergence and cohort error rates
relayopsctl rollout status

# Promote canary to 100% production
relayopsctl rollout promote <REVISION_ID>

# Or instantly abort and restore stable revision
relayopsctl rollout abort <REVISION_ID>
```
