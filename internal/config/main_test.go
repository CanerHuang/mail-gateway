package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maild/internal/config"
)

func readSample(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "conf", "maild.toml"))
	if err != nil {
		t.Fatalf("read sample config: %v", err)
	}
	return string(data)
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "maild.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_Sample(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, readSample(t)))
	if err != nil {
		t.Fatalf("sample config should load: %v", err)
	}
	if got := cfg.Senders["service"].SMTP.Host; got != "smtp.gmail.com" {
		t.Errorf("senders.service.smtp.host = %q", got)
	}
	if got := cfg.Clients["order-service"].Limit(); got != 200 {
		t.Errorf("clients.order-service rate_limit = %d", got)
	}
	if got := cfg.Log.FieldLimit(); got != 200 {
		t.Errorf("log.max_field_length = %d", got)
	}
	if got := cfg.Server.MaxRequestBytes(); got != 4<<20 {
		t.Errorf("max request bytes = %d", got)
	}
}

func TestLoad_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		old     string
		new     string
		wantErr string
	}{
		{"unknown field", "[mail]\n", "[mail]\nunknown_key = 1\n", "unknown_key"},
		{"shutdown not above smtp timeout", "shutdown_timeout = '35s'", "shutdown_timeout = '30s'", "shutdown_timeout"},
		{"request not above attachments", "max_request_size_mib = 4", "max_request_size_mib = 2", "max_request_size_mib"},
		{"bad security", "security = 'tls'", "security = 'ssl'", "security"},
		{"credentials without tls", "security = 'tls'", "security = 'none'", "must be empty when security"},
		{"half credentials", "password = 'change_me'\n\n[senders.billing]", "\n[senders.billing]", "set together"},
		{"unknown sender alias", "senders = ['service', 'billing']", "senders = ['service', 'nobody']", "not defined in [senders]"},
		{"default sender not allowed", "senders = ['service']\ndefault_sender = 'service'", "senders = ['service']\ndefault_sender = 'billing'", "default_sender"},
		{"missing rate limit", "rate_limit = 100\n", "", "rate_limit must be set"},
		{"missing log field length", "max_field_length = 200\n", "", "max_field_length must be set"},
		{"meta line too large", "max_recipients = 50", "max_recipients = 500", "meta line"},
		{"retry max below initial", "retry_max_interval = '1h'", "retry_max_interval = '30s'", "retry_max_interval"},
		{"sender address with name", "address = 'billing@example.com'", "address = 'Billing <billing@example.com>'", "senders.billing.address"},
	}
	sample := readSample(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(sample, tc.old) {
				t.Fatalf("sample config does not contain %q", tc.old)
			}
			_, err := config.Load(writeConfig(t, strings.Replace(sample, tc.old, tc.new, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestStaticEqual(t *testing.T) {
	sample := readSample(t)
	base, err := config.Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	reloadable, err := config.Load(writeConfig(t, strings.Replace(sample, "rate_limit = 200", "rate_limit = 300", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !base.StaticEqual(reloadable) {
		t.Error("changing [clients] should not count as a static change")
	}
	static, err := config.Load(writeConfig(t, strings.Replace(sample, "workers = 2", "workers = 3", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if base.StaticEqual(static) {
		t.Error("changing [queue] should count as a static change")
	}
}
