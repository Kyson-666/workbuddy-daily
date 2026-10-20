package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// KV 用「一条记录一个文件」模拟 Cloudflare KV：
//   - 键任意字符串（含 ":"），文件名取 base64url(键)，避免路径穿越与非法字符
//   - 支持 TTL（登录会话 15 分钟）
//   - 值就是原样字符串，密文封存逻辑在 crypto.go，磁盘上不落明文令牌
//
// 单进程内用一把锁串行化写入，签到程序并发量极低，够用。
type KV struct {
	dir string
	mu  sync.RWMutex
}

type kvEntry struct {
	K string `json:"k"`
	V string `json:"v"`
	E int64  `json:"e,omitempty"` // 过期时间（unix 毫秒），0 = 不过期
}

func NewKV(dir string) (*KV, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &KV{dir: dir}, nil
}

func (kv *KV) path(key string) string {
	name := base64.RawURLEncoding.EncodeToString([]byte(key))
	return filepath.Join(kv.dir, name+".kv")
}

func (kv *KV) Get(key string) (string, bool) {
	kv.mu.RLock()
	p := kv.path(key)
	kv.mu.RUnlock()

	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	var e kvEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return "", false
	}
	if e.E > 0 && time.Now().UnixMilli() > e.E {
		_ = os.Remove(p)
		return "", false
	}
	return e.V, true
}

func (kv *KV) Put(key, val string, ttl time.Duration) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	e := kvEntry{K: key, V: val}
	if ttl > 0 {
		e.E = time.Now().Add(ttl).UnixMilli()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	p := kv.path(key)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (kv *KV) Delete(key string) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	err := os.Remove(kv.path(key))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// scopedKV 按前缀隔离两个平台，对应 worker.js 里的 kvScope。
type scopedKV struct {
	kv     *KV
	prefix string
}

func (s scopedKV) Get(k string) (string, bool) { return s.kv.Get(s.prefix + k) }
func (s scopedKV) Put(k, v string, ttl time.Duration) error {
	return s.kv.Put(s.prefix+k, v, ttl)
}
func (s scopedKV) Delete(k string) error { return s.kv.Delete(s.prefix + k) }

func (s scopedKV) List(prefix string) []string {
	entries, err := os.ReadDir(s.kv.dir)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".kv") {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSuffix(name, ".kv"))
		if err != nil {
			continue
		}
		key := string(raw)
		if !strings.HasPrefix(key, s.prefix+prefix) {
			continue
		}
		out = append(out, strings.TrimPrefix(key, s.prefix))
	}
	return out
}
