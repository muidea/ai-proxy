package metrics

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// Generation 标识该 provider|rule 的违规生命周期:entered 时分配,resolved 沿用同一代;
// 重投对账时必须匹配,防止上一生命周期的 entered 在再次违规后被误发。
// EventID 为单条状态变化的稳定幂等键(instance|provider|rule|generation|state),
// 对账裁剪 batch 后仍保持不变。
type SLOViolation struct {
	At         time.Time `json:"at"`
	Provider   string    `json:"provider"`
	Rule       string    `json:"rule"`
	Observed   float64   `json:"observed"`
	Threshold  float64   `json:"threshold"`
	Detail     string    `json:"detail,omitempty"`
	Generation uint64    `json:"generation,omitempty"`
	EventID    string    `json:"event_id,omitempty"`
}

// SLO 状态变化常量:仅 entered / resolved 会通知 listener 与 webhook。
const (
	SLOStateEntered  = "entered"
	SLOStateResolved = "resolved"
)

// SLOStateChange 是本地 listener 收到的状态变化事件。
type SLOStateChange struct {
	State     string // entered | resolved
	Violation SLOViolation
}

// SLOWebhookPayload 是状态变化时 POST 的批量载荷。
// entered: 本轮新进入 violation; resolved: 本轮恢复正常。
//
// 序号语义(重要):
//   - InstanceID: 进程/evaluator 启动时随机生成,重启后变化;消费者仅在同一 instance 内比较 seq;
//   - Seq: 实际投递序号(本 instance 内每次(重)入队递增),用于拒绝倒序投递;
//   - 每条 violation.EventID: 单条状态变化幂等键 instance|provider|rule|generation|state,
//     对账裁剪 batch 后各条 ID 仍保持不变(batch 本身不是幂等键)。
//
// 消费者应按 violation.event_id 做幂等去重;按 (instance_id, seq) 拒绝倒序。
type SLOWebhookPayload struct {
	At         time.Time      `json:"at"`
	InstanceID string         `json:"instance_id"`
	Seq        uint64         `json:"seq"`
	Entered    []SLOViolation `json:"entered,omitempty"`
	Resolved   []SLOViolation `json:"resolved,omitempty"`
}

// webhookJob 是投递层内部单元:与 active 解耦,失败可延期重投。
type webhookJob struct {
	Payload   SLOWebhookPayload
	Attempts  int
	NextRetry time.Time
}

// maxViolationHistory 限制内存中保留的 violation 条数,防止持续故障无限增长。
const maxViolationHistory = 256

// webhook 投递参数。
// 状态变化 webhook 使用单 worker,保证 entered/resolved 完成顺序与入队顺序一致。
// 单次 attempt 失败后写入 undelivered+NextRetry,不在 worker 内长 sleep,避免阻塞后续状态。
const (
	webhookTimeout       = 3 * time.Second
	webhookBodyLimit     = 64 << 10 // 64KiB,仅用于丢弃响应
	webhookMaxConcurrent = 1
	webhookQueueSize     = 64
	webhookMaxAttempts   = 3
	webhookRetryBase     = 100 * time.Millisecond
	webhookRetryAfterMax = 30 * time.Second
	maxUndelivered       = 32
)

// SLOEvaluator 周期地根据 Registry 快照检查 SLO,产出 violation 事件。
// evaluator 自身协程安全,但 Close 后所有方法应停止使用。
//
// 状态分层:
//   - active + generation: 评估层当前违规与生命周期代次;
//   - webhook 队列 / undelivered: 投递层;失败可延期重投,与 active 解耦;
//   - 投递前 reconcile: 按 generation 丢弃过期 entered/resolved,避免跨生命周期重放。
//
// CheckNow 经 checkMu 串行,保证 listener 与 seq 分配顺序与状态变化一致。
// InstanceID 在构造时随机生成:event_id/seq/generation 仅在同一 instance 内有意义。
type SLOEvaluator struct {
	registry   *Registry
	config     SLOConfig
	webhook    string
	listener   func(SLOStateChange)
	instanceID string

	// checkMu 串行化完整 CheckNow(评估→listener→flush→enqueue),避免并发 CheckNow 乱序。
	checkMu sync.Mutex

	mu         sync.Mutex
	violations []SLOViolation
	lastCheck  time.Time
	// active 记录当前持续中的 violation,key=provider|rule。
	active map[string]SLOViolation
	// gen 为每个 key 最近一次 entered 的 generation;resolved 后仍保留,供对账匹配。
	// 再次 entered 时递增。
	gen map[string]uint64
	// undelivered 是投递失败后待重投的 job(与 active 独立;不重放 listener)。
	undelivered []webhookJob
	// seq 为实际投递序号(每次入队/重投入队递增,仅本 instance 内有效)。
	seq uint64
	// genCounter 全局 generation 分配器(仅本 instance 内有效)。
	genCounter uint64

	client        *http.Client
	webhookCh     chan webhookJob
	webhookWG     sync.WaitGroup
	webhookStop   chan struct{}
	webhookCtx    context.Context
	webhookCancel context.CancelFunc

	dropped         atomic.Uint64
	webhookOK       atomic.Uint64
	webhookError    atomic.Uint64
	webhookNon2xx   atomic.Uint64
	webhookCanceled atomic.Uint64
	started         atomic.Bool
}

