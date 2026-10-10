// Command relayopsctl manages RelayOps configuration as code: export the live
// configuration, validate and plan a declarative document (with consumer impact
// and traffic replay), apply it fleet-wide or as a canary, and drive rollouts.
//
//	relayopsctl export -o relayops.yaml
//	relayopsctl plan  -f relayops.yaml [--prune] [--output text|json|markdown] [--detailed-exitcode]
//	relayopsctl apply -f relayops.yaml [--prune] [--plan-hash H] [--canary --traffic-percent 10 --canary-header X-Canary] --auto-approve
//	relayopsctl rollout status | promote <rev> | abort <rev> | canary <rev> [--traffic-percent N] [--canary-header H]
//	relayopsctl revisions | rollback <rev>
//
// Connection: --server / RELAYOPS_URL (default http://localhost:9090) and
// --token / RELAYOPS_TOKEN (admin bearer token or session token).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/relayops/apim/internal/apiops"
	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/testingstudio"
	"gopkg.in/yaml.v3"
)

const usage = `relayopsctl - RelayOps configuration as code

Usage:
  relayopsctl export     [-o FILE]                       Export live config (YAML if FILE ends in .yaml/.yml)
  relayopsctl validate   -f FILE                         Validate a document against the live control plane
  relayopsctl plan       -f FILE [flags]                 Show what apply would change, with consumer impact
  relayopsctl apply      -f FILE [flags]                 Apply a document as one atomic revision
  relayopsctl bundle     build [--dir DIR] [--env ENV] [-o FILE]   Compile repository into a canonical bundle
  relayopsctl bundle     validate -f FILE                          Validate a compiled bundle and its hashes
  relayopsctl rollout    status                          Show the stable/canary revisions and fleet convergence
  relayopsctl rollout    canary  REV [--traffic-percent N] [--canary-header H [--canary-header-value V]]
  relayopsctl rollout    promote REV                     Promote a canary to the whole fleet
  relayopsctl rollout    abort   REV                     Withdraw a canary and restore the stable revision
  relayopsctl revisions                                  List recent revisions
  relayopsctl rollback   REV                             Restore an earlier revision as a new revision
  relayopsctl maintenance purge [flags]                  Purge stale logs, test records and sessions
  relayopsctl test       validate -f FILE                Validate a Studio suite JSON/YAML definition
  relayopsctl test       import -f FILE --api API_ID     Import a Studio suite JSON
  relayopsctl test       run SUITE_ID [--env ENV] [--target-revision REV] [--wait] [--format text|json|junit]
  relayopsctl mcp        [--server URL] [--api-key KEY] [--token TOKEN]   Run Model Context Protocol (MCP) server over stdio

Common flags:
  --server URL      control plane URL (env RELAYOPS_URL, default http://localhost:9090)
  --token TOKEN     admin token (env RELAYOPS_TOKEN)
  --api-key KEY     developer API key for MCP catalog discovery & invocations (env RELAYOPS_API_KEY)
  --tenant SLUG     tenant to act in (env RELAYOPS_TENANT); export, plan and apply are per tenant

plan/apply flags:
  --prune                 delete APIs and plans the document does not declare
  --output FORMAT         text (default), json or markdown
  --detailed-exitcode     plan: exit 0 = no changes, 2 = changes, 1 = error
  --no-impact             skip consumer impact and traffic replay
  --save-plan FILE        plan: also write the raw plan JSON (plan_hash, summary) to FILE
  --plan-hash HASH        apply: refuse unless the reviewed plan is still current
  --canary                apply: publish as an in-flight canary instead of fleet-wide
  --traffic-percent N     apply/canary: share of traffic for the canary on every node
  --canary-header NAME    apply/canary: route requests carrying this header to the canary
  --auto-approve          apply: do not ask for confirmation
`

