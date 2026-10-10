package policy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// WasmAction represents the decision action returned by a WebAssembly policy plugin.
type WasmAction string

const (
	ActionAllow  WasmAction = "allow"
	ActionDeny   WasmAction = "deny"
	ActionModify WasmAction = "modify"
)

// WasmPolicyResult represents the decision returned by a WebAssembly policy plugin.
type WasmPolicyResult struct {
	Action  WasmAction        `json:"action"`
	Status  int               `json:"status,omitempty"`
	Reason  string            `json:"reason,omitempty"`
	Detail  string            `json:"detail,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// RemoveHeaders are deleted from the upstream request (modify only).
	RemoveHeaders []string `json:"remove_headers,omitempty"`
	// Body, when set on a modify result, replaces the upstream request body.
	Body *string `json:"body,omitempty"`
}

// maxIdleInstances bounds the reusable instances kept per plugin.
const maxIdleInstances = 64

// WasmPlugin encapsulates a compiled WebAssembly policy module. Instances are
// reused across requests (one request at a time each), so a plugin must not
// keep per-request state in globals.
type WasmPlugin struct {
	Name     string
	compiled wazero.CompiledModule
	runtime  wazero.Runtime
	idle     chan api.Module
	// phases says which phases the plugin handles: PhaseRequest, PhaseResponse.
	phases int32
}

// Plugin phases (the optional "relayops_phases" export returns a bitmask).
const (
	PhaseRequest  int32 = 1
	PhaseResponse int32 = 2
)

// HandlesResponses reports whether the plugin runs on responses.
func (p *WasmPlugin) HandlesResponses() bool { return p != nil && p.phases&PhaseResponse != 0 }

// HandlesRequests reports whether the plugin runs on requests.
func (p *WasmPlugin) HandlesRequests() bool { return p != nil && p.phases&PhaseRequest != 0 }

// WasmManager manages compilation, caching, and execution of WebAssembly policy plugins.
type WasmManager struct {
	ctx     context.Context
	runtime wazero.Runtime
	mu      sync.RWMutex
	plugins map[string]*WasmPlugin
}

// NewWasmManager initializes a thread-safe WebAssembly policy manager.
func NewWasmManager(ctx context.Context) (*WasmManager, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Close module instances when the request context ends, so a plugin stuck
	// in a loop cannot hold a request (or a goroutine) past its deadline.
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
	// Enable WASI snapshot preview1 host imports for standard compiler toolchains (Rust, TinyGo)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("failed to instantiate WASI host environment: %w", err)
	}

	// Register RelayOps host functions in module "env"
	_, err := r.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, level int32, ptr uint32, length uint32) {
			buf, ok := mod.Memory().Read(ptr, length)
			if !ok {
				return
			}
			msg := string(buf)
			switch level {
			case 1:
				slog.Debug("wasm plugin log", "msg", msg)
			case 2:
				slog.Info("wasm plugin log", "msg", msg)
			case 3:
				slog.Warn("wasm plugin log", "msg", msg)
			default:
				slog.Error("wasm plugin log", "msg", msg)
			}
		}).
		Export("relayops_log").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context) int64 {
			return time.Now().UnixMilli()
		}).
		Export("relayops_now_ms").
		Instantiate(ctx)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("failed to register RelayOps host module: %w", err)
	}

	return &WasmManager{
		ctx:     ctx,
		runtime: r,
		plugins: make(map[string]*WasmPlugin),
	}, nil
}

// Close terminates the Wazero runtime and frees allocated resources.
func (m *WasmManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime.Close(m.ctx)
}

// RegisterPlugin compiles and stores a WebAssembly policy module by name.
func (m *WasmManager) RegisterPlugin(ctx context.Context, name string, wasmBytes []byte) (*WasmPlugin, error) {
	if name == "" {
		return nil, errors.New("plugin name cannot be empty")
	}
	if len(wasmBytes) == 0 {
		return nil, errors.New("wasm bytecode cannot be empty")
	}

	compiled, err := m.runtime.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to compile wasm module %q: %w", name, err)
	}

	plugin := &WasmPlugin{
		Name:     name,
		compiled: compiled,
		runtime:  m.runtime,
		idle:     make(chan api.Module, maxIdleInstances),
	}

	// Keep one warm instance ready so the first request does not pay for it.
	mod, err := plugin.instantiate()
	if err != nil {
		return nil, fmt.Errorf("failed to start wasm module %q: %w", name, err)
	}
	plugin.phases = detectPhases(ctx, mod)
	plugin.release(mod, true)

	m.mu.Lock()
	m.plugins[name] = plugin
	m.mu.Unlock()

	return plugin, nil
}

// Names lists the registered plugins, sorted.
func (m *WasmManager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.plugins))
	for n := range m.plugins {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// GetPlugin retrieves a registered plugin by name.
func (m *WasmManager) GetPlugin(name string) (*WasmPlugin, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.plugins[name]
	return p, ok
}

// instantiate creates a module instance. Reactor modules (Go
// -buildmode=c-shared, TinyGo, Rust cdylib) are initialised through
// _initialize; a command module's _start is not run.
func (p *WasmPlugin) instantiate() (api.Module, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize").
		WithStdout(io.Discard).
		WithStderr(io.Discard).
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(rand.Reader)
	mod, err := p.runtime.InstantiateModule(ctx, p.compiled, cfg)
	if err != nil {
		return nil, err
	}
	// A new instance's first call pays one-time costs (for Go modules, the
	// runtime and reflection caches: tens of milliseconds). Pay them here,
	// outside any request's deadline, with a throwaway request.
	if _, err := p.call(ctx, mod, Request{Method: http.MethodGet, Path: "/"}); err != nil {
		_ = mod.Close(context.Background())
		return nil, fmt.Errorf("plugin warm-up: %w", err)
	}
	return mod, nil
}

func (p *WasmPlugin) acquire() (api.Module, error) {
	for {
		select {
		case mod := <-p.idle:
			if !mod.IsClosed() {
				return mod, nil
			}
		default:
			return p.instantiate()
		}
	}
}

// release returns a healthy instance to the pool; a failed one is dropped,
// since its memory may be in any state.
func (p *WasmPlugin) release(mod api.Module, healthy bool) {
	if healthy && !mod.IsClosed() {
		select {
		case p.idle <- mod:
			return
		default:
		}
	}
	_ = mod.Close(context.Background())
}

// Execute evaluates the WebAssembly policy plugin against the incoming HTTP request.
func (p *WasmPlugin) Execute(ctx context.Context, req Request) (WasmPolicyResult, error) {
	if p == nil || p.compiled == nil {
		return WasmPolicyResult{Action: ActionAllow}, nil
	}
	mod, err := p.acquire()
	if err != nil {
		return WasmPolicyResult{
			Action: ActionDeny,
			Status: http.StatusInternalServerError,
			Reason: "wasm_instantiation_failed",
			Detail: err.Error(),
		}, err
	}
	res, err := p.call(ctx, mod, req)
	p.release(mod, err == nil)
	return res, err
}

// detectPhases asks the module which phases it handles, falling back to
// its exports: execute_response means it handles responses.
func detectPhases(ctx context.Context, mod api.Module) int32 {
	if fn := mod.ExportedFunction("relayops_phases"); fn != nil {
		if res, err := fn.Call(ctx); err == nil && len(res) > 0 {
			return int32(res[0])
		}
	}
	phases := PhaseRequest
	if mod.ExportedFunction("execute_response") != nil {
		phases |= PhaseResponse
	}
	return phases
}

// invoke runs a JSON-in/JSON-out export (execute_policy, execute_response).
// ok is false when the module does not export it.
func invoke(ctx context.Context, mod api.Module, export string, input any) (result WasmPolicyResult, ok bool, err error) {
	execFn := mod.ExportedFunction(export)
	if execFn == nil {
		return WasmPolicyResult{}, false, nil
	}
	allocFn := mod.ExportedFunction("allocate")
	if allocFn == nil {
		allocFn = mod.ExportedFunction("malloc")
	}
	inputBytes, err := json.Marshal(input)
	if err != nil {
		return WasmPolicyResult{Action: ActionDeny, Status: http.StatusInternalServerError, Reason: "wasm_input_invalid"}, true, err
	}
	var inputPtr uint32 = 0
	if allocFn != nil {
		res, err := allocFn.Call(ctx, uint64(len(inputBytes)))
		if err != nil {
			return WasmPolicyResult{Action: ActionDeny, Status: http.StatusInternalServerError, Reason: "wasm_alloc_failed"}, true, err
		}
		inputPtr = uint32(res[0])
	}
	if !mod.Memory().Write(inputPtr, inputBytes) {
		return WasmPolicyResult{Action: ActionDeny, Status: http.StatusInternalServerError, Reason: "wasm_memory_write_failed"}, true, errors.New("failed to write into wasm memory")
	}
	res, err := execFn.Call(ctx, uint64(inputPtr), uint64(len(inputBytes)))
	if err != nil {
		return WasmPolicyResult{Action: ActionDeny, Status: http.StatusInternalServerError, Reason: "wasm_execution_error", Detail: err.Error()}, true, err
	}
	if len(res) == 0 || uint32(res[0]&0xFFFFFFFF) == 0 {
		return WasmPolicyResult{Action: ActionAllow}, true, nil
	}
	outPtr, outLen := uint32(res[0]>>32), uint32(res[0]&0xFFFFFFFF)
	outBytes, okRead := mod.Memory().Read(outPtr, outLen)
	if !okRead {
		return WasmPolicyResult{Action: ActionDeny, Status: http.StatusInternalServerError, Reason: "wasm_result_unreadable"}, true, errors.New("plugin result is outside wasm memory")
	}
	if err := json.Unmarshal(outBytes, &result); err != nil {
		return WasmPolicyResult{Action: ActionDeny, Status: http.StatusInternalServerError, Reason: "wasm_result_invalid", Detail: err.Error()}, true, err
	}
	if result.Action == "" {
		result.Action = ActionAllow
	}
	return result, true, nil
}

// ResponseInput is what a response-phase plugin receives.
type ResponseInput struct {
	Status int
	Header http.Header
	// Body is the response body as text; nil when the API does not give
	// plugins response bodies, or the body was too large or streamed.
	Body          []byte
	BodyTruncated bool
	Request       Request
	PluginConfig  json.RawMessage
}

// ExecuteResponse runs the plugin's response phase.
func (p *WasmPlugin) ExecuteResponse(ctx context.Context, in ResponseInput) (WasmPolicyResult, error) {
	if !p.HandlesResponses() {
		return WasmPolicyResult{Action: ActionAllow}, nil
	}
	mod, err := p.acquire()
	if err != nil {
		return WasmPolicyResult{Action: ActionDeny, Status: http.StatusInternalServerError, Reason: "wasm_instantiation_failed", Detail: err.Error()}, err
	}
	input := map[string]any{
		"phase":   "response",
		"status":  in.Status,
		"headers": in.Header,
		"request": map[string]any{"method": in.Request.Method, "path": in.Request.Path, "client_ip": in.Request.ClientIP, "headers": in.Request.Header},
	}
	if in.Body != nil {
		input["body"] = string(in.Body)
	}
	if in.BodyTruncated {
		input["body_truncated"] = true
	}
	if len(in.PluginConfig) > 0 {
		input["config"] = in.PluginConfig
	}
	res, _, err := invoke(ctx, mod, "execute_response", input)
	p.release(mod, err == nil)
	return res, err
}

func (p *WasmPlugin) call(ctx context.Context, mod api.Module, req Request) (WasmPolicyResult, error) {
	input := map[string]any{
		"method":       req.Method,
		"path":         req.Path,
		"client_ip":    req.ClientIP,
		"headers":      req.Header,
		"query_params": req.QueryParams,
	}
	if req.Body != nil {
		input["body"] = string(req.Body)
	}
	if len(req.PluginConfig) > 0 {
		input["config"] = req.PluginConfig
	}
	if res, ok, err := invoke(ctx, mod, "execute_policy", input); ok {
		return res, err
	}
	// Alternatively, support lightweight direct status code function: "process_request"
	if procFn := mod.ExportedFunction("process_request"); procFn != nil {
		res, err := procFn.Call(ctx)
		if err != nil {
			return WasmPolicyResult{Action: ActionDeny, Status: http.StatusInternalServerError, Reason: "wasm_execution_error", Detail: err.Error()}, err
		}
		if len(res) > 0 {
			code := int(res[0])
			if code == 0 || code == http.StatusOK {
				return WasmPolicyResult{Action: ActionAllow}, nil
			}
			if code >= 400 && code < 600 {
				return WasmPolicyResult{
					Action: ActionDeny,
					Status: code,
					Reason: fmt.Sprintf("wasm_policy_denied_%d", code),
					Detail: fmt.Sprintf("request blocked by wasm policy with status %d", code),
				}, nil
			}
		}
	}

	return WasmPolicyResult{Action: ActionAllow}, nil
}
