// Package common 定义 RouteRegistry Initiator 对其它运行单元暴露的窄基础设施契约。
package common

import enginehttp "github.com/muidea/magicEngine/http"

const RouteRegistryInitiator = "aiproxy.initiator.routeregistry"

// RouteRegistryHelper 只暴露 HTTP 路由注册能力。
// Application Module 可用它声明自己的路由，但不持有 HTTP listener 的生命周期。
type RouteRegistryHelper interface {
	GetRouteRegistry() enginehttp.RouteRegistry
}

// GatewayRuntimeHelper 供 process service 等基础设施编排者等待 HTTP server 退出。
// 它不应被业务 Module 用作跨组件通信通道。
type GatewayRuntimeHelper interface {
	RouteRegistryHelper
	Start() error
	Done() <-chan error
}