func main() {
	code, err := run(os.Args[1:], os.Stdout, os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

type options struct {
	server, token, tenant, file, out, output, planHash, canaryHeader, canaryHeaderValue, savePlan, envID, apiID, apiKey string
	dir, format, olderThan                                                                                              string
	prune, detailed, noImpact, canary, autoApprove, wait, vacuum                                                        bool
	trafficPercent                                                                                                      int
	targetRevision                                                                                                      int64
	args                                                                                                                []string
}

func parse(name string, args []string) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.server, "server", envOr("RELAYOPS_URL", "http://localhost:9090"), "")
	fs.StringVar(&o.token, "token", os.Getenv("RELAYOPS_TOKEN"), "")
	fs.StringVar(&o.apiKey, "api-key", envOr("RELAYOPS_API_KEY", ""), "")
	fs.StringVar(&o.tenant, "tenant", os.Getenv("RELAYOPS_TENANT"), "")
	fs.StringVar(&o.file, "f", "", "")
	fs.StringVar(&o.file, "file", "", "")
	fs.StringVar(&o.out, "o", "", "")
	fs.StringVar(&o.output, "output", "text", "")
	fs.StringVar(&o.format, "format", "text", "")
	fs.StringVar(&o.dir, "dir", ".", "")
	fs.StringVar(&o.planHash, "plan-hash", "", "")
	fs.StringVar(&o.savePlan, "save-plan", "", "")
	fs.StringVar(&o.canaryHeader, "canary-header", "", "")
	fs.StringVar(&o.canaryHeaderValue, "canary-header-value", "", "")
	fs.StringVar(&o.envID, "env", "", "")
	fs.StringVar(&o.apiID, "api", "", "")
	fs.StringVar(&o.olderThan, "older-than", "24h", "")
	fs.Int64Var(&o.targetRevision, "target-revision", 0, "")
	fs.BoolVar(&o.wait, "wait", true, "")
	fs.BoolVar(&o.prune, "prune", false, "")
	fs.BoolVar(&o.detailed, "detailed-exitcode", false, "")
	fs.BoolVar(&o.noImpact, "no-impact", false, "")
	fs.BoolVar(&o.canary, "canary", false, "")
	fs.BoolVar(&o.autoApprove, "auto-approve", false, "")
	fs.BoolVar(&o.vacuum, "vacuum", false, "")
	fs.IntVar(&o.trafficPercent, "traffic-percent", 0, "")
	// Allow flags after positional arguments (e.g. "canary 12 --traffic-percent 5").
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	o.args = positional
	switch o.output {
	case "text", "json", "markdown":
	default:
		return nil, fmt.Errorf("--output must be text, json or markdown")
	}
	switch o.format {
	case "text", "json", "junit":
	default:
		return nil, fmt.Errorf("--format must be text, json or junit")
	}
	return o, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(args []string, stdout io.Writer, stdin io.Reader) (int, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0, nil
	}
	cmd := args[0]
	o, err := parse(cmd, args[1:])
	if err != nil {
		return 1, err
	}
	c := &client{base: strings.TrimRight(o.server, "/"), token: o.token, tenant: o.tenant, http: &http.Client{Timeout: 120 * time.Second}}
	switch cmd {
	case "export":
		return cmdExport(c, o, stdout)
	case "validate":
		o.noImpact = true
		return cmdPlan(c, o, stdout, true)
	case "plan":
		return cmdPlan(c, o, stdout, false)
	case "apply":
		return cmdApply(c, o, stdout, stdin)
	case "rollout":
		return cmdRollout(c, o, stdout)
	case "revisions":
		return cmdRevisions(c, o, stdout)
	case "rollback":
		if len(o.args) != 1 {
			return 1, errors.New("usage: relayopsctl rollback REV")
		}
		return printResult(c.do("POST", "/api/revisions/"+o.args[0]+"/rollback", nil), o, stdout)
	case "maintenance":
		return cmdMaintenance(c, o, stdout)
	case "test":
		return cmdTest(c, o, stdout)
	case "bundle":
		return cmdBundle(o, stdout)
	case "mcp":
		return cmdMCP(c, o, stdout, stdin)
	default:
		return 1, fmt.Errorf("unknown command %q (run relayopsctl help)", cmd)
	}
}

func cmdMCP(c *client, o *options, stdout io.Writer, stdin io.Reader) (int, error) {
	scanner := bufio.NewScanner(stdin)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	authHeader := ""
	if o.apiKey != "" {
		authHeader = "Bearer " + o.apiKey
	} else if o.token != "" {
		authHeader = "Bearer " + o.token
	}

	targetURL := c.base + "/mcp/message"
	fmt.Fprintf(os.Stderr, "[relayops-mcp] Bridge connected to %s\n", c.base)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		httpReq, err := http.NewRequest("POST", targetURL, strings.NewReader(line))
		if err != nil {
			errResp := fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"failed to create http request: %s"}}`+"\n", err.Error())
			stdout.Write([]byte(errResp))
			continue
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if authHeader != "" {
			httpReq.Header.Set("Authorization", authHeader)
		}
		if o.apiKey != "" {
			httpReq.Header.Set("X-API-Key", o.apiKey)
		}

		resp, err := c.http.Do(httpReq)
		if err != nil {
			errResp := fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"cannot reach RelayOps control plane at %s: %s"}}`+"\n", targetURL, err.Error())
			stdout.Write([]byte(errResp))
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if len(body) > 0 {
			stdout.Write(body)
			stdout.Write([]byte("\n"))
		}
	}
	return 0, scanner.Err()
}

// ---------------------------------------------------------------------------
// HTTP client
// ---------------------------------------------------------------------------

type client struct {
	base, token, tenant string
	http                *http.Client
}

type response struct {
	status int
	body   []byte
	err    error
}

func (c *client) do(method, path string, body []byte) response {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return response{err: err}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.tenant != "" {
		req.Header.Set("X-RelayOps-Tenant", c.tenant)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "relayopsctl/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return response{err: fmt.Errorf("cannot reach control plane at %s: %w", c.base, err)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: b, err: err}
}

func (r response) apiError() error {
	if r.err != nil {
		return r.err
	}
	if r.status >= 200 && r.status < 300 {
		return nil
	}
	var e struct{ Error, Message string }
	if json.Unmarshal(r.body, &e) == nil && (e.Error != "" || e.Message != "") {
		return fmt.Errorf("%s (HTTP %d): %s", e.Error, r.status, e.Message)
	}
	return fmt.Errorf("HTTP %d: %s", r.status, strings.TrimSpace(string(r.body)))
}

func printResult(r response, o *options, w io.Writer) (int, error) {
	if err := r.apiError(); err != nil {
		return 1, err
	}
	if o.output == "json" {
		w.Write(r.body)
		return 0, nil
	}
	var m map[string]any
	if json.Unmarshal(r.body, &m) == nil {
		if msg, ok := m["message"].(string); ok {
			fmt.Fprintln(w, msg)
			return 0, nil
		}
	}
	w.Write(r.body)
	return 0, nil
}

// ---------------------------------------------------------------------------
// Documents (JSON or YAML)
// ---------------------------------------------------------------------------

// readDocument loads a JSON or YAML document and returns canonical JSON.
func readDocument(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("a document is required: -f FILE")
	}
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	return toJSON(raw, path)
}

