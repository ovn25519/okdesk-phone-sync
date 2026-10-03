// Команда okdesk-phone-sync — демон, который периодически заполняет
// дополнительный атрибут заявки Okdesk номером телефона контакта заявки.
//
// Режим работы: по умолчанию процесс работает постоянно и выполняет проходы с
// заданным интервалом; флаг -once выполняет один проход и завершается.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"okdesk-phone-sync/internal/config"
	"okdesk-phone-sync/internal/okdesk"
	"okdesk-phone-sync/internal/sync"
)

// version и commit подставляются при сборке:
//
//	go build -ldflags "-X main.version=v0.1.0 -X main.commit=abc1234"
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config.toml", "путь к файлу конфигурации")
	showVersion := flag.Bool("version", false, "показать версию и выйти")
	once := flag.Bool("once", false, "выполнить один проход и завершиться")
	flag.Parse()

	if *showVersion {
		fmt.Printf("okdesk-phone-sync %s (commit %s)\n", version, commit)
		return 0
	}

	// Бутстрап-логгер нужен, чтобы сообщить об ошибке загрузки конфигурации до
	// того, как станет известен настроенный уровень логирования.
	bootstrap := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		bootstrap.Error("не удалось загрузить конфигурацию", "error", err)
		return 1
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	slog.SetDefault(logger)
	// cfg реализует slog.LogValuer: секреты в лог не попадают.
	logger.Info("конфигурация загружена", "version", version, "commit", commit, "cfg", cfg)

	client, err := okdesk.New(okdesk.Config{
		BaseURL:        cfg.Okdesk.BaseURL,
		APIToken:       cfg.Okdesk.APIToken,
		HTTPClient:     &http.Client{Timeout: time.Duration(cfg.Okdesk.HTTPTimeoutSeconds) * time.Second},
		Logger:         logger,
		MaxAttempts:    cfg.Sync.MaxRetries + 1,
		InitialBackoff: time.Duration(cfg.Sync.InitialBackoffSeconds) * time.Second,
		MaxBackoff:     time.Duration(cfg.Sync.MaxBackoffSeconds) * time.Second,
	})
	if err != nil {
		logger.Error("не удалось создать клиент Okdesk", "error", err)
		return 1
	}

	runner := sync.New(client, sync.Config{
		PhoneAttrCode:       cfg.Okdesk.PhoneAttrCode,
		StatusCodes:         cfg.Okdesk.StatusCodes,
		PhonePriority:       cfg.Okdesk.PhonePriority,
		PageSize:            cfg.Sync.PageSize,
		PollInterval:        time.Duration(cfg.Sync.PollIntervalMinutes) * time.Minute,
		RequestDelay:        time.Duration(cfg.Sync.RequestDelayMillis) * time.Millisecond,
		DryRun:              cfg.Sync.DryRun,
		VerifyAttr:          cfg.Okdesk.VerifyAttr,
		SearchInDescription: cfg.Sync.SearchInDescription,
	}, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		logger.Info("разовый проход", "dry_run", cfg.Sync.DryRun, "attr_code", cfg.Okdesk.PhoneAttrCode)
		if _, err := runner.RunOnce(ctx); err != nil {
			logger.Error("разовый проход завершился с ошибкой", "error", err)
			return 1
		}
		return 0
	}

	logger.Info("сервис запущен",
		"attr_code", cfg.Okdesk.PhoneAttrCode,
		"statuses", cfg.Okdesk.StatusCodes,
		"interval", time.Duration(cfg.Sync.PollIntervalMinutes)*time.Minute,
		"dry_run", cfg.Sync.DryRun)

	if err := runner.Run(ctx); err != nil {
		logger.Error("сервис остановлен с ошибкой", "error", err)
		return 1
	}

	logger.Info("сервис остановлен")
	return 0
}
