package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// Fingerprint 算法常量。stablePrefixBytes 是输入截取长度。
const stablePrefixBytes = 256

// ComputeRequestFingerprint 基于请求体计算 stable prefix hash 与整体 fingerprint。
// 返回值 hex 编码,长度 64(对应 32 字节 SHA-256)。
//
// 算法:对 system 段(如有) + 拼接 messages 段前 stablePrefixBytes 字节进行 SHA-256。
// 整体 fingerprint 则对完整 body 字节再算一次,用于跨 run 完整比对。
func ComputeRequestFingerprint(body []byte) (stablePrefixHash string, requestFingerprint string) {
	if len(body) == 0 {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:]), hex.EncodeToString(sum[:])
	}
	fullSum := sha256.Sum256(body)
	requestFingerprint = hex.EncodeToString(fullSum[:])

	stable := extractStablePrefix(body)
	prefixSum := sha256.Sum256(stable)
	stablePrefixHash = hex.EncodeToString(prefixSum[:])
	return
}

// extractStablePrefix 从 JSON 请求体里提取 system 段 + messages 段前 N 字节。
// 截断按字节边界进行,确保两个 body 共享前 N 字节时得到相同的 prefix。
// 对非 JSON 输入或缺字段的情况退化为原始字节的前 N 字节。
func extractStablePrefix(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return firstNBytes(body, stablePrefixBytes)
	}
	var segments [][]byte
	if system, ok := payload["system"].(string); ok && system != "" {
		segments = append(segments, []byte("system:"+system+"\n"))
	}
	if messages, ok := payload["messages"].([]any); ok {
		for _, msg := range messages {
			entry, _ := json.Marshal(msg)
			segments = append(segments, entry)
			segments = append(segments, []byte{'\n'})
		}
	}
	if len(segments) == 0 {
		return firstNBytes(body, stablePrefixBytes)
	}
	return takePrefixBytes(segments, stablePrefixBytes)
}

// takePrefixBytes 把 segments 顺序拼接,直到累计长度达到 n 字节。
// 若总长不足 n 则返回全部内容。
func takePrefixBytes(segments [][]byte, n int) []byte {
	out := make([]byte, 0, n)
	for _, seg := range segments {
		remaining := n - len(out)
		if remaining <= 0 {
			break
		}
		if len(seg) <= remaining {
			out = append(out, seg...)
			continue
		}
		out = append(out, seg[:remaining]...)
		break
	}
	return out
}

func firstNBytes(b []byte, n int) []byte {
	if len(b) <= n {
		out := make([]byte, len(b))
		copy(out, b)
		return out
	}
	out := make([]byte, n)
	copy(out, b[:n])
	return out
}

// FingerprintDriftTracker 跟踪最近 N 次请求的 stable prefix hash,
// 命中连续漂移时记一个 stable_prefix_drift 事件。
//
// 注意:这是进程级全局序列检测,跨并发请求与不同 model 混合观察;
// 语义是“近期请求 stable prefix 是否连续变化”,而非单会话/单 model 漂移。
// 并发安全:所有可变状态由 mu 保护。
type FingerprintDriftTracker struct {
	mu        sync.Mutex
	threshold int
	prev      string
	consec    int
}

// NewFingerprintDriftTracker 构造漂移检测器,threshold=连续不同 hash 次数阈值。
// 0 或负值时退化为不检测(永远不返回 true)。
func NewFingerprintDriftTracker(threshold int) *FingerprintDriftTracker {
	if threshold < 1 {
		threshold = 1
	}
	return &FingerprintDriftTracker{threshold: threshold}
}

// Observe 记录一次新的 stable prefix hash,若与上次不同且连续达到阈值返回 true。
// prev 为空时仅记录,不返回 true(首次建立基线)。
func (d *FingerprintDriftTracker) Observe(hash string) (drift bool, consecutiveDifferent int) {
	if d == nil {
		return false, 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.prev == "" {
		d.prev = hash
		return false, 0
	}
	if d.prev == hash {
		d.consec = 0
		return false, 0
	}
	d.consec++
	if d.consec >= d.threshold {
		return true, d.consec
	}
	return false, d.consec
}

// Reset 清空漂移跟踪状态。prev 重新置为空。
func (d *FingerprintDriftTracker) Reset() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prev = ""
	d.consec = 0
}
