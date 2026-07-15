package proxy

import (
	"path/filepath"

	"ai-proxy/internal/archive"
	"ai-proxy/internal/config"
	"ai-proxy/internal/metrics"
	"ai-proxy/internal/stats"
)

// mustHandlerConfig 为测试构造已解析 Config:补齐 endpoint_capabilities 与 catalog RouteOwner。
// 生产路径必须走 config.Load;Handler 不再 materialize。
// operations 仅填 RouteOwner 实际可服务的集合,满足 operation×capability 交叉校验。
func mustHandlerConfig(cfg config.Config) config.Config {
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.Provider{}
	}
	if cfg.ModelCatalog == nil {
		cfg.ModelCatalog = map[string]config.ModelInfo{}
	}
	seedCommonModels := len(cfg.ModelCatalog) == 0
	for name, provider := range cfg.Providers {
		if provider.Name == "" {
			provider.Name = name
		}
		if provider.Protocol == "" {
			if name == "anthropic" {
				provider.Protocol = "anthropic"
			} else {
				provider.Protocol = "openai"
			}
		}
		if len(provider.EndpointCapabilities) == 0 && !provider.Disabled {
			if provider.Protocol == "anthropic" {
				provider.EndpointCapabilities = []string{config.EndpointCapabilityMessages}
			} else {
				provider.EndpointCapabilities = []string{
					config.EndpointCapabilityChatCompletions,
					config.EndpointCapabilityResponses,
					config.EndpointCapabilityCompletions,
					config.EndpointCapabilityEmbeddings,
				}
			}
		}
		cfg.Providers[name] = provider
	}
	// 测试 helper:仅空 catalog 时从精确 models 合成条目。
	if seedCommonModels {
		for name, provider := range cfg.Providers {
			if provider.Disabled {
				continue
			}
			for _, pattern := range provider.Models {
				if pattern == "" || containsStar(pattern) {
					continue
				}
				if _, ok := cfg.ModelCatalog[pattern]; ok {
					continue
				}
				cfg.ModelCatalog[pattern] = config.ModelInfo{
					ID:                  pattern,
					ContextWindowTokens: 128000,
					MaxOutputTokens:     16384,
					Operations:          serviceableOperations(provider),
					RouteOwner:          name,
				}
			}
		}
		for _, modelID := range []string{
			"gpt-test", "deepseek-chat", "claude-test", "gpt-5.4", "kimi-k2",
			"healthcheck", "gpt-4o", "shared-model",
		} {
			if _, ok := cfg.ModelCatalog[modelID]; ok {
				continue
			}
			matches := matchingEnabled(cfg.Providers, modelID)
			if len(matches) != 1 {
				continue
			}
			owner := matches[0]
			cfg.ModelCatalog[modelID] = config.ModelInfo{
				ID:                  modelID,
				ContextWindowTokens: 128000,
				MaxOutputTokens:     16384,
				Operations:          serviceableOperations(cfg.Providers[owner]),
				RouteOwner:          owner,
			}
		}
	} else {
		// 显式 catalog:仅重绑已有条目失效的 RouteOwner,不新增 model。
		for modelID, info := range cfg.ModelCatalog {
			if info.RouteOwner != "" {
				if p, ok := cfg.Providers[info.RouteOwner]; ok && !p.Disabled && config.ProviderMatchesModel(info.RouteOwner, p, info.ID) {
					continue
				}
			}
			matches := matchingEnabled(cfg.Providers, info.ID)
			if len(matches) == 1 {
				info.RouteOwner = matches[0]
				cfg.ModelCatalog[modelID] = info
			}
		}
	}
	for id, info := range cfg.ModelCatalog {
		if info.ID == "" {
			info.ID = id
		}
		if info.RouteOwner == "" {
			matches := matchingEnabled(cfg.Providers, info.ID)
			if len(matches) == 1 {
				info.RouteOwner = matches[0]
			}
		}
		if len(info.Operations) == 0 {
			if p, ok := cfg.Providers[info.RouteOwner]; ok {
				info.Operations = serviceableOperations(p)
			} else {
				info.Operations = []string{config.ModelOperationChatCompletions}
			}
		} else if p, ok := cfg.Providers[info.RouteOwner]; ok {
			// 过滤掉 RouteOwner 无法服务的 operation,避免测试夹具与 capability 冲突。
			info.Operations = filterServiceableOperations(info.Operations, p)
			if len(info.Operations) == 0 {
				info.Operations = serviceableOperations(p)
			}
		}
		if info.ContextWindowTokens <= 0 {
			info.ContextWindowTokens = 128000
		}
		if info.MaxOutputTokens <= 0 {
			info.MaxOutputTokens = 16384
		}
		if info.MaxOutputTokens >= info.ContextWindowTokens {
			info.MaxOutputTokens = info.ContextWindowTokens - 1
		}
		cfg.ModelCatalog[id] = info
	}
	return cfg
}

func serviceableOperations(provider config.Provider) []string {
	ops := make([]string, 0, 2)
	if config.ProviderSupportsInboundPath(provider, "/v1/chat/completions") {
		ops = append(ops, config.ModelOperationChatCompletions)
	}
	if config.ProviderSupportsInboundPath(provider, "/v1/embeddings") {
		ops = append(ops, config.ModelOperationEmbeddings)
	}
	if len(ops) == 0 {
		ops = []string{config.ModelOperationChatCompletions}
	}
	return ops
}

func filterServiceableOperations(ops []string, provider config.Provider) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		path := config.OperationToPrimaryInboundPath(op)
		if path != "" && config.ProviderSupportsInboundPath(provider, path) {
			out = append(out, op)
		}
	}
	return out
}

func containsStar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '*' {
			return true
		}
	}
	return false
}

func matchingEnabled(providers map[string]config.Provider, modelID string) []string {
	var matches []string
	for name, provider := range providers {
		if provider.Disabled {
			continue
		}
		if config.ProviderMatchesModel(name, provider, modelID) {
			matches = append(matches, name)
		}
	}
	return matches
}

func newTestHandler(cfg config.Config, usageFile string) *Handler {
	cfg = mustHandlerConfig(cfg)
	if cfg.UsageFile == "" {
		cfg.UsageFile = usageFile
	}
	if cfg.InteractionDir == "" {
		cfg.InteractionDir = filepath.Join(filepath.Dir(usageFile), "interactions")
	}
	interactionRecorder, err := archive.NewRecorder(cfg.InteractionDir)
	if err != nil {
		panic(err)
	}
	return NewHandler(cfg, stats.NewCSVRecorder(cfg.UsageFile), interactionRecorder, metrics.NewRegistry())
}
