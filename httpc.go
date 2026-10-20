package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type httpResult struct {
	Status int
	JSON   any
	Text   string
}

// doJSON 发一次 JSON 请求并把响应尽量解析成 JSON（解析失败时把原文留下给调用方）。
func doJSON(ctx context.Context, method, url string, headers map[string]string, body any, timeout time.Duration) (httpResult, error) {
	var rd io.Reader
	if body != nil && method != http.MethodGet {
		b, err := json.Marshal(body)
		if err != nil {
			return httpResult{}, err
		}
		rd = bytes.NewReader(b)
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, method, url, rd)
	if err != nil {
		return httpResult{}, err
	}
	if body != nil && method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if v == "" {
			// 上游有些接口要求「存在但为空」的头（如 x-cloudide-token: ""），
			// 这里显式写空值，避免被 net/http 当成删除。
			req.Header[k] = []string{""}
			continue
		}
		req.Header.Set(k, v)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return httpResult{}, err
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return httpResult{}, err
	}
	out := httpResult{Status: res.StatusCode, Text: string(raw)}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err == nil {
		out.JSON = v
	}
	return out, nil
}

var httpClient = &http.Client{
	Timeout: 0, // 由每次请求的 context 控制
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
	},
}
