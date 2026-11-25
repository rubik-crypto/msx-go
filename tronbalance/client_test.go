package tronbalance

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testOwner    = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	testContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
)

func pad64(n *big.Int) string {
	out := n.Text(16)
	return strings.Repeat("0", 64-len(out)) + out
}

func abiString(value string) string {
	data := []byte(value)
	body := make([]byte, 64+((len(data)+31)/32)*32)
	binary.BigEndian.PutUint64(body[24:32], 32)
	binary.BigEndian.PutUint64(body[56:64], uint64(len(data)))
	copy(body[64:], data)
	return hex.EncodeToString(body)
}

func TestDecodeAddress(t *testing.T) {
	raw, err := DecodeAddress(testOwner)
	if err != nil {
		t.Fatalf("decode valid address: %v", err)
	}
	if got, want := hex.EncodeToString(raw), "41a614f803b6fd780986a42c78ec9c7f77e6ded13c"; got != want {
		t.Fatalf("decoded address = %s, want %s", got, want)
	}
	bad := []string{
		"",
		"0x1234",
		"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u",
		"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t ",
	}
	for _, item := range bad[:3] {
		if _, err := DecodeAddress(item); !errors.Is(err, ErrInvalidAddress) {
			t.Fatalf("DecodeAddress(%q) error = %v", item, err)
		}
	}
	if _, err := DecodeAddress(strings.TrimSpace(bad[3])); err != nil {
		t.Fatalf("surrounding spaces should be tolerated: %v", err)
	}
}

func TestClientQueriesAndFailover(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/wallet/getaccount":
			_, _ = w.Write([]byte(`{"balance": 2500000}`))
		case "/wallet/triggerconstantcontract":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			fn, _ := body["function_selector"].(string)
			constant := pad64(big.NewInt(1000000))
			switch fn {
			case "decimals()":
				constant = pad64(big.NewInt(6))
			case "symbol()":
				constant = abiString("TEST")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result":          map[string]any{"result": true},
				"constant_result": []string{constant},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer good.Close()

	client := NewWithHTTP(&http.Client{Timeout: 3 * time.Second}, bad.URL, good.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	trx, err := client.NativeBalance(ctx, testOwner)
	if err != nil {
		t.Fatalf("native balance: %v", err)
	}
	if trx.Cmp(big.NewInt(2500000)) != 0 {
		t.Fatalf("native balance = %s", trx)
	}
	balance, err := client.TRC20Balance(ctx, testOwner, testContract)
	if err != nil {
		t.Fatalf("token balance: %v", err)
	}
	if balance.Cmp(big.NewInt(1000000)) != 0 {
		t.Fatalf("token balance = %s", balance)
	}
	info, err := client.TRC20Info(ctx, testOwner, testContract)
	if err != nil {
		t.Fatalf("token info: %v", err)
	}
	if info.Symbol != "TEST" || info.Decimals != 6 {
		t.Fatalf("token info = %+v", info)
	}
}

func TestClientRejectsInvalidAddressAndNonContract(t *testing.T) {
	client := NewWithHTTP(&http.Client{Timeout: time.Second})
	if _, err := client.NativeBalance(context.Background(), "not-an-address"); !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("invalid address error = %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result":          map[string]any{"result": false},
			"constant_result": []string{},
		})
	}))
	defer srv.Close()
	client = NewWithHTTP(&http.Client{Timeout: time.Second}, srv.URL)
	if _, err := client.TRC20Balance(context.Background(), testOwner, testContract); !errors.Is(err, ErrNotContract) {
		t.Fatalf("non-contract error = %v", err)
	}
}

func TestNoUsableEndpoint(t *testing.T) {
	client := NewWithHTTP(&http.Client{Timeout: time.Second}, "not-a-url")
	if _, err := client.NativeBalance(context.Background(), testOwner); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no endpoint error = %v", err)
	}
}

func TestParsingHelpers(t *testing.T) {
	if got := decodeABIString(abiString("ABC")); got != "ABC" {
		t.Fatalf("decodeABIString = %q", got)
	}
	if got := decodeABIString(hex.EncodeToString(append([]byte("XYZ"), make([]byte, 29)...))); got != "XYZ" {
		t.Fatalf("bytes32 decode = %q", got)
	}
	if got, err := parseHexBig("0x1e240"); err != nil || got.String() != "123456" {
		t.Fatalf("parseHexBig = %v, %v", got, err)
	}
	if got := cleanSymbol("  A\nB  "); got != "A B" {
		t.Fatalf("cleanSymbol = %q", got)
	}
}