func toJSON(raw []byte, path string) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return trimmed, nil
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: not valid JSON or YAML: %w", path, err)
	}
	doc, err := normalizeYAML(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return json.Marshal(doc)
}

// normalizeYAML converts YAML maps into JSON-compatible structures.
func normalizeYAML(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			n, err := normalizeYAML(val)
			if err != nil {
				return nil, err
			}
			x[k] = n
		}
		return x, nil
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("non-string key %v", k)
			}
			n, err := normalizeYAML(val)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []any:
		for i := range x {
			n, err := normalizeYAML(x[i])
			if err != nil {
				return nil, err
			}
			x[i] = n
		}
		return x, nil
	}
	return v, nil
}

func cmdExport(c *client, o *options, w io.Writer) (int, error) {
	r := c.do("GET", "/api/system/export", nil)
	if err := r.apiError(); err != nil {
		return 1, err
	}
	out := r.body
	if ext := strings.ToLower(filepath.Ext(o.out)); ext == ".yaml" || ext == ".yml" {
		var doc any
		if err := json.Unmarshal(r.body, &doc); err != nil {
			return 1, err
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(doc); err != nil {
			return 1, err
		}
		out = buf.Bytes()
	} else {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, r.body, "", "  "); err == nil {
			out = append(pretty.Bytes(), '\n')
		}
	}
	if o.out == "" || o.out == "-" {
		w.Write(out)
		return 0, nil
	}
	if err := os.WriteFile(o.out, out, 0o644); err != nil {
		return 1, err
	}
	fmt.Fprintf(w, "Exported live configuration to %s\n", o.out)
	return 0, nil
}

// ---------------------------------------------------------------------------
// plan / apply
// ---------------------------------------------------------------------------

type fieldChange struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

type planChange struct {
	Type     string        `json:"type"`
	Name     string        `json:"name"`
	Action   string        `json:"action"`
	Changes  []fieldChange `json:"changes"`
	Warnings []string      `json:"warnings"`
	Impact   *struct {
		BreakingChanges           []string `json:"breaking_changes"`
		TotalImpactedApplications int      `json:"total_impacted_applications"`
		ActionableSummary         string   `json:"actionable_summary"`
	} `json:"consumer_impact"`
	Replay *struct {
		Replayed      int `json:"replayed"`
		StatusChanged int `json:"status_changed"`
	} `json:"replay"`
}

type configPlan struct {
	Valid      bool     `json:"valid"`
	Errors     []string `json:"errors"`
	Warnings   []string `json:"warnings"`
	Prune      bool     `json:"prune"`
	HasChanges bool     `json:"has_changes"`
	Summary    struct {
		Create, Update, Delete, Unchanged int
	} `json:"summary"`
	Changes  []planChange `json:"changes"`
	PlanHash string       `json:"plan_hash"`
}

func planQuery(o *options) url.Values {
	q := url.Values{}
	if o.prune {
		q.Set("prune", "true")
	}
	if o.noImpact {
		q.Set("impact", "false")
	}
	return q
}

func fetchPlan(c *client, o *options, doc []byte) (configPlan, []byte, error) {
	r := c.do("POST", "/api/system/plan?"+planQuery(o).Encode(), doc)
	if r.err != nil {
		return configPlan{}, nil, r.err
	}
	var p configPlan
	if err := json.Unmarshal(r.body, &p); err != nil || (r.status >= 300 && len(p.Errors) == 0) {
		return configPlan{}, r.body, r.apiError()
	}
	return p, r.body, nil
}

func cmdPlan(c *client, o *options, w io.Writer, validateOnly bool) (int, error) {
	doc, err := readDocument(o.file)
	if err != nil {
		return 1, err
	}
	p, raw, err := fetchPlan(c, o, doc)
	if err != nil {
		return 1, err
	}
	if o.savePlan != "" {
		if err := os.WriteFile(o.savePlan, raw, 0o644); err != nil {
			return 1, err
		}
	}
	switch {
	case o.output == "json":
		w.Write(raw)
	case validateOnly && p.Valid:
		fmt.Fprintf(w, "%s is valid: %d to create, %d to update, %d to delete.\n", o.file, p.Summary.Create, p.Summary.Update, p.Summary.Delete)
	case o.output == "markdown":
		renderMarkdown(w, p)
	default:
		renderText(w, p)
	}
	if !p.Valid {
		return 1, errors.New("the document is invalid")
	}
	if o.detailed && p.HasChanges {
		return 2, nil
	}
	return 0, nil
}

