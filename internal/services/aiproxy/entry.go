package aiproxy

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/services/aiproxy/logging"
)

// Run 是 ai-proxy 主进程的 service shell。cmd 入口只传入构建版本；
// 配置加载、信号处理及 magicCommon lifecycle 编排均归 process service 所有。
// 若 args 为 admin 子命令(如 password-hash)，则不启动 HTTP gateway。
func Run(version string) int {
	// 子命令优先于 flag 解析，避免 flag 吞掉 admin 参数。
	if code, handled := tryAdminSubcommand(os.Args[1:]); handled {
		return code
	}
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintln(out, "Usage:")
		fmt.Fprintln(out, "  ai-proxy [-config <config.yaml>]")
		fmt.Fprintln(out, "  ai-proxy admin password-hash")
		fmt.Fprintln(out, "  ai-proxy admin set-credentials --username <username> [--config <config.yaml>]")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Service options:")
		flag.PrintDefaults()
	}
	configPath := flag.String("config", os.Getenv("AI_PROXY_CONFIG"), "config file path")
	flag.Parse()

	resolvedConfigPath := config.ResolvePath(*configPath)
	cfg, err := config.Load(resolvedConfigPath)
	if err != nil {
		slog.Error("load config", slog.Any("error", err))
		return 1
	}
	logging.ConfigureLogger(cfg.LogFormat, cfg.DebugLog)

	runtime := NewRuntime(configevents.Bootstrap{Config: cfg, ConfigPath: resolvedConfigPath})
	serviceCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runtime.Startup(serviceCtx); err != nil {
		slog.Error("startup ai-proxy service", slog.Any("error", err))
		return 1
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("ai-proxy listening",
			slog.String("version", version),
			slog.String("addr", cfg.ListenAddr),
			slog.String("usage_store", cfg.UsageStore.Path),
			slog.String("interactions", cfg.InteractionDir),
			slog.String("metrics", MetricsAccessLabel(cfg.MetricsRemoteAccess)),
		)
		errCh <- runtime.Run(serviceCtx)
	}()

	exitCode := 0
	select {
	case <-serviceCtx.Done():
		slog.Info("received signal, shutting down")
	case err := <-errCh:
		if err != nil {
			slog.Error("server error", slog.Any("error", err))
			exitCode = 1
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runtime.Shutdown(ctx)
	return exitCode
}
