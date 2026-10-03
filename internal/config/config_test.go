package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig сохраняет содержимое конфигурации во временный файл.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("запись конфигурации: %v", err)
	}
	return path
}

const minimalConfig = `
[okdesk]
base_url = "https://example.okdesk.ru"
api_token = "secret"
phone_attr_code = "phone"
`

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Okdesk.HTTPTimeoutSeconds != 30 {
		t.Errorf("http_timeout_seconds = %d", cfg.Okdesk.HTTPTimeoutSeconds)
	}
	if !cfg.Okdesk.VerifyAttr {
		t.Errorf("verify_attr = false, want true")
	}
	if got := cfg.Okdesk.StatusCodes; len(got) != 3 || got[0] != "opened" || got[2] != "delayed" {
		t.Errorf("status_codes = %v", got)
	}
	if got := cfg.Okdesk.PhonePriority; len(got) != 2 || got[0] != "mobile_phone" || got[1] != "phone" {
		t.Errorf("phone_priority = %v", got)
	}
	if cfg.Sync.PollIntervalMinutes != 5 || cfg.Sync.PageSize != 50 {
		t.Errorf("sync defaults = %+v", cfg.Sync)
	}
	if cfg.Sync.SearchInDescription {
		t.Errorf("search_in_description = true, want false")
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q", cfg.Log.Level)
	}
}

func TestLoadCustomValues(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[okdesk]
base_url = "https://example.okdesk.ru"
api_token = "secret"
phone_attr_code = "phone"
status_codes = ["opened"]
phone_priority = ["phone"]
verify_attr = false

[sync]
poll_interval_minutes = 10
page_size = 10
dry_run = true
search_in_description = true

[log]
level = "debug"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Okdesk.StatusCodes) != 1 || cfg.Okdesk.StatusCodes[0] != "opened" {
		t.Errorf("status_codes = %v", cfg.Okdesk.StatusCodes)
	}
	if len(cfg.Okdesk.PhonePriority) != 1 || cfg.Okdesk.PhonePriority[0] != "phone" {
		t.Errorf("phone_priority = %v", cfg.Okdesk.PhonePriority)
	}
	if cfg.Okdesk.VerifyAttr {
		t.Errorf("verify_attr = true, want false")
	}
	if cfg.Sync.PollIntervalMinutes != 10 || cfg.Sync.PageSize != 10 || !cfg.Sync.DryRun {
		t.Errorf("sync = %+v", cfg.Sync)
	}
	if !cfg.Sync.SearchInDescription {
		t.Errorf("search_in_description = false, want true")
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q", cfg.Log.Level)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(writeConfig(t, `
[okdesk]
base_url = "https://example.okdesk.ru"
api_token = "secret"
phone_attr_code = "phone"
typo_field = "x"
`))
	if err == nil || !strings.Contains(err.Error(), "неизвестные ключи") {
		t.Fatalf("err = %v, want неизвестные ключи", err)
	}
}

func TestLoadValidationCollectsAllProblems(t *testing.T) {
	_, err := Load(writeConfig(t, `
[okdesk]
base_url = ""
api_token = ""
phone_attr_code = ""
`))
	if err == nil {
		t.Fatal("ожидалась ошибка валидации")
	}
	msg := err.Error()
	for _, want := range []string{"okdesk.base_url", "okdesk.api_token", "okdesk.phone_attr_code"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в ошибке нет %q: %s", want, msg)
		}
	}
}

func TestValidateRejectsUnknownPhonePriority(t *testing.T) {
	cfg := Default()
	cfg.Okdesk.BaseURL = "https://example.okdesk.ru"
	cfg.Okdesk.APIToken = "secret"
	cfg.Okdesk.PhoneAttrCode = "phone"
	cfg.Okdesk.PhonePriority = []string{"telegram"}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "phone_priority") {
		t.Fatalf("err = %v", err)
	}
}

func TestRedactedMasksToken(t *testing.T) {
	cfg := Default()
	cfg.Okdesk.APIToken = "secret"

	if got := cfg.Redacted().Okdesk.APIToken; got != maskedValue {
		t.Fatalf("Redacted().APIToken = %q, want %q", got, maskedValue)
	}
}