func cmdApply(c *client, o *options, w io.Writer, stdin io.Reader) (int, error) {
	doc, err := readDocument(o.file)
	if err != nil {
		return 1, err
	}
	hash := o.planHash
	if hash == "" {
		p, _, err := fetchPlan(c, o, doc)
		if err != nil {
			return 1, err
		}
		if o.output != "json" {
			renderText(w, p)
		}
		if !p.Valid {
			return 1, errors.New("the document is invalid")
		}
		if !p.HasChanges {
			if o.output == "json" {
				fmt.Fprintln(w, `{"revision":0,"applied":false,"message":"No changes. Live configuration already matches the document."}`)
			}
			return 0, nil
		}
		if !o.autoApprove {
			fmt.Fprint(w, "\nApply these changes? Only 'yes' is accepted: ")
			line, _ := bufio.NewReader(stdin).ReadString('\n')
			if strings.TrimSpace(line) != "yes" {
				return 1, errors.New("apply cancelled")
			}
		}
		hash = p.PlanHash // apply exactly what was shown
	}
	q := planQuery(o)
	q.Set("plan_hash", hash)
	if o.canary {
		q.Set("rollout", "canary")
		q.Set("traffic_percent", strconv.Itoa(o.trafficPercent))
		if o.canaryHeader != "" {
			q.Set("canary_header", o.canaryHeader)
			q.Set("canary_header_value", o.canaryHeaderValue)
		}
	}
	r := c.do("POST", "/api/system/apply?"+q.Encode(), doc)
	if err := r.apiError(); err != nil {
		return 1, err
	}
	if o.output == "json" {
		w.Write(r.body)
		return 0, nil
	}
	var res struct {
		Revision int64  `json:"revision"`
		Applied  bool   `json:"applied"`
		Rollout  string `json:"rollout"`
		Message  string `json:"message"`
	}
	_ = json.Unmarshal(r.body, &res)
	fmt.Fprintln(w, res.Message)
	if res.Applied && res.Rollout == "canary" {
		fmt.Fprintf(w, "Canary rev_%d is live. Watch it with 'relayopsctl rollout status', then 'rollout promote %d' or 'rollout abort %d'.\n", res.Revision, res.Revision, res.Revision)
	}
	return 0, nil
}

var actionSymbol = map[string]string{"create": "+", "update": "~", "delete": "-", "unchanged": " "}

func short(v any) string {
	if v == nil {
		return "(unset)"
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func renderText(w io.Writer, p configPlan) {
	if !p.Valid {
		fmt.Fprintln(w, "The document is invalid:")
		for _, e := range p.Errors {
			fmt.Fprintf(w, "  - %s\n", e)
		}
		return
	}
	for _, ch := range p.Changes {
		if ch.Action == "unchanged" {
			continue
		}
		fmt.Fprintf(w, "%s %s %q (%s)\n", actionSymbol[ch.Action], ch.Type, ch.Name, ch.Action)
		for _, f := range ch.Changes {
			fmt.Fprintf(w, "    %s: %s -> %s\n", f.Field, short(f.Before), short(f.After))
		}
		if ch.Impact != nil {
			for _, b := range ch.Impact.BreakingChanges {
				fmt.Fprintf(w, "    ! breaking: %s\n", b)
			}
			if ch.Impact.TotalImpactedApplications > 0 {
				fmt.Fprintf(w, "    ! %d consumer application(s) affected\n", ch.Impact.TotalImpactedApplications)
			}
		}
		if ch.Replay != nil && ch.Replay.Replayed > 0 {
			fmt.Fprintf(w, "    replay: %d of %d recent requests would get a different status\n", ch.Replay.StatusChanged, ch.Replay.Replayed)
		}
		for _, warn := range ch.Warnings {
			fmt.Fprintf(w, "    ! %s\n", warn)
		}
	}
	fmt.Fprintf(w, "\nPlan: %d to create, %d to update, %d to delete, %d unchanged.\n", p.Summary.Create, p.Summary.Update, p.Summary.Delete, p.Summary.Unchanged)
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "Warning: %s\n", warn)
	}
	if p.HasChanges {
		fmt.Fprintf(w, "Plan hash: %s\n", p.PlanHash)
	} else {
		fmt.Fprintln(w, "No changes. The live configuration matches the document.")
	}
}

