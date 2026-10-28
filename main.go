package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	mailServer "maild/internal/api/mailServer"
	"maild/internal/config"
	"maild/internal/service"
	"maild/pkg/version"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "2006-01-02T15:04:05.000Z", TimeLocation: time.UTC}
	log.Logger = log.Output(output).With().Logger()
}

func main() {
	flagConfigFile := flag.String("config", "", "Path to the configuration file (default: <exe-dir>/conf/maild.toml)")
	flagVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *flagVersion {
		fmt.Println(version.Detailed())
		os.Exit(0)
	}

	log.Info().Msgf("Mail gateway version %s starting...", version.Info())

	cfg, err := config.Load(*flagConfigFile)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load mail config")
	}

	tlsManager, err := mailServer.NewTLSManager(cfg.TLS)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load TLS certificates")
	}

	mailService, err := service.New(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize mail service")
	}

	queueCtx, stopQueue := context.WithCancel(context.Background())
	queueDone := make(chan struct{})
	go func() {
		defer close(queueDone)
		mailService.RunQueue(queueCtx)
	}()

	server, err := mailServer.NewServer(mailService, cfg.Server, tlsManager)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to start gRPC server")
	}

	log.Info().Str("spool_dir", cfg.Queue.SpoolDir).Int("queued", mailService.QueuedCount()).Msg("mail gateway ready")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for running := true; running; {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				cfg = reload(*flagConfigFile, cfg, tlsManager, mailService)
				continue
			}
			log.Info().Stringer("signal", sig).Msg("received shutdown signal")
			running = false
		case err := <-server.Err():
			if err != nil {
				log.Error().Err(err).Msg("gRPC server failed")
			}
			running = false
		}
	}

	// 先停止取新的佇列檔，再等 gRPC server 收尾（含進行中的同步寄送）；
	// 進行中的背景寄送與 gRPC 收尾共用同一個 shutdown_timeout。
	shutdownStart := time.Now()
	stopQueue()
	server.Stop()
	select {
	case <-queueDone:
	case <-time.After(cfg.Server.ShutdownTimeout.Std() - time.Since(shutdownStart)):
		log.Warn().Msg("background delivery did not finish before shutdown timeout; unfinished mails stay in the queue")
	}
	log.Info().Msg("mail gateway stopped")
}

// reload 處理 SIGHUP：重新讀取設定檔，替換 [senders]、[clients] 並重新載入 TLS 憑證。
// 任何一步失敗都沿用舊的設定，不會因此停機。回傳目前生效的設定。
func reload(path string, current config.Config, tlsManager *mailServer.TLSManager, mailService *service.Service) config.Config {
	log.Info().Msg("received SIGHUP, reloading configuration")
	next, err := config.Load(path)
	if err != nil {
		log.Error().Err(err).Msg("reload failed, keeping the current configuration")
		return current
	}
	if err := tlsManager.Reload(); err != nil {
		log.Error().Err(err).Msg("reload TLS certificates failed, keeping the current configuration")
		return current
	}
	mailService.Reload(next)
	if !current.StaticEqual(next) {
		log.Warn().Msg("settings outside [senders] and [clients] changed; restart maild for them to take effect")
	}
	// 只有 [senders]、[clients] 生效，其他欄位仍以啟動時的值為準。
	current.Senders, current.Clients = next.Senders, next.Clients
	log.Info().Int("senders", len(next.Senders)).Int("clients", len(next.Clients)).Msg("configuration reloaded")
	return current
}
