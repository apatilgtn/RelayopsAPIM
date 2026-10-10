// Command header-rewrite sets, renames and removes upstream request headers.
//
//	"wasm_config": {"header-rewrite": {
//	  "set":    {"X-Env": "production"},
//	  "rename": {"X-Api-Version": "Accept-Version"},
//	  "remove": ["X-Debug", "Cookie"],
//	  "response": {"set": {"Cache-Control": "no-store"}, "remove": ["Server", "X-Powered-By"]}
//	}}
//
// "response" applies the same set/remove to the response the client gets.
//
// The gateway applies every removal before any header is set, so a header
// that is both removed and set (or renamed onto) ends up set.
package main

import plugin "github.com/relayops/apim/sdk/wasmplugin"

type headerOps struct {
	Set    map[string]string `json:"set"`
	Remove []string          `json:"remove"`
}

type config struct {
	Set      map[string]string `json:"set"`
	Rename   map[string]string `json:"rename"`
	Remove   []string          `json:"remove"`
	Response headerOps         `json:"response"`
}

func init() {
	plugin.Handle(handle)
	plugin.HandleResponse(handleResponse)
}

func handleResponse(r plugin.Response) plugin.Result {
	cfg, err := plugin.ResponseConfig[config](r, nil)
	if err != nil {
		return plugin.Deny(502, "plugin_misconfigured", err.Error())
	}
	if len(cfg.Response.Set) == 0 && len(cfg.Response.Remove) == 0 {
		return plugin.Allow()
	}
	res := plugin.Modify()
	for _, h := range cfg.Response.Remove {
		res = res.RemoveHeader(h)
	}
	for k, v := range cfg.Response.Set {
		res = res.SetHeader(k, v)
	}
	return res
}

func main() {}

func handle(r plugin.Request) plugin.Result {
	cfg, err := plugin.Config[config](r, nil)
	if err != nil {
		return plugin.Misconfigured(err)
	}
	res := plugin.Modify()
	for from, to := range cfg.Rename {
		if v := r.Header(from); v != "" {
			res = res.RemoveHeader(from).SetHeader(to, v)
		}
	}
	for _, h := range cfg.Remove {
		res = res.RemoveHeader(h)
	}
	for k, v := range cfg.Set {
		res = res.SetHeader(k, v)
	}
	return res
}