func renderMarkdown(w io.Writer, p configPlan) {
	fmt.Fprintln(w, "### RelayOps configuration plan")
	fmt.Fprintln(w)
	if !p.Valid {
		fmt.Fprintln(w, "**The document is invalid.**")
		fmt.Fprintln(w)
		for _, e := range p.Errors {
			fmt.Fprintf(w, "- %s\n", e)
		}
		return
	}
	fmt.Fprintf(w, "**%d to create, %d to update, %d to delete**, %d unchanged.\n\n", p.Summary.Create, p.Summary.Update, p.Summary.Delete, p.Summary.Unchanged)
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "> ⚠️ %s\n\n", warn)
	}
	if !p.HasChanges {
		fmt.Fprintln(w, "No changes. The live configuration matches the document.")
		return
	}
	fmt.Fprintln(w, "| Action | Resource | Details |")
	fmt.Fprintln(w, "|---|---|---|")
	for _, ch := range p.Changes {
		if ch.Action == "unchanged" {
			continue
		}
		var details []string
		fields := make([]string, 0, len(ch.Changes))
		for _, f := range ch.Changes {
			fields = append(fields, "`"+f.Field+"`")
		}
		sort.Strings(fields)
		if len(fields) > 0 {
			details = append(details, "changes "+strings.Join(fields, ", "))
		}
		if ch.Impact != nil {
			for _, b := range ch.Impact.BreakingChanges {
				details = append(details, "⚠️ "+b)
			}
		}
		if ch.Replay != nil && ch.Replay.StatusChanged > 0 {
			details = append(details, fmt.Sprintf("⚠️ replay: %d of %d recent requests change status", ch.Replay.StatusChanged, ch.Replay.Replayed))
		}
		for _, warn := range ch.Warnings {
			if !strings.Contains(warn, "recent requests") {
				details = append(details, "⚠️ "+warn)
			}
		}
		fmt.Fprintf(w, "| %s | %s `%s` | %s |\n", ch.Action, ch.Type, ch.Name, strings.ReplaceAll(strings.Join(details, "<br>"), "|", "\\|"))
	}
	fmt.Fprintf(w, "\nPlan hash: `%s`\n", p.PlanHash)
}

// ---------------------------------------------------------------------------
// rollout / revisions
// ---------------------------------------------------------------------------

func cmdRollout(c *client, o *options, w io.Writer) (int, error) {
	if len(o.args) == 0 {
		return 1, errors.New("usage: relayopsctl rollout status|canary|promote|abort [REV]")
	}
	sub := o.args[0]
	rev := ""
	if len(o.args) > 1 {
		rev = o.args[1]
		if _, err := strconv.ParseInt(rev, 10, 64); err != nil {
			return 1, fmt.Errorf("revision must be a number, got %q", rev)
		}
	}
	need := func() error {
		if rev == "" {
			return fmt.Errorf("usage: relayopsctl rollout %s REV", sub)
		}
		return nil
	}
	switch sub {
	case "status":
		return rolloutStatus(c, o, w)
	case "canary":
		if err := need(); err != nil {
			return 1, err
		}
		body, _ := json.Marshal(map[string]any{"traffic_percent": o.trafficPercent, "header": o.canaryHeader, "header_value": o.canaryHeaderValue})
		return printResult(c.do("POST", "/api/revisions/"+rev+"/canary", body), o, w)
	case "promote":
		if err := need(); err != nil {
			return 1, err
		}
		return printResult(c.do("POST", "/api/revisions/"+rev+"/promote", nil), o, w)
	case "abort":
		if err := need(); err != nil {
			return 1, err
		}
		return printResult(c.do("POST", "/api/revisions/"+rev+"/abort", nil), o, w)
	}
	return 1, fmt.Errorf("unknown rollout command %q", sub)
}

