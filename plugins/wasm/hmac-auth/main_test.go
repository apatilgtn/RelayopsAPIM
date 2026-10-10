package main

import (
	"strconv"
	"testing"

	pt "github.com/relayops/apim/plugins/wasm/internal/plugintest"
	plugin "github.com/relayops/apim/sdk/wasmplugin"
)

func TestHMACAuth(t *testing.T) {
	const now = int64(1_790_000_000)
	plugin.NowMS = func() int64 { return now * 1000 }
	const secret = "partner-a-shared-secret-0123456789"
	cfg := map[string]any{"keys": map[string]string{"partner-a": secret}}
	body := `{"event":"paid"}`
	signed := func(ts int64, sig string) pt.Input {
		tss := strconv.FormatInt(ts, 10)
		if sig == "" {
			sig = Sign(secret, "POST", "/hooks/pay", tss, body)
		}
		return pt.Input{Method: "POST", Path: "/hooks/pay", Body: pt.Body(body), Config: cfg,
			Headers: map[string][]string{"X-Key-Id": {"partner-a"}, "X-Timestamp": {tss}, "X-Signature": {sig}}}
	}

	ok := pt.Run(t, handle, signed(now-10, ""))
	pt.Expect(t, ok, "modify", 0)
	if ok.Headers["X-Authenticated-Key-Id"] != "partner-a" || len(ok.RemoveHeaders) != 3 {
		t.Fatalf("success result %+v", ok)
	}

	pt.Expect(t, pt.Run(t, handle, signed(now, "00"+Sign(secret, "POST", "/hooks/pay", strconv.FormatInt(now, 10), body)[2:])), "deny", 401)
	pt.Expect(t, pt.Run(t, handle, signed(now-301, "")), "deny", 401) // too old

	tampered := signed(now, "")
	tampered.Body = pt.Body(`{"event":"refund"}`)
	pt.Expect(t, pt.Run(t, handle, tampered), "deny", 401)

	unknown := signed(now, "")
	unknown.Headers["X-Key-Id"] = []string{"partner-b"}
	pt.Expect(t, pt.Run(t, handle, unknown), "deny", 401)

	pt.Expect(t, pt.Run(t, handle, pt.Input{Method: "POST", Body: pt.Body(""), Config: cfg}), "deny", 401)
	pt.Expect(t, pt.Run(t, handle, pt.Input{Method: "POST", Body: pt.Body(""), Config: map[string]any{"keys": map[string]string{"k": "short"}}}), "deny", 500)
}
