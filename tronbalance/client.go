package tronbalance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrInvalidAddress 表示地址不是合法的波场 base58 地址。
	ErrInvalidAddress = errors.New("invalid TRON address")
	// ErrNotContract 表示目标地址不是可调用的 TRC20 合约。
	ErrNotContract = errors.New("not a TRC20 contract")
	// ErrUnavailable 表示所有节点都没有返回可用结果。
	ErrUnavailable = errors.New("TRON endpoint unavailable")
)

// DefaultEndpoint 是未指定节点时使用的公开 TronGrid 接口。
const DefaultEndpoint = "https://api.trongrid.io"

// TokenInfo 是 TRC20 代币的基本信息。
type TokenInfo struct {
	Symbol   string
	Decimals int
}

// Client 是波场余额查询客户端，可以安全地并发使用。
type Client struct {
	endpoints []string
	http      *http.Client
}

// New 创建一个查询客户端。不传节点时使用 DefaultEndpoint；
// 传入多个节点时按顺序故障转移。只接受 http/https 地址。
func New(endpoints ...string) *Client {
	list := make([]string, 0, len(endpoints))
	provided := false
	for _, item := range endpoints {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		provided = true
		u, err := url.Parse(item)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		list = append(list, strings.TrimRight(item, "/"))
	}
	if !provided {
		list = []string{DefaultEndpoint}
	}
	return &Client{
		endpoints: list,
		http: &http.Client{
			Timeout: 25 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          8,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       60 * time.Second,
				TLSHandshakeTimeout:   8 * time.Second,
				ResponseHeaderTimeout: 12 * time.Second,
			},
		},
	}
}

// NewWithHTTP 用调用方提供的 HTTP 客户端创建查询客户端，适合接入自定义代理或测试。
// client 为 nil 时退回 New 的默认设置。
func NewWithHTTP(client *http.Client, endpoints ...string) *Client {
	c := New(endpoints...)
	if client != nil {
		c.http = client
	}
	return c
}

// NativeBalance 查询地址的 TRX 余额，单位是 sun（1 TRX = 1,000,000 sun）。
// 未激活的地址返回 0。
func (c *Client) NativeBalance(ctx context.Context, address string) (*big.Int, error) {
	address = strings.TrimSpace(address)
	if _, err := DecodeAddress(address); err != nil {
		return nil, err
	}
	var lastErr error
	for _, endpoint := range c.endpoints {
		var result struct {
			Balance json.Number `json:"balance"`
			Error   string      `json:"Error"`
		}
		err := c.postJSON(ctx, endpoint, "/wallet/getaccount", map[string]any{
			"address": address,
			"visible": true,
		}, &result)
		if err != nil {
			lastErr = err
			continue
		}
		if result.Error != "" {
			lastErr = errors.New(result.Error)
			continue
		}
		if result.Balance == "" {
			return big.NewInt(0), nil
		}
		value, ok := new(big.Int).SetString(result.Balance.String(), 10)
		if !ok {
			lastErr = errors.New("unexpected balance format")
			continue
		}
		return value, nil
	}
	return nil, wrapUnavailable(lastErr)
}

// TRC20Balance 查询 owner 在 contract 代币上的余额，单位是该代币的最小单位。
// contract 和 owner 都必须是波场 base58 地址。
func (c *Client) TRC20Balance(ctx context.Context, owner, contract string) (*big.Int, error) {
	owner = strings.TrimSpace(owner)
	contract = strings.TrimSpace(contract)
	ownerBytes, err := DecodeAddress(owner)
	if err != nil {
		return nil, err
	}
	if _, err := DecodeAddress(contract); err != nil {
		return nil, err
	}
	param := strings.Repeat("0", 24) + hex.EncodeToString(ownerBytes[1:])
	payload := map[string]any{
		"owner_address":     owner,
		"contract_address":  contract,
		"function_selector": "balanceOf(address)",
		"parameter":         param,
		"visible":           true,
	}
	var lastErr error
	for _, endpoint := range c.endpoints {
		result, err := c.trigger(ctx, endpoint, payload)
		if err != nil {
			lastErr = err
			continue
		}
		if len(result.ConstantResult) == 0 {
			if result.Result.Result {
				return big.NewInt(0), nil
			}
			lastErr = ErrNotContract
			continue
		}
		value, err := parseHexBig(result.ConstantResult[0])
		if err != nil {
			lastErr = err
			continue
		}
		return value, nil
	}
	if errors.Is(lastErr, ErrNotContract) {
		return nil, ErrNotContract
	}
	return nil, wrapUnavailable(lastErr)
}

