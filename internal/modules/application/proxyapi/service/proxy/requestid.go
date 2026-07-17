package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// RequestIDHeader 是 HTTP 头中透传 / 注入 request id 的字段名。
const RequestIDHeader = "X-Request-ID"

// requestIDKey 是 context 中 request id 的私有 key,避免与外部包冲突。
type requestIDKey struct{}

// usageEventKey 与可由客户端透传的 RequestID 分离。用量事件主键必须由
// 服务端生成，避免重复的 X-Request-ID 造成用量写入冲突。
type usageEventKey struct{}

// newRequestID 生成 16 字节随机 ID,返回 32 字符的 hex 字符串。
func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 极端情况下 rand 失败,返回空串;调用方会跳过注入逻辑。
		return ""
	}
	return hex.EncodeToString(buf[:])
}

// withRequestID 把 request id 注入到 ctx。id 为空时直接返回原 ctx。
func withRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// requestIDFromContext 从 ctx 读取 request id,缺失或 ctx 为 nil 时返回空串。
func requestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

func withUsageEventID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, usageEventKey{}, id)
}

func usageEventIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(usageEventKey{}).(string)
	return id
}

// ensureRequestID 优先从入站请求头透传,缺失时生成新的 ID。
// 返回值始终非空(只要 crypto/rand 正常)。
func ensureRequestID(r *http.Request) string {
	if r == nil {
		return newRequestID()
	}
	if existing := strings.TrimSpace(r.Header.Get(RequestIDHeader)); existing != "" {
		return existing
	}
	return newRequestID()
}

// attachRequestID 把 ID 写到响应头,并把 *http.Request 替换为携带 ID 的 context 版本。
// 调用方必须使用返回的 *http.Request 继续处理。
func attachRequestID(w http.ResponseWriter, r *http.Request, id string) *http.Request {
	if w != nil && id != "" {
		w.Header().Set(RequestIDHeader, id)
	}
	if r == nil || id == "" {
		return r
	}
	return r.WithContext(withRequestID(r.Context(), id))
}
