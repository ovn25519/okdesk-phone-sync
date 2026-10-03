// Package config отвечает за загрузку, валидацию и безопасное логирование
// конфигурации сервиса okdesk-phone-sync.
//
// Формат конфигурации — TOML. Значения по умолчанию задаются функцией Default
// и перекрываются только теми ключами, которые присутствуют в файле. Любые
// неизвестные ключи считаются ошибкой: это защищает от опечаток в именах
// параметров, из-за которых настройка молча игнорировалась бы.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/BurntSushi/toml"
)

// maskedValue — значение, которым заменяются секреты при логировании.
const maskedValue = "***"

// Config — корневая структура конфигурации.
type Config struct {
	Okdesk Okdesk `toml:"okdesk"`
	Sync   Sync   `toml:"sync"`
	Log    Log    `toml:"log"`
}

// Okdesk — параметры REST API Okdesk и правила отбора заявок.
type Okdesk struct {
	// BaseURL — базовый URL аккаунта, например https://example.okdesk.ru
	BaseURL string `toml:"base_url"`
	// APIToken — токен API Okdesk. Храните только в config.toml (права 0600).
	APIToken string `toml:"api_token"`
	// PhoneAttrCode — машинный код дополнительного атрибута заявки, в который
	// записывается телефон контакта. Значение задаётся пользователем при
	// создании атрибута в интерфейсе Okdesk. Обязательное поле.
	PhoneAttrCode string `toml:"phone_attr_code"`
	// StatusCodes — коды статусов заявок, которые считаются открытыми и
	// подлежат обработке. По умолчанию: opened, inprogress, delayed.
	StatusCodes []string `toml:"status_codes"`
	// PhonePriority — порядок выбора телефона контакта. Допустимые значения:
	// mobile_phone, phone. Первое непустое поле используется.
	PhonePriority []string `toml:"phone_priority"`
	// HTTPTimeoutSeconds — таймаут одного HTTP-запроса к Okdesk.
	HTTPTimeoutSeconds int `toml:"http_timeout_seconds"`
	// VerifyAttr — проверять на старте, что phone_attr_code присутствует в
	// схеме дополнительных атрибутов заявок. Защищает от опечатки в коде.
	VerifyAttr bool `toml:"verify_attr"`
}

// Sync — параметры цикла синхронизации.
type Sync struct {
	// PollIntervalMinutes — период между проходами демона.
	PollIntervalMinutes int `toml:"poll_interval_minutes"`
	// PageSize — размер страницы при постраничном чтении заявок (1..50).
	PageSize int `toml:"page_size"`
	// RequestDelayMillis — пауза между последовательными запросами к Okdesk.
	RequestDelayMillis int `toml:"request_delay_ms"`
	// MaxRetries — число повторных попыток при сетевой ошибке или ответе 5xx.
	MaxRetries int `toml:"max_retries"`
	// InitialBackoffSeconds — задержка перед первой повторной попыткой.
	InitialBackoffSeconds int `toml:"initial_backoff_seconds"`
	// MaxBackoffSeconds — верхняя граница экспоненциальной задержки повторов.
	MaxBackoffSeconds int `toml:"max_backoff_seconds"`
	// DryRun — режим примерки: заявки и телефоны находятся, но в Okdesk ничего
	// не записывается.
	DryRun bool `toml:"dry_run"`
	// SearchInDescription — искать телефон в описании заявки, если у контакта
	// телефона нет. По умолчанию выключено: в тексте возможны посторонние числа.
	SearchInDescription bool `toml:"search_in_description"`
}

// Log — параметры логирования.
type Log struct {
	// Level — уровень логирования: debug, info, warn или error.
	Level string `toml:"level"`
}

// Default возвращает конфигурацию со значениями по умолчанию. Срезы
// status_codes и phone_priority намеренно не задаются здесь: их значения по
// умолчанию подставляет normalize, только если в файле ключ отсутствует.
func Default() Config {
	return Config{
		Okdesk: Okdesk{
			HTTPTimeoutSeconds: 30,
			VerifyAttr:         true,
		},
		Sync: Sync{
			PollIntervalMinutes:   5,
			PageSize:              50,
			RequestDelayMillis:    200,
			MaxRetries:            3,
			InitialBackoffSeconds: 2,
			MaxBackoffSeconds:     60,
			DryRun:                false,
			SearchInDescription:   false,
		},
		Log: Log{
			Level: "info",
		},
	}
}

// Load читает конфигурацию из файла path, применяет значения по умолчанию,
// нормализует значения и проверяет корректность.
func Load(path string) (Config, error) {
	cfg := Default()

	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("чтение конфигурации %q: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("конфигурация %q содержит неизвестные ключи: %s", path, strings.Join(keys, ", "))
	}

	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("конфигурация %q: %w", path, err)
	}
	return cfg, nil
}