// NewSLOEvaluator 构造 SLOEvaluator。webhook 为空时不发送远程通知;
// listener 为空时调用方只通过 Violations() 拉取事件。
//
// listener 约束(重要):
//   - 在 checkMu 持有期间同步回调,以保证与状态变化/seq 分配同序;
//   - 禁止在 listener 内直接或间接调用 CheckNow / Run,否则会自死锁;
//   - 禁止在 listener 内等待另一个需要执行 CheckNow 的 goroutine。
//
// Webhook 语义:
//   - active 状态变化时通知(entered/resolved),持续违规不重复投递;
//   - CheckNow 串行;单 worker 串行投递;
//   - payload 含 InstanceID + Seq;每条 violation 含 EventID + Generation;
//   - InstanceID 启动时随机生成,重启后变化;seq/generation 仅在同一 instance 内有序;
//   - EventID=instance|provider|rule|generation|state,跨重试/对账裁剪保持不变;
//   - 对账后重投只递增 Seq(delivery),不复用 batch 级幂等键;
//   - 网络/408/425/429/5xx 可重试;429 优先 Retry-After(秒或 HTTP-date,上限 30s);
//   - 失败不在 worker 内长 sleep,写入 undelivered,下轮 CheckNow flush;
//   - Close 取消在途 HTTP,剩余队列与 undelivered 计入 dropped 并清空;
//   - 禁用自动重定向;日志脱敏。
func NewSLOEvaluator(reg *Registry, cfg SLOConfig, webhook string, listener func(SLOStateChange)) *SLOEvaluator {
	return newSLOEvaluator(reg, cfg, webhook, listener, nil)
}

// instanceIDFallbackSeq 在 crypto/rand 失败时保证同纳秒内仍可区分多个 ID。
var instanceIDFallbackSeq atomic.Uint64

// newInstanceID 生成启动唯一标识:16 字节 → 32 个十六进制字符(128 bit)。
// 优先 crypto/rand;失败时用时间戳+PID+原子计数器填充 16 字节,格式契约不变。
func newInstanceID() string {
	return newInstanceIDFrom(rand.Read)
}

// newInstanceIDFrom 允许测试注入随机源(失败/短读路径)。
// read 签名与 rand.Read 一致:填充 b 并返回 (n, err)。
// 仅当 err==nil 且完整读取 len(b) 字节时采用随机结果;短读会走 fallback,避免低熵零填充 ID。
func newInstanceIDFrom(read func([]byte) (int, error)) string {
	var b [16]byte
	if n, err := read(b[:]); err == nil && n == len(b) {
		return hex.EncodeToString(b[:])
	}
	// fallback:仍输出 32 hex 字符,避免消费者按固定长度校验失败。
	// 布局: [0:8] unix nano big-endian, [8:12] pid, [12:16] 原子计数器。
	nano := time.Now().UnixNano()
	binary.BigEndian.PutUint64(b[0:8], uint64(nano))
	binary.BigEndian.PutUint32(b[8:12], uint32(os.Getpid()))
	binary.BigEndian.PutUint32(b[12:16], uint32(instanceIDFallbackSeq.Add(1)))
	return hex.EncodeToString(b[:])
}

// violationEventID 构造单条状态变化的稳定幂等键。
func violationEventID(instanceID, provider, rule string, generation uint64, state string) string {
	return instanceID + "|" + provider + "|" + rule + "|" + strconv.FormatUint(generation, 10) + "|" + state
}

