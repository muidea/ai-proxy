package aiproxy

import (
	_ "ai-proxy/internal/initiators/routeregistry"
	_ "ai-proxy/internal/modules/application/adminapi"
	_ "ai-proxy/internal/modules/application/proxyapi"
	_ "ai-proxy/internal/modules/blocks/configruntime"
	_ "ai-proxy/internal/modules/blocks/metricsruntime"
	_ "ai-proxy/internal/modules/blocks/usageruntime"
)
