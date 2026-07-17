// Package aiproxybootstrap 保存单次进程启动时由 entry 注入的只读快照。
// 只有 framework 启动基础设施（Initiator 与 Config Block）可消费该快照；
// 业务 Module 必须通过 EventHub 获取当前配置。
package aiproxybootstrap

import (
	"sync"

	"ai-proxy/internal/pkg/aiproxycontract"
)

var state struct {
	sync.RWMutex
	value aiproxycontract.Bootstrap
	set   bool
}

func Configure(value aiproxycontract.Bootstrap) {
	state.Lock()
	defer state.Unlock()
	state.value = value
	state.set = true
}

func Current() (aiproxycontract.Bootstrap, bool) {
	state.RLock()
	defer state.RUnlock()
	return state.value, state.set
}
