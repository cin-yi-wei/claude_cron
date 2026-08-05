package channelagent

import "encoding/json"

const (
	RPCParseError     = -32700
	RPCInvalidRequest = -32600
	RPCMethodNotFound = -32601
	RPCInvalidParams  = -32602
	RPCInternalError  = -32603
	// Application-defined range.
	RPCUnauthorized = -32001
	RPCForbidden    = -32002
	RPCCapacityFull = -32003
)

type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type RPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

func ParseRPC(body []byte) (RPCRequest, *RPCError) {
	var req RPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return RPCRequest{}, &RPCError{Code: RPCParseError, Message: "malformed JSON"}
	}
	if req.JSONRPC != "2.0" {
		return RPCRequest{}, &RPCError{Code: RPCInvalidRequest, Message: "jsonrpc must be \"2.0\""}
	}
	if req.Method == "" {
		return RPCRequest{}, &RPCError{Code: RPCInvalidRequest, Message: "method is required"}
	}
	return req, nil
}

func RPCOK(id any, result any) RPCResponse {
	return RPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func RPCFail(id any, code int, msg string) RPCResponse {
	return RPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
}
