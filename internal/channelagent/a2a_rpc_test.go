package channelagent

import (
	"encoding/json"
	"testing"
)

func TestParseRPCAcceptsValidRequest(t *testing.T) {
	req, rerr := ParseRPC([]byte(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"x":1}}`))
	if rerr != nil {
		t.Fatalf("unexpected error: %#v", rerr)
	}
	if req.Method != "message/send" {
		t.Fatalf("Method = %q", req.Method)
	}
}

func TestParseRPCRejectsMalformedJSON(t *testing.T) {
	_, rerr := ParseRPC([]byte(`{not json`))
	if rerr == nil || rerr.Code != RPCParseError {
		t.Fatalf("want parse error, got %#v", rerr)
	}
}

func TestParseRPCRejectsWrongVersionAndMissingMethod(t *testing.T) {
	_, rerr := ParseRPC([]byte(`{"jsonrpc":"1.0","id":1,"method":"m"}`))
	if rerr == nil || rerr.Code != RPCInvalidRequest {
		t.Fatalf("want invalid request for bad version, got %#v", rerr)
	}
	_, rerr = ParseRPC([]byte(`{"jsonrpc":"2.0","id":1}`))
	if rerr == nil || rerr.Code != RPCInvalidRequest {
		t.Fatalf("want invalid request for missing method, got %#v", rerr)
	}
}

func TestRPCOKAndFailShape(t *testing.T) {
	ok := RPCOK(7, map[string]string{"a": "b"})
	blob, _ := json.Marshal(ok)
	var decoded map[string]any
	_ = json.Unmarshal(blob, &decoded)
	if decoded["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v", decoded["jsonrpc"])
	}
	if _, hasErr := decoded["error"]; hasErr {
		t.Fatal("success response must not carry error")
	}
	if _, hasResult := decoded["result"]; !hasResult {
		t.Fatal("success response must carry result")
	}

	bad := RPCFail(7, RPCForbidden, "nope")
	blob, _ = json.Marshal(bad)
	decoded = map[string]any{}
	_ = json.Unmarshal(blob, &decoded)
	if _, hasResult := decoded["result"]; hasResult {
		t.Fatal("error response must not carry result")
	}
	if _, hasErr := decoded["error"]; !hasErr {
		t.Fatal("error response must carry error")
	}
	errObj, ok2 := decoded["error"].(map[string]any)
	if !ok2 {
		t.Fatalf("error field is not an object: %#v", decoded["error"])
	}
	if code, _ := errObj["code"].(float64); int(code) != RPCForbidden {
		t.Fatalf("error.code = %v, want %d", errObj["code"], RPCForbidden)
	}
}

// TestRPCOKAlwaysCarriesResultKey covers the bug where Result any
// `json:"result,omitempty"` only omitted the field when the interface value
// itself was nil, but a nil *result* (a legitimate "success, no payload"
// ack) must still marshal a "result" key per JSON-RPC 2.0 — a response must
// carry exactly one of "result" or "error", never neither.
func TestRPCOKAlwaysCarriesResultKey(t *testing.T) {
	cases := []struct {
		name   string
		result any
	}{
		{"nil", nil},
		{"false", false},
		{"zero", 0},
		{"empty string", ""},
		{"empty map", map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := RPCOK(1, tc.result)
			blob, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(blob, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if _, hasResult := decoded["result"]; !hasResult {
				t.Fatalf("RPCOK(1, %#v) must carry a result key, got %s", tc.result, blob)
			}
			if _, hasErr := decoded["error"]; hasErr {
				t.Fatalf("RPCOK(1, %#v) must not carry an error key, got %s", tc.result, blob)
			}
		})
	}

	// The nil case specifically must serialize the value null, not merely
	// be present-but-wrong.
	resp := RPCOK(1, nil)
	blob, _ := json.Marshal(resp)
	var decoded map[string]any
	_ = json.Unmarshal(blob, &decoded)
	if v, ok := decoded["result"]; !ok || v != nil {
		t.Fatalf("RPCOK(1, nil) result = %#v, want null", v)
	}
}

// TestRPCFailNeverCarriesResultKey guards the other direction: an error
// response must never carry a "result" key, always carries "error", and
// the error code round-trips through JSON unchanged.
func TestRPCFailNeverCarriesResultKey(t *testing.T) {
	resp := RPCFail(1, RPCInvalidParams, "bad params")
	blob, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, hasResult := decoded["result"]; hasResult {
		t.Fatalf("RPCFail must not carry a result key, got %s", blob)
	}
	errVal, hasErr := decoded["error"]
	if !hasErr {
		t.Fatalf("RPCFail must carry an error key, got %s", blob)
	}
	errObj, ok := errVal.(map[string]any)
	if !ok {
		t.Fatalf("error field is not an object: %#v", errVal)
	}
	if code, _ := errObj["code"].(float64); int(code) != RPCInvalidParams {
		t.Fatalf("error.code = %v, want %d", errObj["code"], RPCInvalidParams)
	}
}
