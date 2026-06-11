package metrics

import (
	"fmt"
	"sync"
	"time"
)

// SLOConfig 描述聚合层需要满足的服务等级目标。
// 任何字段保持零值时,对应检查不会触发 violation。
type SLOConfig struct {
	// CacheHitRateMin 是单个 provider 的最小缓存命中率(0~1)。低于阈值即告警。
	CacheHitRateMin float64
	// UpstreamErrorRateMax 是单个 provider 的上游错误率上限(0~1)。
	UpstreamErrorRateMax float64
	// P99LatencyMaxMS 是单 provider 的 p99 延迟上限(毫秒)。
	P99LatencyMaxMS float64
	// CheckInterval 控制后台巡检周期;<= 0 时禁用周期检查。
	CheckInterval time.Duration
}

// SLOViolation 描述一次命中 SLO 阈值的违规事件。
type SLOViolation struct {
	At        time.Time `json:"at"`
	Provider  string    `json:"provider"`
	Rule      string    `json:"rule"`
	Observed  float64   `json:"observed"`
	Threshold float64   `json:"threshold"`
	Detail    string    `json:"detail,omitempty"`
}

// SLOEvaluator 周期地根据 Registry 快照检查 SLO,产出 violation 事件。
// evaluator 自身协程安全,但 Close 后所有方法应停止使用。
type SLOEvaluator struct {
	registry *Registry
	config   SLOConfig
	webhook  string
	listener func(SLOViolation)

	mu         sync.Mutex
	violations []SLOViolation
	lastCheck  time.Time
}

// NewSLOEvaluator 构造 SLOEvaluator。webhook 为空时不发送远程通知;
// listener 为空时调用方只通过 Violations() 拉取事件。
func NewSLOEvaluator(reg *Registry, cfg SLOConfig, webhook string, listener func(SLOViolation)) *SLOEvaluator {
	return &SLOEvaluator{
		registry:   reg,
		config:     cfg,
		webhook:    webhook,
		listener:   listener,
		violations: nil,
	}
}

// Violations 返回自上次 Reset 以来累计的违规事件快照。
func (e *SLOEvaluator) Violations() []SLOViolation {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]SLOViolation, len(e.violations))
	copy(out, e.violations)
	return out
}

// Reset 清空累积的 violation 历史。
func (e *SLOEvaluator) Reset() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.violations = nil
	e.mu.Unlock()
}

// CheckNow 立即按当前 config 检查一次 Registry 状态,产出 violation 列表。
// 供测试与外部触发使用;周期巡检由 Run 自动调度。
func (e *SLOEvaluator) CheckNow() []SLOViolation {
	if e == nil || e.registry == nil {
		return nil
	}
	snap := e.registry.snapshotForSLO()
	violations := e.evaluate(snap)
	if len(violations) == 0 {
		return nil
	}
	e.record(violations)
	for _, v := range violations {
		if e.listener != nil {
			e.listener(v)
		}
	}
	return violations
}

// Run 启动后台巡检,直到 ctx 取消或返回 false。CheckInterval<=0 时立即返回。
func (e *SLOEvaluator) Run(ctx ContextLike) {
	if e == nil || e.config.CheckInterval <= 0 {
		return
	}
	ticker := time.NewTicker(e.config.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.CheckNow()
		}
	}
}

// evaluate 对快照逐条规则判定,产出违规事件。
func (e *SLOEvaluator) evaluate(snap sloSnapshot) []SLOViolation {
	var out []SLOViolation
	for provider, s := range snap.byProvider {
		if e.config.CacheHitRateMin > 0 {
			total := s.hits + s.misses
			if total >= 10 {
				rate := float64(s.hits) / float64(total)
				if rate < e.config.CacheHitRateMin {
					out = append(out, SLOViolation{
						At:        time.Now(),
						Provider:  provider,
						Rule:      "cache_hit_rate_min",
						Observed:  rate,
						Threshold: e.config.CacheHitRateMin,
						Detail:    fmt.Sprintf("hits=%d misses=%d", s.hits, s.misses),
					})
				}
			}
		}
		if e.config.UpstreamErrorRateMax > 0 {
			total := int64(s.requests)
			if total >= 10 {
				errRate := float64(s.errors) / float64(total)
				if errRate > e.config.UpstreamErrorRateMax {
					out = append(out, SLOViolation{
						At:        time.Now(),
						Provider:  provider,
						Rule:      "upstream_error_rate_max",
						Observed:  errRate,
						Threshold: e.config.UpstreamErrorRateMax,
						Detail:    fmt.Sprintf("errors=%d requests=%d", s.errors, total),
					})
				}
			}
		}
		if e.config.P99LatencyMaxMS > 0 && s.p99MS > e.config.P99LatencyMaxMS {
			out = append(out, SLOViolation{
				At:        time.Now(),
				Provider:  provider,
				Rule:      "p99_latency_max_ms",
				Observed:  s.p99MS,
				Threshold: e.config.P99LatencyMaxMS,
				Detail:    fmt.Sprintf("samples=%d", s.samples),
			})
		}
	}
	return out
}

func (e *SLOEvaluator) record(violations []SLOViolation) {
	e.mu.Lock()
	e.violations = append(e.violations, violations...)
	e.lastCheck = time.Now()
	e.mu.Unlock()
}

// Webhook 返回配置中的 webhook URL(可能为空)。
func (e *SLOEvaluator) Webhook() string {
	if e == nil {
		return ""
	}
	return e.webhook
}