// newSLOEvaluator 允许测试注入 HTTP client(例如阻塞 RoundTripper)。
func newSLOEvaluator(reg *Registry, cfg SLOConfig, webhook string, listener func(SLOStateChange), client *http.Client) *SLOEvaluator {
	ctx, cancel := context.WithCancel(context.Background())
	e := &SLOEvaluator{
		registry:      reg,
		config:        cfg,
		webhook:       strings.TrimSpace(webhook),
		listener:      listener,
		instanceID:    newInstanceID(),
		violations:    nil,
		active:        map[string]SLOViolation{},
		gen:           map[string]uint64{},
		webhookStop:   make(chan struct{}),
		webhookCtx:    ctx,
		webhookCancel: cancel,
	}
	if client != nil {
		e.client = client
	} else {
		e.client = &http.Client{
			Timeout: webhookTimeout,
			// Webhook 必须使用最终 URL;禁止跟随重定向。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if e.webhook != "" {
		e.webhookCh = make(chan webhookJob, webhookQueueSize)
		for range webhookMaxConcurrent {
			e.webhookWG.Add(1)
			go e.webhookWorker()
		}
		e.started.Store(true)
	}
	return e
}

// Close 停止 webhook worker:取消在途请求;丢弃并计数剩余队列与 undelivered。
// 在途 HTTP 绑定 webhookCtx,cancel 后应快速返回;Wait 上界约等于单次 webhookTimeout
// (仅当 transport 忽略 context 时才会逼近该上界)。
func (e *SLOEvaluator) Close() {
	if e == nil {
		return
	}
	if e.webhookCancel != nil {
		e.webhookCancel()
	}
	if !e.started.Swap(false) {
		// 无 worker 时仍取消 ctx,并清理可能残留的 undelivered。
		e.discardPending("evaluator closed")
		return
	}
	close(e.webhookStop)
	// 不关闭 webhookCh,避免 enqueue 竞态;worker 见 stop 后直接退出。
	e.webhookWG.Wait()
	e.discardPending("shutdown")
}

// discardPending 把 webhook 队列与 undelivered 中剩余批次计入 dropped 并清空。
// Close 后调用,保证 queue_length==0 且 dropped 语义完整。
func (e *SLOEvaluator) discardPending(reason string) {
	if e == nil {
		return
	}
	n := 0
	if e.webhookCh != nil {
		for {
			select {
			case <-e.webhookCh:
				n++
			default:
				goto drained
			}
		}
	}
drained:
	e.mu.Lock()
	n += len(e.undelivered)
	e.undelivered = nil
	e.mu.Unlock()
	if n == 0 {
		return
	}
	e.dropped.Add(uint64(n))
	slog.Warn("slo webhook discarded pending",
		slog.String("reason", reason),
		slog.Int("discarded", n),
		slog.Uint64("dropped_total", e.dropped.Load()),
	)
}

// WebhookDropped 返回因队列满、undelivered 溢出、过期丢弃或 Close 丢弃而计入的批次数。
func (e *SLOEvaluator) WebhookDropped() uint64 {
	if e == nil {
		return 0
	}
	return e.dropped.Load()
}

// WebhookQueueLength 返回待发送 backlog:内存队列 + 待重投 undelivered。
func (e *SLOEvaluator) WebhookQueueLength() int {
	if e == nil {
		return 0
	}
	n := 0
	if e.webhookCh != nil {
		n += len(e.webhookCh)
	}
	e.mu.Lock()
	n += len(e.undelivered)
	e.mu.Unlock()
	return n
}

// WebhookRequestCount 返回指定 result 的投递计数:ok|error|non_2xx|canceled。
func (e *SLOEvaluator) WebhookRequestCount(result string) uint64 {
	if e == nil {
		return 0
	}
	switch result {
	case "ok":
		return e.webhookOK.Load()
	case "error":
		return e.webhookError.Load()
	case "non_2xx":
		return e.webhookNon2xx.Load()
	case "canceled":
		return e.webhookCanceled.Load()
	default:
		return 0
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

// Reset 清空累积的 violation 历史(不影响 active 状态机)。
func (e *SLOEvaluator) Reset() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.violations = nil
	e.mu.Unlock()
}

// Active 返回当前持续中的 violation 快照。
func (e *SLOEvaluator) Active() []SLOViolation {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]SLOViolation, 0, len(e.active))
	for _, v := range e.active {
		out = append(out, v)
	}
	return out
}