// TRC20Info 读取代币符号与小数位。owner 用于发起常量合约调用，不会产生交易。
// 符号读取失败时返回空字符串，但精度读取成功仍返回结果。
func (c *Client) TRC20Info(ctx context.Context, owner, contract string) (TokenInfo, error) {
	owner = strings.TrimSpace(owner)
	contract = strings.TrimSpace(contract)
	if _, err := DecodeAddress(owner); err != nil {
		return TokenInfo{}, err
	}
	if _, err := DecodeAddress(contract); err != nil {
		return TokenInfo{}, err
	}
	var lastErr error
	for _, endpoint := range c.endpoints {
		decResult, err := c.trigger(ctx, endpoint, map[string]any{
			"owner_address":     owner,
			"contract_address":  contract,
			"function_selector": "decimals()",
			"parameter":         "",
			"visible":           true,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if len(decResult.ConstantResult) == 0 || !decResult.Result.Result {
			lastErr = ErrNotContract
			continue
		}
		if strings.TrimSpace(decResult.ConstantResult[0]) == "" {
			lastErr = errors.New("empty token decimals")
			continue
		}
		decimals, err := parseHexUint(decResult.ConstantResult[0])
		if err != nil || decimals > 36 {
			lastErr = errors.New("unexpected token decimals")
			continue
		}
		info := TokenInfo{Decimals: int(decimals)}
		symResult, symErr := c.trigger(ctx, endpoint, map[string]any{
			"owner_address":     owner,
			"contract_address":  contract,
			"function_selector": "symbol()",
			"parameter":         "",
			"visible":           true,
		})
		if symErr == nil && len(symResult.ConstantResult) > 0 {
			info.Symbol = cleanSymbol(decodeABIString(symResult.ConstantResult[0]))
		}
		return info, nil
	}
	if errors.Is(lastErr, ErrNotContract) {
		return TokenInfo{}, ErrNotContract
	}
	return TokenInfo{}, wrapUnavailable(lastErr)
}

type triggerResponse struct {
	Result         callResult `json:"result"`
	ConstantResult []string   `json:"constant_result"`
	Error          string     `json:"Error"`
}

type callResult struct {
	Result  bool   `json:"result"`
	Message string `json:"message"`
}

func (c *Client) trigger(ctx context.Context, endpoint string, payload map[string]any) (*triggerResponse, error) {
	var result triggerResponse
	if err := c.postJSON(ctx, endpoint, "/wallet/triggerconstantcontract", payload, &result); err != nil {
		return nil, err
	}
	if result.Error != "" {
		return nil, errors.New(result.Error)
	}
	return &result, nil
}

func (c *Client) postJSON(ctx context.Context, endpoint, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, appendAPIPath(endpoint, path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

func appendAPIPath(endpoint, apiPath string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return strings.TrimRight(endpoint, "/") + apiPath
	}
	u.Path = strings.TrimRight(u.Path, "/") + apiPath
	return u.String()
}

func wrapUnavailable(lastErr error) error {
	if lastErr == nil {
		return ErrUnavailable
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, lastErr)
}

// DecodeAddress 解码并校验波场 base58 地址，返回包含 0x41 前缀的 21 字节地址。
func DecodeAddress(address string) ([]byte, error) {
	decoded, err := base58Decode(strings.TrimSpace(address))
	if err != nil || len(decoded) != 25 {
		return nil, ErrInvalidAddress
	}
	if decoded[0] != 0x41 {
		return nil, ErrInvalidAddress
	}
	first := sha256.Sum256(decoded[:21])
	second := sha256.Sum256(first[:])
	if subtle.ConstantTimeCompare(decoded[21:25], second[:4]) != 1 {
		return nil, ErrInvalidAddress
	}
	return decoded[:21], nil
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Decode(s string) ([]byte, error) {
	if s == "" {
		return nil, ErrInvalidAddress
	}
	n := new(big.Int)
	base := big.NewInt(58)
	for _, r := range s {
		idx := strings.IndexRune(base58Alphabet, r)
		if idx < 0 {
			return nil, ErrInvalidAddress
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(idx)))
	}
	decoded := n.Bytes()
	leading := 0
	for leading < len(s) && s[leading] == '1' {
		leading++
	}
	if leading > 0 {
		decoded = append(make([]byte, leading), decoded...)
	}
	return decoded, nil
}

func parseHexBig(value string) (*big.Int, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "0x")
	value = strings.TrimPrefix(value, "0X")
	if value == "" {
		return big.NewInt(0), nil
	}
	if len(value)%2 != 0 {
		value = "0" + value
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(raw), nil
}

func parseHexUint(value string) (uint64, error) {
	n, err := parseHexBig(value)
	if err != nil {
		return 0, err
	}
	if !n.IsUint64() {
		return 0, errors.New("number out of range")
	}
	return n.Uint64(), nil
}

func decodeABIString(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "0x")
	value = strings.TrimPrefix(value, "0X")
	if value == "" || len(value)%2 != 0 {
		return ""
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) == 0 {
		return ""
	}
	if len(raw) == 32 {
		return trimAtNUL(raw)
	}
	if len(raw) >= 64 {
		offsetBig := new(big.Int).SetBytes(raw[:32])
		if offsetBig.IsUint64() {
			offset := int(offsetBig.Uint64())
			if offset >= 0 && offset+32 <= len(raw) {
				lengthBig := new(big.Int).SetBytes(raw[offset : offset+32])
				if lengthBig.IsUint64() {
					length := int(lengthBig.Uint64())
					start := offset + 32
					if length >= 0 && start+length <= len(raw) {
						return trimAtNUL(raw[start : start+length])
					}
				}
			}
		}
	}
	return trimAtNUL(raw)
}

func trimAtNUL(raw []byte) string {
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	if len(raw) > 64 {
		raw = raw[:64]
	}
	return string(raw)
}

func cleanSymbol(symbol string) string {
	symbol = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, symbol)
	symbol = strings.Join(strings.Fields(symbol), " ")
	if utf8.RuneCountInString(symbol) > 32 {
		symbol = string([]rune(symbol)[:32])
	}
	return symbol
}