// normalize приводит значения к каноническому виду: убирает завершающие «/» у
// базового URL, подставляет значения по умолчанию для необязательных срезов и
// обрезает пробелы.
func (c *Config) normalize() {
	c.Okdesk.BaseURL = strings.TrimRight(strings.TrimSpace(c.Okdesk.BaseURL), "/")
	c.Okdesk.PhoneAttrCode = strings.TrimSpace(c.Okdesk.PhoneAttrCode)

	if len(c.Okdesk.StatusCodes) == 0 {
		c.Okdesk.StatusCodes = []string{"opened", "inprogress", "delayed"}
	} else {
		c.Okdesk.StatusCodes = trimAll(c.Okdesk.StatusCodes)
	}

	if len(c.Okdesk.PhonePriority) == 0 {
		c.Okdesk.PhonePriority = []string{"mobile_phone", "phone"}
	} else {
		c.Okdesk.PhonePriority = trimAll(c.Okdesk.PhonePriority)
	}

	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
}

// Validate проверяет конфигурацию и возвращает составную ошибку со списком всех
// найденных проблем (чтобы не исправлять их по одной).
func (c Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	// --- Okdesk ---
	if msg := validateHTTPURL("okdesk.base_url", c.Okdesk.BaseURL); msg != "" {
		add("%s", msg)
	}
	if strings.TrimSpace(c.Okdesk.APIToken) == "" {
		add("okdesk.api_token: обязательное поле не заполнено")
	}
	if c.Okdesk.PhoneAttrCode == "" {
		add("okdesk.phone_attr_code: обязательное поле не заполнено")
	}
	if len(c.Okdesk.StatusCodes) == 0 {
		add("okdesk.status_codes: требуется хотя бы один код статуса")
	}
	for i, code := range c.Okdesk.StatusCodes {
		if code == "" {
			add("okdesk.status_codes[%d]: пустое значение", i)
		}
	}
	if len(c.Okdesk.PhonePriority) == 0 {
		add("okdesk.phone_priority: требуется хотя бы одно поле телефона")
	}
	for i, field := range c.Okdesk.PhonePriority {
		if field != "mobile_phone" && field != "phone" {
			add("okdesk.phone_priority[%d]: недопустимое поле %q (ожидается mobile_phone или phone)", i, field)
		}
	}
	if c.Okdesk.HTTPTimeoutSeconds < 1 {
		add("okdesk.http_timeout_seconds: должно быть >= 1, получено %d", c.Okdesk.HTTPTimeoutSeconds)
	}

	// --- Sync ---
	if c.Sync.PollIntervalMinutes < 1 {
		add("sync.poll_interval_minutes: должно быть >= 1, получено %d", c.Sync.PollIntervalMinutes)
	}
	if c.Sync.PageSize < 1 || c.Sync.PageSize > 50 {
		add("sync.page_size: должно быть в диапазоне 1..50, получено %d", c.Sync.PageSize)
	}
	if c.Sync.RequestDelayMillis < 0 {
		add("sync.request_delay_ms: должно быть >= 0, получено %d", c.Sync.RequestDelayMillis)
	}
	if c.Sync.MaxRetries < 0 {
		add("sync.max_retries: должно быть >= 0, получено %d", c.Sync.MaxRetries)
	}
	if c.Sync.InitialBackoffSeconds < 1 {
		add("sync.initial_backoff_seconds: должно быть >= 1, получено %d", c.Sync.InitialBackoffSeconds)
	}
	if c.Sync.MaxBackoffSeconds < c.Sync.InitialBackoffSeconds {
		add("sync.max_backoff_seconds: должно быть >= sync.initial_backoff_seconds (%d), получено %d",
			c.Sync.InitialBackoffSeconds, c.Sync.MaxBackoffSeconds)
	}

	// --- Log ---
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level: недопустимый уровень %q (ожидается debug, info, warn или error)", c.Log.Level)
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("конфигурация невалидна:\n  - %s", strings.Join(errs, "\n  - "))
}

// SlogLevel возвращает уровень логирования для slog. Значение уже проверено
// Validate, поэтому неизвестный уровень трактуется как info.
func (c Config) SlogLevel() slog.Level {
	switch c.Log.Level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Redacted возвращает копию конфигурации с замаскированными секретами.
func (c Config) Redacted() Config {
	r := c
	r.Okdesk.APIToken = mask(c.Okdesk.APIToken)
	return r
}

// LogValue реализует slog.LogValuer: конфигурация всегда логируется в
// замаскированном виде, даже при случайном slog.Any("config", cfg).
func (c Config) LogValue() slog.Value {
	r := c.Redacted()
	return slog.GroupValue(
		slog.Any("okdesk", r.Okdesk),
		slog.Any("sync", r.Sync),
		slog.Any("log", r.Log),
	)
}

// trimAll обрезает пробелы и отбрасывает пустые элементы.
func trimAll(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if v := strings.TrimSpace(item); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// mask прячет непустой секрет, оставляя пустую строку пустой.
func mask(secret string) string {
	if secret == "" {
		return ""
	}
	return maskedValue
}

// validateHTTPURL возвращает текст ошибки для поля field или пустую строку.
func validateHTTPURL(field, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return field + ": обязательное поле не заполнено"
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Sprintf("%s: ожидается абсолютный http(s)-URL, получено %q", field, raw)
	}
	return ""
}