// CheckNow 立即按当前 config 检查一次 Registry 状态。
// 返回本轮新进入的 violation 列表;持续违规不重复出现在返回值中。
// 状态变化时回调 listener(SLOStateChange),并异步 webhook:{entered,resolved}。
// 经 checkMu 串行完整路径(评估→listener→flush→enqueue),保证顺序。
// 先 flush 到期 undelivered,再入队本轮变化。
func (e *SLOEvaluator) CheckNow() []SLOViolation {
	if e == nil || e.registry == nil {
		return nil
	}
	e.checkMu.Lock()
	defer e.checkMu.Unlock()

	snap := e.registry.snapshotForSLO()
	current := e.evaluate(snap)
	entered, resolved := e.applyState(current)
	if len(entered) > 0 {
		e.record(entered)
		for _, v := range entered {
			if e.listener != nil {
				e.listener(SLOStateChange{State: SLOStateEntered, Violation: v})
			}
		}
	}
	for _, v := range resolved {
		if e.listener != nil {
			e.listener(SLOStateChange{State: SLOStateResolved, Violation: v})
		}
	}
	// 先重投到期失败批次(generation 对账后),再入队本轮状态。
	e.flushUndelivered()
	if len(entered) > 0 || len(resolved) > 0 {
		e.enqueueWebhook(e.newPayload(entered, resolved))
	}
	return entered
}

// Run 启动后台巡检,直到 ctx 取消。CheckInterval<=0 时立即返回。
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

func violationKey(v SLOViolation) string {
	return v.Provider + "|" + v.Rule
}

