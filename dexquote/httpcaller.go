package dexquote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 本文件给 dexquote 配一个默认的取数通道：只用标准库的 JSON-RPC 客户端。
//
// 为什么单独放一份：询价的性能全看「一次问多少个问题」。一个一个问，
// 十几条候选路线就是十几个网络来回；打包成一批问出去，一次 HTTP 就够。
// 自己已经有节点客户端的项目可以不用这个文件，把 BatchCaller 换成自己的实现即可
// （只要保证返回顺序和入参一致）。

// RPCError 是节点明确回的一个错误（区别于网络不通这类传输层错误）。
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("节点返回错误 %d: %s", e.Code, e.Message)
}

// Reverted 判断这次调用是不是「合约执行被拒」。询价时大量用它来判断
// 「这个交易对没有池子」——这是预期内的结果，不是故障。
func (e *RPCError) Reverted() bool {
	if e.Code == 3 { // execution reverted
		return true
	}
	msg := strings.ToLower(e.Message)
	return strings.Contains(msg, "revert") || strings.Contains(msg, "invalid opcode") ||
		strings.Contains(msg, "out of gas") || strings.Contains(msg, "execution fail")
}

// IsRevert 判断错误链里有没有「合约拒绝执行」。
func IsRevert(err error) bool {
	var re *RPCError
	if errors.As(err, &re) {
		return re.Reverted()
	}
	return false
}

const (
	defaultHTTPTimeout = 12 * time.Second
	maxBatchPerRequest = 100
	// 节点连续失败多久之后才再去试它
	endpointCooldown = 30 * time.Second
)

// HTTPCaller 返回一个走 HTTP 的批量询价通道。urls 按优先级排列：
// 同一个时刻只用最靠前那个可用的，它出问题才换下一个（故障转移，不是轮流用）。
func HTTPCaller(urls []string, timeout time.Duration) BatchCaller {
	clean := make([]string, 0, len(urls))
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			clean = append(clean, u)
		}
	}
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	client := &http.Client{Timeout: timeout}
	var mu sync.Mutex
	var seq int64
	downUntil := map[string]time.Time{}

	nextID := func() int64 {
		mu.Lock()
		defer mu.Unlock()
		seq++
		return seq
	}
	ordered := func() []string {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		out := make([]string, 0, len(clean))
		for _, u := range clean {
			if until, ok := downUntil[u]; ok && now.Before(until) {
				continue
			}
			out = append(out, u)
		}
		if len(out) == 0 {
			// 全都在冷却里：那就还用第一个，总比什么都不做要好
			out = append(out, clean...)
		}
		return out
	}
	markFail := func(url string) {
		mu.Lock()
		defer mu.Unlock()
		downUntil[url] = time.Now().Add(endpointCooldown)
	}
	markOK := func(url string) {
		mu.Lock()
		defer mu.Unlock()
		delete(downUntil, url)
	}

	return func(ctx context.Context, calls []Call) ([]Result, error) {
		if len(calls) == 0 {
			return nil, nil
		}
		if len(clean) == 0 {
			return nil, errors.New("没有配置节点地址")
		}
		out := make([]Result, len(calls))
		for start := 0; start < len(calls); start += maxBatchPerRequest {
			end := start + maxBatchPerRequest
			if end > len(calls) {
				end = len(calls)
			}
			chunk := calls[start:end]
			reqs := make([]rpcRequest, len(chunk))
			for i, c := range chunk {
				reqs[i] = rpcRequest{
					JSONRPC: "2.0",
					ID:      nextID(),
					Method:  "eth_call",
					Params:  []any{map[string]any{"to": c.To, "data": hexData(c.Data)}, "latest"},
				}
			}
			res, err := postBatch(ctx, client, ordered(), markOK, markFail, reqs)
			if err != nil {
				return nil, err
			}
			for i := range res {
				out[start+i] = parseResult(res[i])
			}
		}
		return out, nil
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func postBatch(ctx context.Context, client *http.Client, urls []string,
	markOK func(string), markFail func(string), reqs []rpcRequest) ([]rpcResponse, error) {
	body, err := json.Marshal(reqs)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, url := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			markFail(url)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			markFail(url)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("节点返回 HTTP %d", resp.StatusCode)
			markFail(url)
			continue
		}
		out, err := decodeBatch(data, reqs)
		if err != nil {
			lastErr = err
			markFail(url)
			continue
		}
		markOK(url)
		return out, nil
	}
	return nil, fmt.Errorf("所有节点都连不上：%w", lastErr)
}

// decodeBatch 把一批响应按「请求里写的 id」认领回原顺序：
// 规范不保证回包顺序和请求顺序一致，按顺序硬对会张冠李戴，
// 把 A 交易对的价格当成 B 的。
func decodeBatch(data []byte, reqs []rpcRequest) ([]rpcResponse, error) {
	var batch []rpcResponse
	if err := json.Unmarshal(data, &batch); err != nil {
		var single rpcResponse
		if err2 := json.Unmarshal(data, &single); err2 != nil {
			return nil, errors.New("节点返回的不是 JSON-RPC 响应")
		}
		batch = []rpcResponse{single}
	}
	byID := make(map[int64]rpcResponse, len(batch))
	for _, r := range batch {
		byID[r.ID] = r
	}
	// 只问了一条也只回了一条时不必死抠 id：少数节点会把 id 写错或省略，
	// 这种情况没有歧义，认下来比直接失败更有用。
	if len(reqs) == 1 && len(batch) == 1 {
		return []rpcResponse{batch[0]}, nil
	}
	out := make([]rpcResponse, len(reqs))
	for i, req := range reqs {
		r, ok := byID[req.ID]
		if !ok {
			return nil, fmt.Errorf("节点漏回了第 %d 条请求", i+1)
		}
		out[i] = r
	}
	return out, nil
}

func parseResult(r rpcResponse) Result {
	if r.Error != nil {
		return Result{Err: &RPCError{Code: r.Error.Code, Message: r.Error.Message}}
	}
	var hexStr string
	if err := json.Unmarshal(r.Result, &hexStr); err != nil {
		if len(r.Result) == 0 || string(r.Result) == "null" {
			return Result{}
		}
		return Result{Err: errors.New("返回结果不是十六进制字符串")}
	}
	return Result{Data: decodeHex(hexStr)}
}

func hexData(b []byte) string {
	const digits = "0123456789abcdef"
	if len(b) == 0 {
		return "0x"
	}
	out := make([]byte, 2+len(b)*2)
	out[0] = '0'
	out[1] = 'x'
	for i, c := range b {
		out[2+i*2] = digits[c>>4]
		out[3+i*2] = digits[c&0x0f]
	}
	return string(out)
}

func decodeHex(s string) []byte {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if s == "" {
		return nil
	}
	if len(s)%2 != 0 {
		s = "0" + s
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		out[i] = hexPair(s[i*2], s[i*2+1])
	}
	return out
}

func hexPair(hi, lo byte) byte {
	return hexNibble(hi)<<4 | hexNibble(lo)
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}
