package clientauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// ClientIdentity 是请求期只读调用方身份；只暴露稳定 KeyID，不含原始密钥。
type ClientIdentity struct {
	KeyID string
}

// KeyEntry 描述配置中的一个客户端 API Key（校验后视图）。
type KeyEntry struct {
	ID         string
	APIKey     string
	APIKeyHash string
	Enabled    bool
}

// Index 是密钥 digests 到身份的只读查找表。
// digest 只存在内存，不进入日志、DuckDB 或 Web API。
type Index struct {
	byDigest map[[32]byte]ClientIdentity
	// enabledDigests 与 disabledDigests 分离，便于 disabled 返回 401 而非 default。
	enabled  map[[32]byte]ClientIdentity
	disabled map[[32]byte]struct{}
}

// ErrAuthenticationFailed 表示客户端提供了无效、未知、禁用或冲突的凭据。
// 调用方应映射为 HTTP 401，且不得在响应中暴露密钥或候选 ID。
var ErrAuthenticationFailed = errors.New("authentication failed")

// BuildIndex 从配置条目构造只读索引。调用前配置层应已完成唯一性与格式校验。
func BuildIndex(entries []KeyEntry) *Index {
	idx := &Index{
		byDigest: make(map[[32]byte]ClientIdentity, len(entries)),
		enabled:  make(map[[32]byte]ClientIdentity, len(entries)),
		disabled: make(map[[32]byte]struct{}),
	}
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		var d [32]byte
		key := strings.TrimSpace(e.APIKey)
		if key != "" {
			d = sha256.Sum256([]byte(key))
		} else if !parseHash(e.APIKeyHash, &d) {
			continue
		}
		ident := ClientIdentity{KeyID: id}
		idx.byDigest[d] = ident
		if e.Enabled {
			idx.enabled[d] = ident
		} else {
			idx.disabled[d] = struct{}{}
		}
	}
	return idx
}

func parseHash(value string, target *[32]byte) bool {
	value = strings.TrimPrefix(strings.TrimSpace(value), "sha256:")
	if len(value) != 64 || target == nil {
		return false
	}
	_, err := hex.Decode(target[:], []byte(value))
	return err == nil
}

// ResolveHeaders 按方案 §8 从标准请求头解析身份。
// 不从 query、body、Cookie、model 名或自定义 provider 头读取。
func ResolveHeaders(header http.Header, idx *Index) (ClientIdentity, error) {
	if idx == nil {
		idx = BuildIndex(nil)
	}
	authRaw := strings.TrimSpace(header.Get("Authorization"))
	xAPIKey := strings.TrimSpace(header.Get("X-API-Key"))

	var creds []string
	if authRaw != "" {
		token, ok := parseBearer(authRaw)
		if !ok {
			// Authorization 存在但不是合法 Bearer 形式 → 无效凭据。
			return ClientIdentity{}, ErrAuthenticationFailed
		}
		if token != "" {
			creds = append(creds, token)
		}
	}
	if xAPIKey != "" {
		creds = append(creds, xAPIKey)
	}

	if len(creds) == 0 {
		return ClientIdentity{}, ErrAuthenticationFailed
	}

	// 两个 Header 同时存在且值不同 → 401。
	if len(creds) == 2 && subtle.ConstantTimeCompare([]byte(creds[0]), []byte(creds[1])) != 1 {
		return ClientIdentity{}, ErrAuthenticationFailed
	}
	// 同值或仅一个：只解析一次。
	return lookupKey(creds[0], idx)
}

func parseBearer(auth string) (token string, ok bool) {
	const prefix = "Bearer "
	if len(auth) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(auth[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(auth[len(prefix):]), true
}

func lookupKey(raw string, idx *Index) (ClientIdentity, error) {
	d := sha256.Sum256([]byte(raw))
	if _, disabled := idx.disabled[d]; disabled {
		return ClientIdentity{}, ErrAuthenticationFailed
	}
	if ident, ok := idx.enabled[d]; ok {
		return ident, nil
	}
	// 未知 Key。
	return ClientIdentity{}, ErrAuthenticationFailed
}

type identityContextKey struct{}

// WithClientIdentity 将已解析身份写入 request context。
func WithClientIdentity(ctx context.Context, identity ClientIdentity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// ClientIdentityFromContext 读取已认证身份。缺失身份表示调用方绕过了认证边界，
// 仅用于防御性降级；业务请求在进入此处前必须已完成 Header 认证。
func ClientIdentityFromContext(ctx context.Context) ClientIdentity {
	if ctx == nil {
		return ClientIdentity{}
	}
	if v, ok := ctx.Value(identityContextKey{}).(ClientIdentity); ok && v.KeyID != "" {
		return v
	}
	return ClientIdentity{}
}