// applyState 对比 active 与本轮结果,返回 entered/resolved,并更新 active。
// 新 entered 分配递增 Generation 与稳定 EventID;resolved 沿用该代 Generation 并生成 resolved EventID。
func (e *SLOEvaluator) applyState(current []SLOViolation) (entered, resolved []SLOViolation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastCheck = time.Now()
	curMap := make(map[string]SLOViolation, len(current))
	for _, v := range current {
		curMap[violationKey(v)] = v
	}
	// entered
	for k, v := range curMap {
		if prev, ok := e.active[k]; !ok {
			e.genCounter++
			v.Generation = e.genCounter
			e.gen[k] = v.Generation
			v.EventID = violationEventID(e.instanceID, v.Provider, v.Rule, v.Generation, SLOStateEntered)
			entered = append(entered, v)
			e.active[k] = v
		} else {
			// 持续违规:保留 generation 与 event id 语义(active 内存 entered 的 EventID)
			v.Generation = prev.Generation
			v.EventID = prev.EventID
			e.active[k] = v
		}
	}
	// resolved
	for k, v := range e.active {
		if _, ok := curMap[k]; !ok {
			v.EventID = violationEventID(e.instanceID, v.Provider, v.Rule, v.Generation, SLOStateResolved)
			resolved = append(resolved, v)
			delete(e.active, k)
			// e.gen[k] 保留,便于 undelivered 的 resolved 对账匹配同一生命周期
		}
	}
	return entered, resolved
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
		if e.config.P99LatencyMaxMS > 0 {
			if s.samples >= 10 && s.p99MS > e.config.P99LatencyMaxMS {
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
	}
	return out
}

func (e *SLOEvaluator) record(violations []SLOViolation) {
	e.mu.Lock()
	e.violations = append(e.violations, violations...)
	if len(e.violations) > maxViolationHistory {
		e.violations = append([]SLOViolation(nil), e.violations[len(e.violations)-maxViolationHistory:]...)
	}
	e.mu.Unlock()
}

func (e *SLOEvaluator) newPayload(entered, resolved []SLOViolation) SLOWebhookPayload {
	e.mu.Lock()
	e.seq++
	seq := e.seq
	instanceID := e.instanceID
	e.mu.Unlock()
	return SLOWebhookPayload{
		At:         time.Now(),
		InstanceID: instanceID,
		Seq:        seq,
		Entered:    entered,
		Resolved:   resolved,
	}
}

func (e *SLOEvaluator) enqueueWebhook(payload SLOWebhookPayload) {
	e.enqueueJob(webhookJob{Payload: payload})
}

func (e *SLOEvaluator) enqueueJob(job webhookJob) {
	if e == nil || e.webhook == "" || e.webhookCh == nil {
		return
	}
	if !e.started.Load() {
		e.dropped.Add(1)
		slog.Warn("slo webhook dropped: evaluator closed",
			slog.Uint64("seq", job.Payload.Seq),
			slog.Int("entered", len(job.Payload.Entered)),
			slog.Int("resolved", len(job.Payload.Resolved)),
			slog.Uint64("dropped_total", e.dropped.Load()),
		)
		return
	}
	select {
	case e.webhookCh <- job:
	default:
		e.dropped.Add(1)
		slog.Warn("slo webhook dropped: queue full",
			slog.Uint64("seq", job.Payload.Seq),
			slog.Int("entered", len(job.Payload.Entered)),
			slog.Int("resolved", len(job.Payload.Resolved)),
			slog.Uint64("dropped_total", e.dropped.Load()),
		)
	}
}

// reconcilePayload 按当前 active 与 generation 过滤过期事件:
//   - entered: key 仍在 active 且 generation 与当前代一致(Generation==0 时跳过代次检查,兼容测试注入);
//   - resolved: key 已不在 active 且 generation 与该 key 最后一代一致。
//
// 过滤后为空则 ok=false。
func (e *SLOEvaluator) reconcilePayload(p SLOWebhookPayload) (SLOWebhookPayload, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var entered, resolved []SLOViolation
	for _, v := range p.Entered {
		k := violationKey(v)
		cur, ok := e.active[k]
		if !ok {
			continue
		}
		if v.Generation != 0 && cur.Generation != v.Generation {
			continue
		}
		entered = append(entered, v)
	}
	for _, v := range p.Resolved {
		k := violationKey(v)
		if _, ok := e.active[k]; ok {
			// 已再次违规,旧 resolved 过期
			continue
		}
		if v.Generation != 0 && e.gen[k] != v.Generation {
			// 已进入更新的生命周期
			continue
		}
		resolved = append(resolved, v)
	}
	if len(entered) == 0 && len(resolved) == 0 {
		return SLOWebhookPayload{}, false
	}
	p.Entered = entered
	p.Resolved = resolved
	return p, true
}

// flushUndelivered 把到期的待重投 job 重新入队;未到期或队列满则继续留在 undelivered。
// 入队前对账,丢弃已过期状态;对账后重新分配递增 Seq(delivery),
// 各条 violation.EventID 在 applyState 时已固定,对账裁剪不改变剩余项的 EventID。
func (e *SLOEvaluator) flushUndelivered() {
	if e == nil || e.webhook == "" || e.webhookCh == nil || !e.started.Load() {
		return
	}
	now := time.Now()
	e.mu.Lock()
	if len(e.undelivered) == 0 {
		e.mu.Unlock()
		return
	}
	pending := e.undelivered
	e.undelivered = nil
	instanceID := e.instanceID
	e.mu.Unlock()

	var leftover []webhookJob
	for _, job := range pending {
		if !e.started.Load() {
			leftover = append(leftover, job)
			continue
		}
		if !job.NextRetry.IsZero() && job.NextRetry.After(now) {
			leftover = append(leftover, job)
			continue
		}
		payload, ok := e.reconcilePayload(job.Payload)
		if !ok {
			// 状态已过期,计 dropped
			e.dropped.Add(1)
			continue
		}
		// 重投只递增 Seq;InstanceID 与各条 EventID 保持不变。
		prevSeq := payload.Seq
		e.mu.Lock()
		e.seq++
		payload.Seq = e.seq
		payload.InstanceID = instanceID
		e.mu.Unlock()
		// 确保对账后剩余条目仍有 EventID(测试注入路径可能缺失)
		for i := range payload.Entered {
			if payload.Entered[i].EventID == "" {
				payload.Entered[i].EventID = violationEventID(instanceID, payload.Entered[i].Provider, payload.Entered[i].Rule, payload.Entered[i].Generation, SLOStateEntered)
			}
		}
		for i := range payload.Resolved {
			if payload.Resolved[i].EventID == "" {
				payload.Resolved[i].EventID = violationEventID(instanceID, payload.Resolved[i].Provider, payload.Resolved[i].Rule, payload.Resolved[i].Generation, SLOStateResolved)
			}
		}
		job.Payload = payload
		job.NextRetry = time.Time{}
		select {
		case e.webhookCh <- job:
			if prevSeq != 0 && prevSeq != payload.Seq {
				slog.Debug("slo webhook redelivery reseq",
					slog.String("instance_id", payload.InstanceID),
					slog.Uint64("prev_seq", prevSeq),
					slog.Uint64("seq", payload.Seq),
					slog.Int("entered", len(payload.Entered)),
					slog.Int("resolved", len(payload.Resolved)),
				)
			}
		default:
			// 队列满:保留新 seq 的 job 待下次 flush(序号已占位,可接受空洞)
			leftover = append(leftover, job)
		}
	}
	if len(leftover) == 0 {
		return
	}
	e.mu.Lock()
	e.undelivered = append(leftover, e.undelivered...)
	e.trimUndeliveredLocked()
	e.mu.Unlock()
}

// storeUndelivered 在可重试失败后保存 job,供后续 CheckNow flush。
func (e *SLOEvaluator) storeUndelivered(job webhookJob) {
	if e == nil || !e.started.Load() {
		e.dropped.Add(1)
		return
	}
	e.mu.Lock()
	e.undelivered = append(e.undelivered, job)
	e.trimUndeliveredLocked()
	e.mu.Unlock()
}

func (e *SLOEvaluator) trimUndeliveredLocked() {
	if len(e.undelivered) <= maxUndelivered {
		return
	}
	drop := len(e.undelivered) - maxUndelivered
	e.undelivered = append([]webhookJob(nil), e.undelivered[drop:]...)
	e.dropped.Add(uint64(drop))
	slog.Warn("slo webhook undelivered overflow",
		slog.Int("discarded", drop),
		slog.Uint64("dropped_total", e.dropped.Load()),
	)
}

func (e *SLOEvaluator) webhookWorker() {
	defer e.webhookWG.Done()
	for {
		select {
		case <-e.webhookStop:
			// Close:剩余队列由 discardPending 计入 dropped。
			return
		case job, ok := <-e.webhookCh:
			if !ok {
				return
			}
			// stop 与队列同时就绪时 Go 可能选到本分支;投递前再确认一次。
			select {
			case <-e.webhookStop:
				e.dropped.Add(1)
				return
			default:
			}
			e.deliverWebhook(job)
		}
	}
}

// deliverWebhook 单次 attempt;可重试失败写入 undelivered(不占用 worker sleep);
// 投递前对账,过期直接丢弃。
func (e *SLOEvaluator) deliverWebhook(job webhookJob) {
	if e == nil || e.webhook == "" || e.client == nil {
		return
	}
	payload, ok := e.reconcilePayload(job.Payload)
	if !ok {
		e.dropped.Add(1)
		return
	}
	job.Payload = payload

	body, err := json.Marshal(payload)
	if err != nil {
		e.webhookError.Add(1)
		slog.Warn("slo webhook marshal failed", slog.Any("error", err))
		return
	}

	select {
	case <-e.webhookStop:
		e.webhookCanceled.Add(1)
		e.dropped.Add(1)
		return
	default:
	}

	result, retryAfter := e.postWebhookOnce(body)
	switch result {
	case webhookResultOK:
		return
	case webhookResultCanceled:
		e.dropped.Add(1)
		return
	case webhookResultPermanent:
		// 不可恢复,不进 undelivered
		return
	case webhookResultRetryable:
		job.Attempts++
		if job.Attempts >= webhookMaxAttempts {
			slog.Warn("slo webhook exhausted retries",
				slog.String("webhook", redactWebhookURL(e.webhook)),
				slog.Uint64("seq", job.Payload.Seq),
				slog.Int("attempts", job.Attempts),
				slog.Int("entered", len(job.Payload.Entered)),
				slog.Int("resolved", len(job.Payload.Resolved)),
			)
			e.dropped.Add(1)
			return
		}
		// 延期重投,不阻塞 worker
		delay := retryAfter
		if delay <= 0 {
			// attempts 已含本次,退避:100ms,200ms,...
			shift := job.Attempts - 1
			if shift < 0 {
				shift = 0
			}
			delay = webhookRetryBase << shift
		}
		if delay > webhookRetryAfterMax {
			delay = webhookRetryAfterMax
		}
		job.NextRetry = time.Now().Add(delay)
		e.storeUndelivered(job)
	}
}

type webhookPostResult int

const (
	webhookResultOK webhookPostResult = iota
	webhookResultRetryable
	webhookResultPermanent
	webhookResultCanceled
)

// postWebhookOnce 单次 POST;每次 attempt 都计入 requests_total{result=...}。
// 第二个返回值为建议退避(仅 429+Retry-After 时非零)。
func (e *SLOEvaluator) postWebhookOnce(body []byte) (webhookPostResult, time.Duration) {
	ctx, cancel := context.WithTimeout(e.webhookCtx, webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.webhook, bytes.NewReader(body))
	if err != nil {
		e.webhookError.Add(1)
		slog.Warn("slo webhook request build failed", slog.String("error", sanitizeWebhookError(err)))
		return webhookResultPermanent, 0
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		if e.webhookCtx.Err() != nil {
			e.webhookCanceled.Add(1)
			return webhookResultCanceled, 0
		}
		e.webhookError.Add(1)
		slog.Warn("slo webhook post failed",
			slog.String("webhook", redactWebhookURL(e.webhook)),
			slog.String("error", sanitizeWebhookError(err)),
		)
		return webhookResultRetryable, 0
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookBodyLimit))
	_ = resp.Body.Close()

	if isRetryableWebhookStatus(resp.StatusCode) {
		e.webhookNon2xx.Add(1)
		slog.Warn("slo webhook non-2xx",
			slog.String("webhook", redactWebhookURL(e.webhook)),
			slog.Int("status", resp.StatusCode),
		)
		var retryAfter time.Duration
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		return webhookResultRetryable, retryAfter
	}
	if resp.StatusCode >= 300 {
		e.webhookNon2xx.Add(1)
		slog.Warn("slo webhook non-2xx",
			slog.String("webhook", redactWebhookURL(e.webhook)),
			slog.Int("status", resp.StatusCode),
		)
		return webhookResultPermanent, 0
	}
	e.webhookOK.Add(1)
	return webhookResultOK, 0
}