func rolloutStatus(c *client, o *options, w io.Writer) (int, error) {
	r := c.do("GET", "/api/fleet/status", nil)
	if err := r.apiError(); err != nil {
		return 1, err
	}
	var fleet struct {
		TargetRevision int64 `json:"target_revision"`
		CanaryRevision int64 `json:"canary_revision"`
		Converged      bool  `json:"converged"`
		Nodes          []struct {
			NodeID    string `json:"node_id"`
			Revision  int64  `json:"revision"`
			CanaryRev int64  `json:"canary_revision"`
			NodeGroup string `json:"node_group"`
			IsCanary  bool   `json:"is_canary"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(r.body, &fleet); err != nil {
		return 1, err
	}
	if o.output == "json" {
		w.Write(r.body)
		return 0, nil
	}
	fmt.Fprintf(w, "Stable revision: rev_%d   converged: %v\n", fleet.TargetRevision, fleet.Converged)
	if fleet.CanaryRevision > 0 {
		cs := c.do("GET", fmt.Sprintf("/api/revisions/%d/canary-status", fleet.CanaryRevision), nil)
		var st struct {
			TrafficPercent int    `json:"traffic_percent"`
			Header         string `json:"header"`
			Comparison     struct {
				Canary, Baseline struct {
					Requests     int64   `json:"requests"`
					ErrorRatePct float64 `json:"error_rate_pct"`
				}
			} `json:"comparison"`
		}
		_ = json.Unmarshal(cs.body, &st)
		fmt.Fprintf(w, "Canary revision: rev_%d   traffic: %d%%", fleet.CanaryRevision, st.TrafficPercent)
		if st.Header != "" {
			fmt.Fprintf(w, " + header %s", st.Header)
		}
		fmt.Fprintf(w, "\n  canary:   %d requests, %.2f%% errors\n  baseline: %d requests, %.2f%% errors\n",
			st.Comparison.Canary.Requests, st.Comparison.Canary.ErrorRatePct, st.Comparison.Baseline.Requests, st.Comparison.Baseline.ErrorRatePct)
	} else {
		fmt.Fprintln(w, "Canary revision: none in flight")
	}
	fmt.Fprintln(w, "\nNODE                          GROUP       REVISION  CANARY")
	for _, n := range fleet.Nodes {
		canary := "-"
		if n.IsCanary {
			canary = "canary node"
		} else if n.CanaryRev > 0 {
			canary = fmt.Sprintf("split rev_%d", n.CanaryRev)
		}
		fmt.Fprintf(w, "%-29s %-11s rev_%-5d %s\n", n.NodeID, n.NodeGroup, n.Revision, canary)
	}
	return 0, nil
}

func cmdRevisions(c *client, o *options, w io.Writer) (int, error) {
	r := c.do("GET", "/api/revisions?limit=20", nil)
	if err := r.apiError(); err != nil {
		return 1, err
	}
	if o.output == "json" {
		w.Write(r.body)
		return 0, nil
	}
	var revs []struct {
		Revision    int64     `json:"revision"`
		CreatedAt   time.Time `json:"created_at"`
		CreatedBy   string    `json:"created_by"`
		Description string    `json:"description"`
		Status      string    `json:"status"`
	}
	if err := json.Unmarshal(r.body, &revs); err != nil {
		return 1, err
	}
	fmt.Fprintln(w, "REVISION  STATUS       CREATED              BY                    DESCRIPTION")
	for _, rv := range revs {
		fmt.Fprintf(w, "rev_%-5d %-12s %-20s %-21s %s\n", rv.Revision, rv.Status, rv.CreatedAt.Local().Format("2006-01-02 15:04:05"), trunc(rv.CreatedBy, 21), rv.Description)
	}
	return 0, nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func cmdTest(c *client, o *options, stdout io.Writer) (int, error) {
	if len(o.args) == 0 {
		return 1, errors.New("usage: relayopsctl test validate -f FILE | test import -f FILE --api API_ID | test run SUITE_ID [--env ENV_ID] [--target-revision REV] [--wait]")
	}
	sub := o.args[0]
	if sub == "validate" {
		return cmdTestValidate(o, stdout)
	}
	if sub == "import" {
		return cmdTestImport(c, o, stdout)
	}
	if sub != "run" {
		return 1, fmt.Errorf("unknown test command %q (expected 'validate', 'import' or 'run')", sub)
	}
	if len(o.args) < 2 {
		return 1, errors.New("missing suite ID: relayopsctl test run SUITE_ID")
	}
	suiteID := o.args[1]

	payload := map[string]any{
		"suite_id": suiteID,
	}
	if o.envID != "" {
		payload["environment_id"] = o.envID
	}
	if o.targetRevision > 0 {
		payload["target_revision"] = o.targetRevision
	}
	b, _ := json.Marshal(payload)
	res := c.do("POST", "/api/tests/runs", b)
	if res.err != nil {
		return 1, res.err
	}
	if res.status >= 400 {
		return 1, fmt.Errorf("HTTP %d: %s", res.status, string(res.body))
	}

	var run struct {
		ID             string `json:"id"`
		Mode           string `json:"mode"`
		LifecycleState string `json:"lifecycle_state"`
		TotalSteps     int    `json:"total_steps"`
	}
	if err := json.Unmarshal(res.body, &run); err != nil {
		return 1, err
	}

	if !o.wait {
		if o.format == "json" {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(run)
		} else {
			fmt.Fprintf(stdout, "Triggered test run %s (mode: %s, %d steps)\n", run.ID, run.Mode, run.TotalSteps)
		}
		return 0, nil
	}

	if o.format == "text" {
		fmt.Fprintf(stdout, "Triggered test run %s (mode: %s, %d steps)\n", run.ID, run.Mode, run.TotalSteps)
		fmt.Fprintln(stdout, "Waiting for execution to complete...")
	}
	start := time.Now()
	for {
		time.Sleep(1 * time.Second)
		pollRes := c.do("GET", "/api/tests/runs/"+run.ID, nil)
		if pollRes.err != nil {
			return 1, pollRes.err
		}
		var detail TestRunDetail
		if err := json.Unmarshal(pollRes.body, &detail); err != nil {
			return 1, err
		}

		state := detail.Run.LifecycleState
		if state == "completed" || state == "failed" || state == "cancelled" || state == "interrupted" {
			switch o.format {
			case "json":
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(detail)
			case "junit":
				renderJUnit(detail, suiteID, time.Since(start).Seconds(), stdout)
			default:
				fmt.Fprintf(stdout, "\nRun completed in %s: %s\n", time.Since(start).Round(time.Millisecond), strings.ToUpper(state))
				fmt.Fprintf(stdout, "Summary: %d passed, %d failed, %d skipped (observed rev_%d)\n\n",
					detail.Run.PassedSteps, detail.Run.FailedSteps, detail.Run.SkippedSteps, detail.Run.ActualRevision)
				for _, step := range detail.Steps {
					statusIcon := "✓"
					if len(step.AssertionResults) > 0 {
						for _, a := range step.AssertionResults {
							if !a.Passed {
								statusIcon = "✗"
								break
							}
						}
					}
					fmt.Fprintf(stdout, "  [%s] Step %d: %s %s -> HTTP %d (%.1fms, rev_%d)\n",
						statusIcon, step.StepIndex, step.Method, step.URL, step.StatusCode, step.DurationMS, step.ObservedRevision)
					for _, a := range step.AssertionResults {
						if !a.Passed {
							fmt.Fprintf(stdout, "      Assertion failed: %s (%s) expected=%s, actual=%s\n", a.Type, a.Target, a.Expected, a.Actual)
						}
					}
				}
				if detail.Run.FailureReason != "" {
					fmt.Fprintf(stdout, "\nFailure reason: %s\n", detail.Run.FailureReason)
				}
			}

			if state == "completed" && detail.Run.FailedSteps == 0 && (detail.Run.PassedSteps > 0 || len(detail.Steps) > 0) {
				return 0, nil
			}
			return 1, nil
		}
		if time.Since(start) > 5*time.Minute {
			return 1, errors.New("timeout waiting for test run completion (>5m)")
		}
	}
}

func cmdTestValidate(o *options, stdout io.Writer) (int, error) {
	if o.file == "" {
		return 1, errors.New("usage: relayopsctl test validate -f FILE")
	}
	raw, err := os.ReadFile(o.file)
	if err != nil {
		return 1, err
	}
	var def store.SuiteDefinition
	if err := json.Unmarshal(raw, &def); err != nil {
		if yerr := yaml.Unmarshal(raw, &def); yerr != nil {
			return 1, fmt.Errorf("suite file is not JSON or YAML: %w", err)
		}
	}
	if err := testingstudio.ValidateSuiteDefinition(def); err != nil {
		return 1, fmt.Errorf("suite validation failed: %w", err)
	}
	fmt.Fprintf(stdout, "Valid suite %q: %d request(s) configured.\n", def.Name, len(def.Requests))
	return 0, nil
}

func cmdTestImport(c *client, o *options, stdout io.Writer) (int, error) {
	if o.file == "" {
		return 1, errors.New("usage: relayopsctl test import -f FILE --api API_ID")
	}
	if o.apiID == "" {
		return 1, errors.New("test import requires --api API_ID")
	}
	raw, err := os.ReadFile(o.file)
	if err != nil {
		return 1, err
	}
	var definition map[string]any
	if err := json.Unmarshal(raw, &definition); err != nil {
		if yerr := yaml.Unmarshal(raw, &definition); yerr != nil {
			return 1, fmt.Errorf("suite file is not JSON or YAML: %w", err)
		}
	}
	name, _ := definition["name"].(string)
	if name == "" {
		name = filepath.Base(o.file)
	}
	desc, _ := definition["description"].(string)
	payload, err := json.Marshal(map[string]any{
		"name":        name,
		"description": desc,
		"api_id":      o.apiID,
		"ownership":   "team",
		"definition":  definition,
	})
	if err != nil {
		return 1, err
	}
	res := c.do("POST", "/api/tests/suites", payload)
	if res.err != nil {
		return 1, res.err
	}
	if res.status >= 400 {
		return 1, fmt.Errorf("HTTP %d: %s", res.status, string(res.body))
	}
	var created struct {
		Suite struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"suite"`
	}
	if err := json.Unmarshal(res.body, &created); err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "Imported suite %s (%s)\n", created.Suite.ID, created.Suite.Name)
	return 0, nil
}

