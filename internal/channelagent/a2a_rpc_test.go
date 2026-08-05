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

	bad := RPCFail(7, RPCForbidden, "nope")
	blob, _ = json.Marshal(bad)
	decoded = map[string]any{}
	_ = json.Unmarshal(blob, &decoded)
	if _, hasResult := decoded["result"]; hasResult {
		t.Fatal("error response must not carry result")
	}
}
