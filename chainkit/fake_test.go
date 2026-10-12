package chainkit

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

// 测试用的极简 JSON-RPC 假节点。所有测试都不联网、不依赖外部服务。

type rpcRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// recordedCall 一次被节点记下的调用
type recordedCall struct {
	Method string
	Params []json.RawMessage
}

// fakeNode 假 JSON-RPC 节点。
//
// handle 返回 (nil, nil) 表示「本方法不自定义处理」，落回默认实现；
// httpStatus 非 0 时整个端点直接返回该状态码（用于注入 429 / 5xx）。
type fakeNode struct {
	t      *testing.T
	srv    *httptest.Server
	handle func(n *fakeNode, method string, params []json.RawMessage) (any, *rpcError)

	mu         sync.Mutex
	calls      []recordedCall
	chainID    uint64
	head       uint64
	httpStatus int
	batchLimit int // >0 时，元素数超过它的批量请求直接回 400（模拟节点的体积限制）
	requests   int
}

func newFakeNode(t *testing.T, chainID uint64) *fakeNode {
	t.Helper()
	n := &fakeNode{t: t, chainID: chainID}
	n.srv = httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNode) URL() string { return n.srv.URL }

// requestCount HTTP 请求数（批量请求算一次）。方法调用次数用 callsOf 数。
func (n *fakeNode) requestCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.requests
}

func (n *fakeNode) setHead(h uint64) {
	n.mu.Lock()
	n.head = h
	n.mu.Unlock()
}

func (n *fakeNode) setHTTPStatus(status int) {
	n.mu.Lock()
	n.httpStatus = status
	n.mu.Unlock()
}

func (n *fakeNode) recorded() []recordedCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]recordedCall(nil), n.calls...)
}

// callsOf 某方法的调用记录
func (n *fakeNode) callsOf(method string) []recordedCall {
	var out []recordedCall
	for _, c := range n.recorded() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (n *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	n.mu.Lock()
	status := n.httpStatus
	n.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	n.mu.Lock()
	n.requests++
	n.mu.Unlock()

	trimmed := bytes.TrimSpace(body)
	w.Header().Set("Content-Type", "application/json")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var reqs []rpcRequest
		if err := json.Unmarshal(trimmed, &reqs); err != nil {
			n.t.Errorf("假节点收到无法解析的批量请求: %v", err)
			return
		}
		n.mu.Lock()
		limit := n.batchLimit
		n.mu.Unlock()
		if limit > 0 && len(reqs) > limit {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Bad Request"))
			return
		}
		resps := make([]rpcResponse, 0, len(reqs))
		for _, req := range reqs {
			resps = append(resps, n.respond(req))
		}
		_ = json.NewEncoder(w).Encode(resps)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		n.t.Errorf("假节点收到无法解析的请求: %v", err)
		return
	}
	_ = json.NewEncoder(w).Encode(n.respond(req))
}

func (n *fakeNode) respond(req rpcRequest) rpcResponse {
	n.mu.Lock()
	n.calls = append(n.calls, recordedCall{Method: req.Method, Params: req.Params})
	handler := n.handle
	chainID, head := n.chainID, n.head
	n.mu.Unlock()

	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}

	if req.Method == "eth_chainId" {
		resp.Result = hexutil.Uint64(chainID)
		return resp
	}
	if handler != nil {
		if result, rpcErr := handler(n, req.Method, req.Params); rpcErr != nil {
			resp.Error = rpcErr
			return resp
		} else if result != nil {
			resp.Result = result
			return resp
		}
	}
	if req.Method == "eth_blockNumber" {
		resp.Result = hexutil.Uint64(head)
		return resp
	}
	// 默认给一个空区块：代币路径补时间戳会跟进这次查询，大多数测试并不关心它的内容
	if req.Method == "eth_getBlockByNumber" && len(req.Params) > 0 {
		var number hexutil.Uint64
		if err := json.Unmarshal(req.Params[0], &number); err == nil {
			resp.Result = map[string]any{
				"number":       number,
				"timestamp":    hexutil.Uint64(1_700_000_000 + uint64(number)),
				"transactions": []any{},
			}
			return resp
		}
	}
	resp.Error = &rpcError{Code: -32601, Message: "the method " + req.Method + " does not exist/is not available"}
	return resp
}

// paramObject 把单个请求参数解成对象，便于断言节点收到的过滤条件
func paramObject(t *testing.T, call recordedCall) map[string]any {
	t.Helper()
	if len(call.Params) == 0 {
		t.Fatalf("%s 没有参数", call.Method)
	}
	var out map[string]any
	if err := json.Unmarshal(call.Params[0], &out); err != nil {
		t.Fatalf("解析 %s 的参数失败: %v", call.Method, err)
	}
	return out
}