type TestRunDetail struct {
	Run struct {
		ID             string `json:"id"`
		LifecycleState string `json:"lifecycle_state"`
		PassedSteps    int    `json:"passed_steps"`
		FailedSteps    int    `json:"failed_steps"`
		SkippedSteps   int    `json:"skipped_steps"`
		ActualRevision int64  `json:"actual_revision"`
		FailureReason  string `json:"failure_reason"`
	} `json:"run"`
	Steps []TestRunStepDetail `json:"steps"`
}

type TestRunStepDetail struct {
	StepIndex        int                    `json:"step_index"`
	RequestName      string                 `json:"request_name"`
	Method           string                 `json:"method"`
	URL              string                 `json:"url"`
	StatusCode       int                    `json:"status_code"`
	ObservedRevision int64                  `json:"observed_revision"`
	DurationMS       float64                `json:"duration_ms"`
	AssertionResults []TestStepAssertionRes `json:"assertion_results"`
}

type TestStepAssertionRes struct {
	Type     string `json:"type"`
	Target   string `json:"target"`
	Passed   bool   `json:"passed"`
	Error    string `json:"error"`
	Actual   string `json:"actual"`
	Expected string `json:"expected"`
}

type JUnitTestSuites struct {
	XMLName    xml.Name         `xml:"testsuites"`
	TestSuites []JUnitTestSuite `xml:"testsuite"`
}

type JUnitTestSuite struct {
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	Skipped   int             `xml:"skipped,attr"`
	Time      float64         `xml:"time,attr"`
	TestCases []JUnitTestCase `xml:"testcase"`
}

type JUnitTestCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *JUnitFailure `xml:"failure,omitempty"`
	Skipped   *JUnitSkipped `xml:"skipped,omitempty"`
}

type JUnitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Content string `xml:",chardata"`
}

type JUnitSkipped struct {
	Message string `xml:"message,attr,omitempty"`
}