// isRetryableWebhookStatus: 408/425/429 与 5xx 可重试;其他 4xx 永久失败。
func isRetryableWebhookStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooEarly,        // 425
		http.StatusTooManyRequests: // 429
		return true
	}
	return code >= 500
}

// parseRetryAfter 解析 Retry-After:先试 delta-seconds,再试 HTTP-date。
// 非法、已过期或超上限时:超上限裁剪到 webhookRetryAfterMax;非法/过期返回 0(调用方走指数退避)。
// 极大整数秒数在乘法前裁剪,避免 time.Duration 溢出变成负数。
func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if sec, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if sec <= 0 {
			return 0
		}
		maxSeconds := int64(webhookRetryAfterMax / time.Second)
		if sec >= maxSeconds {
			return webhookRetryAfterMax
		}
		return time.Duration(sec) * time.Second
	}
	// HTTP-date (RFC 7231)
	t, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	d := time.Until(t)
	if d <= 0 {
		return 0
	}
	if d > webhookRetryAfterMax {
		return webhookRetryAfterMax
	}
	return d
}

// sanitizeWebhookError 去掉 *url.Error 中嵌套的完整 URL,避免 path/query 凭据进入日志。
func sanitizeWebhookError(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Err != nil {
			return urlErr.Op + ": " + urlErr.Err.Error()
		}
		return urlErr.Op + " failed"
	}
	msg := err.Error()
	if strings.Contains(msg, "://") {
		if i := strings.Index(msg, " \""); i > 0 {
			return msg[:i] + " <redacted-url>"
		}
		if i := strings.Index(msg, "://"); i > 0 {
			j := i
			for j > 0 && msg[j-1] != ' ' && msg[j-1] != '"' {
				j--
			}
			return msg[:j] + "<redacted-url>"
		}
	}
	return msg
}

// redactWebhookURL 仅保留 scheme://host，避免日志泄露 path/query 中的 webhook 凭据。
func redactWebhookURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<invalid-webhook-url>"
	}
	return u.Scheme + "://" + u.Host
}

// Webhook 返回配置中的 webhook URL(可能为空)。
func (e *SLOEvaluator) Webhook() string {
	if e == nil {
		return ""
	}
	return e.webhook
}

// InstanceID 返回本 evaluator 启动时生成的实例标识(可能为空,当 e 为 nil)。
func (e *SLOEvaluator) InstanceID() string {
	if e == nil {
		return ""
	}
	return e.instanceID
}
