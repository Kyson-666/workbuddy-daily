package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// 基准值由 testdata/gen_fixture.js 用与 upstream/worker.js 相同的算法生成
func loadFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/fixture.json")
	if err != nil {
		t.Fatalf("读取基准值失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("基准值解析失败: %v", err)
	}
	return m
}

func TestAuthorizeURLMatchesUpstream(t *testing.T) {
	fx := loadFixture(t)
	want := fx["authorize_url"].(string)
	got := buildAuthorizeURL("trae", clientIDIDE, "abc123", "http://127.0.0.1:17388/authorize",
		"123456789012345", "deadbeefdeadbeef", "CHAL_xyz-1")
	if got != want {
		t.Fatalf("授权链接与上游不一致\n got: %s\nwant: %s", got, want)
	}
}

func TestEncodeURIComponentMatchesJS(t *testing.T) {
	fx := loadFixture(t)
	want := fx["encoded"].(string)
	got := encodeURIComponent("a b/中!*'()~._-:@&=+$,#?")
	if got != want {
		t.Fatalf("encodeURIComponent 与 JS 不一致\n got: %s\nwant: %s", got, want)
	}
}

func TestOpenRecordFromJS(t *testing.T) {
	fx := loadFixture(t)
	master, _ := base64.StdEncoding.DecodeString(fx["master"].(string))
	if len(master) != 32 {
		t.Fatalf("基准主密钥长度异常: %d", len(master))
	}
	var got struct {
		ID           string `json:"id"`
		Label        string `json:"label"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := openRecord(master, infoTrae, fx["sealed"].(string), &got); err != nil {
		t.Fatalf("解不开 JS 生成的记录: %v", err)
	}
	if got.ID != "x1" || got.Label != "测试" || got.RefreshToken != "RT-abc" {
		t.Fatalf("解密结果不对: %+v", got)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	master := randomBytes(32)
	in := map[string]any{"id": "a", "refreshToken": "rt", "n": float64(3)}
	sealed, err := sealRecord(master, infoWB, in)
	if err != nil {
		t.Fatalf("封存失败: %v", err)
	}
	var out map[string]any
	if err := openRecord(master, infoWB, sealed, &out); err != nil {
		t.Fatalf("解封失败: %v", err)
	}
	if out["refreshToken"] != "rt" {
		t.Fatalf("往返结果不一致: %+v", out)
	}
	// 用错密钥必须失败
	var dummy map[string]any
	if err := openRecord(randomBytes(32), infoWB, sealed, &dummy); err == nil {
		t.Fatal("错误密钥竟然解开了")
	}
}

func TestEcSignDERAndLowS(t *testing.T) {
	privPEM, pubPEM, err := genDeviceIdentity()
	if err != nil {
		t.Fatalf("生成设备密钥失败: %v", err)
	}
	data := []byte("POST\n/trae/api/v3/oauth/ExchangeToken\ncid\nrt\n1700000000\nabc")
	sigB64, err := ecSign(privPEM, data)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	der, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("签名不是 base64: %v", err)
	}
	block, _ := pem.Decode([]byte(pubPEM))
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("公钥解析失败: %v", err)
	}
	ecPub := pub.(*ecdsa.PublicKey)
	digest := sha256.Sum256(data)
	if !ecdsa.VerifyASN1(ecPub, digest[:], der) {
		t.Fatal("DER 签名验证失败")
	}
	var rs struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &rs); err != nil {
		t.Fatalf("签名不是合法 DER: %v", err)
	}
	half := new(big.Int).Rsh(ecPub.Curve.Params().N, 1)
	if rs.S.Cmp(half) > 0 {
		t.Fatal("s 没有做低值归一化")
	}
	// 篡改数据后必须验不过
	bad := sha256.Sum256(append(data, 'x'))
	if ecdsa.VerifyASN1(ecPub, bad[:], der) {
		t.Fatal("篡改数据后签名竟然通过了")
	}
}

func TestParseCallbackParams(t *testing.T) {
	raw := "http://127.0.0.1:17388/authorize?authCodeInfo=%7B%22AuthCode%22%3A%22AC-1%22%7D" +
		"&loginTraceID=trace-9&host=api.trae.cn&name=%E5%B0%8F%E6%98%8E"
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL 解析失败: %v", err)
	}
	p := parseCallbackParams(u)
	if p.AuthCode != "AC-1" || p.Trace != "trace-9" || p.Host != "api.trae.cn" || p.Name != "小明" {
		t.Fatalf("回调参数解析不对: %+v", p)
	}

	// 只有 code 的形式
	u2, _ := url.Parse("http://127.0.0.1:17388/authorize?code=AC-2&refreshToken=%22RT%22")
	p2 := parseCallbackParams(u2)
	if p2.AuthCode != "AC-2" || p2.RefreshToken != "RT" {
		t.Fatalf("code/refreshToken 解析不对: %+v", p2)
	}
}

func TestKVTTL(t *testing.T) {
	dir := t.TempDir()
	kv, err := NewKV(dir)
	if err != nil {
		t.Fatalf("KV 初始化失败: %v", err)
	}
	kv.Put("a:1", "hello", 0)
	if v, ok := kv.Get("a:1"); !ok || v != "hello" {
		t.Fatal("普通读写失败")
	}
	kv.Put("a:2", "temp", 50*time.Millisecond)
	if _, ok := kv.Get("a:2"); !ok {
		t.Fatal("TTL 未到期就取不到了")
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := kv.Get("a:2"); ok {
		t.Fatal("TTL 到期后还能取到")
	}
	kv.Delete("a:1")
	if _, ok := kv.Get("a:1"); ok {
		t.Fatal("删除后仍能取到")
	}
}

func TestJwtExp(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1893456000}`))
	tok := "eyJhbGciOiJub25lIn0." + payload + ".sig"
	if got := jwtExp(tok); got != 1893456000 {
		t.Fatalf("exp 解析错误: %d", got)
	}
	// 毫秒级 TokenExpireAt 要归一化成秒
	ms := base64.RawURLEncoding.EncodeToString([]byte(`{"TokenExpireAt":1893456000000}`))
	if got := jwtExp("a." + ms + ".c"); got != 1893456000 {
		t.Fatalf("毫秒 exp 归一化错误: %d", got)
	}
	if got := jwtExp("not-a-jwt"); got != 0 {
		t.Fatalf("非法 token 应返回 0，得到 %d", got)
	}
}

func TestPKCEPair(t *testing.T) {
	v, c := pkcePair()
	if len(v) != 64 || strings.TrimLeft(v, "0123456789abcdef") != "" {
		t.Fatalf("verifier 形态不对: %s", v)
	}
	sum := sha256.Sum256([]byte(v))
	if c != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("challenge 不是 base64url(SHA256(verifier))")
	}
}

func TestGenDeviceID(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := genDeviceID()
		if len(id) != 15 {
			t.Fatalf("设备号长度不对: %s", id)
		}
		if id[0] == '0' {
			t.Fatalf("设备号首位不应为 0: %s", id)
		}
		for _, c := range id {
			if c < '0' || c > '9' {
				t.Fatalf("设备号含非数字: %s", id)
			}
		}
	}
}
