package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
)

func logf(format string, args ...any) {
	log.Printf(format, args...)
}

/* ---------------- 随机与编码 ---------------- */

func randomBytesStr(n int) string {
	return string(randomBytes(n))
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// randToken 等价于 worker.js 的 randomId(bytes)：base64url 无填充。
func randToken(n int) string { return b64url(randomBytes(n)) }

func randomHex(n int) string {
	const hex = "0123456789abcdef"
	buf := randomBytes(n)
	out := make([]byte, 0, n*2)
	for _, b := range buf {
		out = append(out, hex[b>>4], hex[b&0x0f])
	}
	return string(out)
}

// encodeURIComponent 与浏览器行为一致（Go 的 QueryEscape 把空格写成 + 且不转义 !'()*）。
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '!', c == '~', c == '*', c == '\'', c == '(', c == ')':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

/* ---------------- JSON 辅助：接口返回结构各版本不一致，按需宽松取值 ---------------- */

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// jget 取 map 字段，键不区分大小写地兜底一次（部分接口大小写不稳定）。
func jget(v any, key string) any {
	m := asMap(v)
	if m == nil {
		return nil
	}
	if x, ok := m[key]; ok {
		return x
	}
	for k, x := range m {
		if strings.EqualFold(k, key) {
			return x
		}
	}
	return nil
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func asInt64(v any) int64 {
	f, ok := asFloat(v)
	if !ok {
		return 0
	}
	return int64(f)
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// deepFindString 递归找第一个非空字符串字段，用于各版本响应里找 token。
func deepFindString(v any, keys []string, depth int) string {
	if depth > 6 || v == nil {
		return ""
	}
	if m := asMap(v); m != nil {
		for _, k := range keys {
			if s := strings.TrimSpace(asString(m[k])); s != "" {
				return s
			}
		}
		for _, k := range keys { // 大小写不一致时再兜一次
			for mk, mv := range m {
				if strings.EqualFold(mk, k) {
					if s := strings.TrimSpace(asString(mv)); s != "" {
						return s
					}
				}
			}
		}
		for _, mv := range m {
			if r := deepFindString(mv, keys, depth+1); r != "" {
				return r
			}
		}
		return ""
	}
	for _, mv := range asSlice(v) {
		if r := deepFindString(mv, keys, depth+1); r != "" {
			return r
		}
	}
	return ""
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
