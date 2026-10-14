package dexquote

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNode 是一个够真的假节点：支持批量、故意把回包顺序打乱、能按请求内容回不同的东西。
type fakeNode struct {
	srv     *httptest.Server
	calls   int64
	fail    atomic.Bool
	reorder bool
}

func newFakeNode() *fakeNode {
	n := &fakeNode{reorder: true}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n.calls, 1)
		if n.fail.Load() {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var reqs []map[string]json.RawMessage
		if err := json.Unmarshal(body, &reqs); err != nil {
			var single map[string]json.RawMessage
			if err2 := json.Unmarshal(body, &single); err2 != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			reqs = []map[string]json.RawMessage{single}
		}
		out := make([]map[string]any, 0, len(reqs))
		for _, req := range reqs {
			var params []json.RawMessage
			json.Unmarshal(req["params"], &params)
			var obj struct {
				To   string `json:"to"`
				Data string `json:"data"`
			}
			if len(params) > 0 {
				json.Unmarshal(params[0], &obj)
			}
			item := map[string]any{"jsonrpc": "2.0", "id": req["id"]}
			switch {
			case strings.Contains(strings.ToLower(obj.To), "dead"):
				item["error"] = map[string]any{"code": 3, "message": "execution reverted: INSUFFICIENT_LIQUIDITY"}
			case strings.Contains(strings.ToLower(obj.To), "down"):
				item["error"] = map[string]any{"code": -32000, "message": "server error"}
			default:
				// 用 to 的最后一位当返回值，方便断言「这一条对应哪一个请求」
				last := obj.To[len(obj.To)-1]
				item["result"] = "0x" + strings.Repeat("0", 62) + "0" + string(last)
			}
			out = append(out, item)
		}
		if n.reorder {
			for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
				out[i], out[j] = out[j], out[i]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if len(out) == 1 {
			json.NewEncoder(w).Encode(out[0])
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	return n
}

func (n *fakeNode) close() { n.srv.Close() }

func TestHTTPCallerBatchKeepsOrder(t *testing.T) {
	node := newFakeNode()
	defer node.close()
	caller := HTTPCaller([]string{node.srv.URL}, time.Second)

	res, err := caller(context.Background(), []Call{
		{To: "0x0000000000000000000000000000000000000001"},
		{To: "0x0000000000000000000000000000000000000002"},
		{To: "0x0000000000000000000000000000000000000003"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("回了 %d 条，期望 3 条", len(res))
	}
	for i, r := range res {
		if r.Err != nil {
			t.Fatalf("第 %d 条出错：%v", i+1, r.Err)
		}
		if len(r.Data) != 32 || r.Data[31] != byte(i+1) {
			t.Errorf("第 %d 条的结果对错了（回包顺序是打乱的，必须按 id 认领）：%x", i+1, r.Data)
		}
	}
	if atomic.LoadInt64(&node.calls) != 1 {
		t.Errorf("批量请求应该只走一次 HTTP，实际 %d 次", node.calls)
	}
}

func TestHTTPCallerPerCallErrors(t *testing.T) {
	node := newFakeNode()
	defer node.close()
	caller := HTTPCaller([]string{node.srv.URL}, time.Second)
	res, err := caller(context.Background(), []Call{
		{To: "0x0000000000000000000000000000000000000001"},
		{To: "0x000000000000000000000000000000000000dead"},
		{To: "0x00000000000000000000000000000000000000down"},
	})
	if err != nil {
		t.Fatalf("个别调用失败不该让整批失败：%v", err)
	}
	if res[0].Err != nil {
		t.Errorf("第一条不该出错：%v", res[0].Err)
	}
	if !IsRevert(res[1].Err) {
		t.Errorf("第二条应该是「合约拒绝执行」：%v", res[1].Err)
	}
	if res[2].Err == nil || IsRevert(res[2].Err) {
		t.Errorf("第二条之外的服务端错误不该被当成「没有池子」：%v", res[2].Err)
	}
}

func TestHTTPCallerFailover(t *testing.T) {
	bad := newFakeNode()
	bad.fail.Store(true)
	defer bad.close()
	good := newFakeNode()
	defer good.close()

	caller := HTTPCaller([]string{bad.srv.URL, good.srv.URL}, time.Second)
	res, err := caller(context.Background(), []Call{{To: "0x0000000000000000000000000000000000000007"}})
	if err != nil {
		t.Fatalf("坏节点后面的好节点应该顶上：%v", err)
	}
	if len(res) != 1 || res[0].Data[31] != 7 {
		t.Fatalf("结果不对：%+v", res)
	}
	// 冷却期内不该再往坏节点上撞
	before := atomic.LoadInt64(&bad.calls)
	if _, err := caller(context.Background(), []Call{{To: "0x0000000000000000000000000000000000000001"}}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&bad.calls) != before {
		t.Error("冷却期内的节点不该被再次尝试")
	}
}

func TestHTTPCallerAllDown(t *testing.T) {
	bad := newFakeNode()
	bad.fail.Store(true)
	defer bad.close()
	caller := HTTPCaller([]string{bad.srv.URL}, time.Second)
	if _, err := caller(context.Background(), []Call{{To: "0x1"}}); err == nil {
		t.Fatal("节点全不通时应该报错，而不是回一堆「没有池子」")
	}
	if _, err := HTTPCaller(nil, time.Second)(context.Background(), []Call{{To: "0x1"}}); err == nil {
		t.Fatal("没配节点时应该报错")
	}
	if res, err := HTTPCaller([]string{bad.srv.URL}, time.Second)(context.Background(), nil); err != nil || res != nil {
		t.Errorf("空请求应该直接返回空：%v %v", res, err)
	}
}

// 询价和取数要能串起来跑通：这条测试是「假节点 + 真询价」的完整回路。
func TestQuoterWithHTTPCaller(t *testing.T) {
	node := newFakeNode()
	defer node.close()
	// getAmountsOut 的返回值就是上面那些「末尾数字」的结果，不是合法数组，
	// 所以这里只验证「调用发出去了、结果被认领回来了」，不验证数值。
	q := New(HTTPCaller([]string{node.srv.URL}, time.Second))
	dex := Dex{ID: "d", Name: "D", Kind: KindV2, Router: "0x00000000000000000000000000000000000000a1"}
	quotes, err := q.QuoteAll(context.Background(), []Query{
		{Dex: dex, Hops: []Hop{{From: "0x0000000000000000000000000000000000000011", To: "0x0000000000000000000000000000000000000022"}}, AmountIn: bigFromInt(1000)},
	}, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(quotes) != 1 {
		t.Fatalf("应该回一条结果，实际 %d 条", len(quotes))
	}
	if quotes[0].Err == nil {
		t.Error("返回值不是合法的 uint256[]，应该如实报错而不是给个假数字")
	}
}

func TestHexHelpersRoundTrip(t *testing.T) {
	if got := hexData(nil); got != "0x" {
		t.Errorf("hexData(nil) = %q", got)
	}
	if got := hexData([]byte{0xde, 0xad, 0xbe, 0xef}); got != "0xdeadbeef" {
		t.Errorf("hexData = %q", got)
	}
	if got := decodeHex("0xDEADBEEF"); len(got) != 4 || got[0] != 0xde {
		t.Errorf("decodeHex 大小写不对: %x", got)
	}
	if got := decodeHex("0xf"); len(got) != 1 || got[0] != 0x0f {
		t.Errorf("奇数长度的十六进制串要补零: %x", got)
	}
	if got := decodeHex(""); got != nil {
		t.Errorf("空串应该回 nil: %x", got)
	}
}

func bigFromInt(n int64) *big.Int { return big.NewInt(n) }