func renderJUnit(detail TestRunDetail, suiteID string, totalSeconds float64, w io.Writer) {
	ts := JUnitTestSuite{
		Name:     "TestStudio-" + suiteID,
		Tests:    len(detail.Steps),
		Failures: detail.Run.FailedSteps,
		Skipped:  detail.Run.SkippedSteps,
		Time:     totalSeconds,
	}
	for _, step := range detail.Steps {
		tc := JUnitTestCase{
			Name:      fmt.Sprintf("Step %d: %s %s", step.StepIndex, step.Method, step.RequestName),
			ClassName: "relayops.teststudio." + suiteID,
			Time:      step.DurationMS / 1000.0,
		}
		var failureMsgs []string
		for _, a := range step.AssertionResults {
			if !a.Passed {
				failureMsgs = append(failureMsgs, fmt.Sprintf("%s (%s): expected %s, actual %s", a.Type, a.Target, a.Expected, a.Actual))
			}
		}
		if len(failureMsgs) > 0 {
			tc.Failure = &JUnitFailure{
				Message: "Assertion failure",
				Type:    "AssertionError",
				Content: strings.Join(failureMsgs, "\n"),
			}
		}
		ts.TestCases = append(ts.TestCases, tc)
	}
	suites := JUnitTestSuites{TestSuites: []JUnitTestSuite{ts}}
	b, err := xml.MarshalIndent(suites, "", "  ")
	if err == nil {
		fmt.Fprintln(w, xml.Header+string(b))
	}
}

func cmdBundle(o *options, stdout io.Writer) (int, error) {
	if len(o.args) == 0 {
		return 1, errors.New("usage: relayopsctl bundle build|validate [flags]")
	}
	subcmd := o.args[0]
	switch subcmd {
	case "build":
		dir := o.dir
		if dir == "" {
			dir = "."
		}
		env := o.envID
		bundle, err := apiops.CompileDirectory(dir, env)
		if err != nil {
			return 1, fmt.Errorf("bundle build failed: %w", err)
		}
		data, err := json.MarshalIndent(bundle, "", "  ")
		if err != nil {
			return 1, err
		}
		if o.out != "" {
			if err := os.WriteFile(o.out, data, 0644); err != nil {
				return 1, fmt.Errorf("write bundle output: %w", err)
			}
			fmt.Fprintf(stdout, "Wrote bundle to %s (source_hash: %s, rendered_hash: %s, %d APIs, %d plans)\n",
				o.out, bundle.SourceHash, bundle.RenderedHash, len(bundle.Config.APIs), len(bundle.Config.Plans))
		} else {
			fmt.Fprintln(stdout, string(data))
		}
		return 0, nil

	case "validate":
		targetFile := o.file
		if targetFile == "" && len(o.args) > 1 {
			targetFile = o.args[1]
		}
		if targetFile == "" {
			return 1, errors.New("usage: relayopsctl bundle validate -f FILE")
		}
		b, err := os.ReadFile(targetFile)
		if err != nil {
			return 1, fmt.Errorf("read bundle file: %w", err)
		}
		var bundle apiops.CompiledBundle
		if err := json.Unmarshal(b, &bundle); err != nil {
			return 1, fmt.Errorf("invalid bundle JSON: %w", err)
		}
		if err := apiops.ValidateBundle(&bundle); err != nil {
			return 1, fmt.Errorf("bundle validation failed: %w", err)
		}
		fmt.Fprintf(stdout, "Bundle is valid:\n  Format: %s\n  Source Hash: %s\n  Rendered Hash: %s\n  APIs: %d\n  Plans: %d\n  Files: %d\n",
			bundle.FormatVersion, bundle.SourceHash, bundle.RenderedHash, len(bundle.Config.APIs), len(bundle.Config.Plans), len(bundle.Files))
		return 0, nil

	default:
		return 1, fmt.Errorf("unknown bundle subcommand %q (use build or validate)", subcmd)
	}
}

func cmdMaintenance(c *client, o *options, stdout io.Writer) (int, error) {
	if len(o.args) == 0 {
		return 1, errors.New("usage: relayopsctl maintenance purge [flags]")
	}
	subcmd := o.args[0]
	switch subcmd {
	case "purge":
		olderThan := o.olderThan
		if olderThan == "" {
			olderThan = "24h"
		}
		path := fmt.Sprintf("/api/system/maintenance/purge?older_than=%s", url.QueryEscape(olderThan))
		if o.vacuum {
			path += "&vacuum=true"
		}
		resp := c.do("POST", path, nil)
		if err := resp.apiError(); err != nil {
			return 1, err
		}
		if o.output == "json" {
			stdout.Write(resp.body)
			fmt.Fprintln(stdout)
			return 0, nil
		}
		var res struct {
			Status           string `json:"status"`
			Message          string `json:"message"`
			OlderThan        string `json:"older_than"`
			LogsPurged       int64  `json:"logs_purged"`
			TestStudioPurged int64  `json:"test_studio_purged"`
			SessionsPurged   int64  `json:"sessions_purged"`
			VacuumExecuted   bool   `json:"vacuum_executed"`
			PurgedAt         string `json:"purged_at"`
		}
		if err := json.Unmarshal(resp.body, &res); err != nil {
			stdout.Write(resp.body)
			return 0, nil
		}
		fmt.Fprintf(stdout, "Maintenance Purge Complete:\n  Status: %s\n  Older Than: %s\n  Logs Purged: %d\n  Test Studio Records Purged: %d\n  Expired Sessions Purged: %d\n  VACUUM Executed: %v\n  Timestamp: %s\n",
			res.Status, res.OlderThan, res.LogsPurged, res.TestStudioPurged, res.SessionsPurged, res.VacuumExecuted, res.PurgedAt)
		return 0, nil
	default:
		return 1, fmt.Errorf("unknown maintenance subcommand %q (use purge)", subcmd)
	}
}
