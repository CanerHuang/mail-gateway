package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"maild/internal/message"

	"github.com/pelletier/go-toml/v2"
)

const (
	DefaultConfigDir      = "conf"
	DefaultConfigFileName = "maild.toml"

	// MetaLineLimit 為佇列檔第一行（寄送資訊 JSON）的長度上限。
	MetaLineLimit = 64 * 1024
	// MetaSubjectLimit 為佇列檔第一行記錄主旨的最大字數；主旨只供 log 使用，完整主旨在郵件內容裡。
	MetaSubjectLimit = 1024
)

const (
	SecurityStartTLS = "starttls"
	SecurityTLS      = "tls"
	SecurityNone     = "none"
)

var (
	senderAliasPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// LabelKeyPattern 為 Mail.labels key 允許的字元。
	LabelKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

type Config struct {
	Server  ServerConfig            `toml:"server"`
	TLS     TLSConfig               `toml:"tls"`
	SMTP    SMTPConfig              `toml:"smtp"`
	Queue   QueueConfig             `toml:"queue"`
	Mail    MailConfig              `toml:"mail"`
	Log     LogConfig               `toml:"log"`
	Senders map[string]SenderConfig `toml:"senders"`
	Clients map[string]ClientConfig `toml:"clients"`
}

type ServerConfig struct {
	ListenAddress string `toml:"listen_address"`
	// ConnectionTimeout 為單一新連線從 accept 到完成 TLS 與 HTTP/2 handshake 的時限；
	// 逾時未完成即關閉該連線。此值同時是 graceful shutdown 等待 accept 迴圈
	// 收斂的上限，設得過大會直接拉長關機時間。
	ConnectionTimeout Duration `toml:"connection_timeout"`
	// ShutdownTimeout 為 graceful shutdown 的等待上限；逾時仍有連線未關閉時
	// 改為強制中斷。背景寄送的收尾也共用這個上限。
	ShutdownTimeout Duration `toml:"shutdown_timeout"`
	// ConnectionLog 決定是否記錄連線的建立與關閉（含 peer address 與 client 憑證 CN）。
	// 與其他欄位不同，bool 無法區分「未設定」與 false，因此省略等同關閉。
	ConnectionLog     bool            `toml:"connection_log"`
	MaxRequestSizeMiB int             `toml:"max_request_size_mib"`
	Keepalive         KeepaliveConfig `toml:"keepalive"`
}

// KeepaliveConfig 為 gRPC server 的 keepalive 與連線存活時間設定。
type KeepaliveConfig struct {
	// Time 為連線閒置多久後主動送 keepalive ping 探活。
	Time Duration `toml:"time"`
	// Timeout 為送出 ping 後等待回應的逾時，逾時未收到回應即斷線。
	Timeout Duration `toml:"timeout"`
	// MinTime 為允許 client 端 keepalive ping 的最短間隔，過於頻繁會被視為違規而中斷連線。
	MinTime Duration `toml:"min_time"`
	// MaxConnectionAge 到期後要求 client 重新連線，讓換掉的 CA／憑證在新的握手生效。
	MaxConnectionAge Duration `toml:"max_connection_age"`
	// MaxConnectionAgeGrace 為到達 MaxConnectionAge 後，等待進行中 RPC 完成的時限。
	MaxConnectionAgeGrace Duration `toml:"max_connection_age_grace"`
}

type TLSConfig struct {
	CertFile      string   `toml:"cert_file"`
	KeyFile       string   `toml:"key_file"`
	ClientCAFile  string   `toml:"client_ca_file"`
	ExpiryWarning Duration `toml:"expiry_warning"`
}

type SMTPConfig struct {
	Timeout            Duration `toml:"timeout"`
	MaxSyncConnections int      `toml:"max_sync_connections"`
	SenderPause        Duration `toml:"sender_pause"`
}

type QueueConfig struct {
	SpoolDir             string   `toml:"spool_dir"`
	ScanInterval         Duration `toml:"scan_interval"`
	Workers              int      `toml:"workers"`
	MaxAttempts          int      `toml:"max_attempts"`
	RetryInitialInterval Duration `toml:"retry_initial_interval"`
	RetryMaxInterval     Duration `toml:"retry_max_interval"`
	MaxQueuedMails       int      `toml:"max_queued_mails"`
	MaxScheduleAhead     Duration `toml:"max_schedule_ahead"`
}

type MailConfig struct {
	MaxRecipients         int `toml:"max_recipients"`
	MaxAttachmentsSizeMiB int `toml:"max_attachments_size_mib"`
	MaxLabels             int `toml:"max_labels"`
	MaxLabelKeyLength     int `toml:"max_label_key_length"`
	MaxLabelValueLength   int `toml:"max_label_value_length"`
}

type LogConfig struct {
	// MaxFieldLength 為 0 表示不限制；用指標區分「未設定」與 0，避免漏寫時默默變成不限制。
	MaxFieldLength *int `toml:"max_field_length"`
}

type SenderConfig struct {
	Address string           `toml:"address"`
	Name    string           `toml:"name"`
	SMTP    SenderSMTPConfig `toml:"smtp"`
}

type SenderSMTPConfig struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	Security string `toml:"security"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

type ClientConfig struct {
	Senders       []string `toml:"senders"`
	DefaultSender string   `toml:"default_sender"`
	// RateLimit 為 0 表示不限制；用指標區分「未設定」與 0。
	RateLimit       *int     `toml:"rate_limit"`
	RateLimitWindow Duration `toml:"rate_limit_window"`
}

func (c ServerConfig) MaxRequestBytes() int { return c.MaxRequestSizeMiB << 20 }

func (c MailConfig) MaxAttachmentsBytes() int64 { return int64(c.MaxAttachmentsSizeMiB) << 20 }

// FieldLimit 回傳 log 文字欄位的最大顯示字數，0 表示不限制。
func (c LogConfig) FieldLimit() int { return *c.MaxFieldLength }

// Limit 回傳寄送量上限，0 表示不限制。
func (c ClientConfig) Limit() int { return *c.RateLimit }

// Load 嚴格載入 maild.toml：檔案必須存在，除註明可省略者外所有欄位必填，且需通過
// 合理性檢查。任何缺失或不合理的值都會回傳 error，不會自動補預設值。
func Load(path string) (Config, error) {
	configPath := strings.TrimSpace(path)
	if configPath == "" {
		configPath = DefaultConfigPath()
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, fmt.Errorf("read mail config %q failed: %w", configPath, err)
	}

	var cfg Config
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		// strict mode 的錯誤本身不含欄位名稱，細節要另外從 StrictMissingError 取出。
		var strictErr *toml.StrictMissingError
		if errors.As(err, &strictErr) {
			return Config{}, fmt.Errorf("parse mail config %q failed: %w\n%s", configPath, err, strictErr.String())
		}
		return Config{}, fmt.Errorf("parse mail config %q failed: %w", configPath, err)
	}

	if err := validate(&cfg); err != nil {
		return Config{}, fmt.Errorf("invalid mail config %q: %w", configPath, err)
	}
	return cfg, nil
}

// StaticEqual 比較 SIGHUP 不會重新載入的欄位（[senders]、[clients] 以外的全部）是否相同，
// 用來在 reload 時提醒「這些修改要重啟才會生效」。
func (c Config) StaticEqual(other Config) bool {
	c.Senders, c.Clients = nil, nil
	other.Senders, other.Clients = nil, nil
	return reflect.DeepEqual(c, other)
}

func validate(cfg *Config) error {
	if err := validateServer(cfg.Server); err != nil {
		return err
	}
	if err := validateTLS(cfg.TLS); err != nil {
		return err
	}
	if err := validateSMTP(cfg.SMTP); err != nil {
		return err
	}
	if err := validateQueue(cfg.Queue); err != nil {
		return err
	}
	if err := validateMail(cfg.Mail); err != nil {
		return err
	}
	if cfg.Log.MaxFieldLength == nil {
		return errors.New("log.max_field_length must be set (0 means unlimited)")
	}
	if *cfg.Log.MaxFieldLength < 0 {
		return errors.New("log.max_field_length must be >= 0")
	}

	smtpTimeout := cfg.SMTP.Timeout.Std()
	if cfg.Server.ShutdownTimeout.Std() <= smtpTimeout {
		return fmt.Errorf("server.shutdown_timeout (%s) must be > smtp.timeout (%s)", cfg.Server.ShutdownTimeout.Std(), smtpTimeout)
	}
	if cfg.Server.Keepalive.MaxConnectionAgeGrace.Std() <= smtpTimeout {
		return fmt.Errorf("server.keepalive.max_connection_age_grace (%s) must be > smtp.timeout (%s)",
			cfg.Server.Keepalive.MaxConnectionAgeGrace.Std(), smtpTimeout)
	}
	if cfg.Server.MaxRequestSizeMiB <= cfg.Mail.MaxAttachmentsSizeMiB {
		return fmt.Errorf("server.max_request_size_mib (%d) must be > mail.max_attachments_size_mib (%d)",
			cfg.Server.MaxRequestSizeMiB, cfg.Mail.MaxAttachmentsSizeMiB)
	}
	if n := estimateMetaLine(cfg.Mail); n > MetaLineLimit {
		return fmt.Errorf("mail.max_recipients and label limits allow a queue meta line of up to %d bytes, exceeding the %d byte limit", n, MetaLineLimit)
	}

	if err := validateSenders(cfg.Senders); err != nil {
		return err
	}
	return validateClients(cfg.Clients, cfg.Senders)
}

func validateServer(s ServerConfig) error {
	if _, _, err := net.SplitHostPort(strings.TrimSpace(s.ListenAddress)); err != nil {
		return fmt.Errorf("server.listen_address %q: %w", s.ListenAddress, err)
	}
	if err := requirePositive(
		named[Duration]{"server.connection_timeout", s.ConnectionTimeout},
		named[Duration]{"server.shutdown_timeout", s.ShutdownTimeout},
		named[Duration]{"server.keepalive.time", s.Keepalive.Time},
		named[Duration]{"server.keepalive.timeout", s.Keepalive.Timeout},
		named[Duration]{"server.keepalive.min_time", s.Keepalive.MinTime},
		named[Duration]{"server.keepalive.max_connection_age", s.Keepalive.MaxConnectionAge},
		named[Duration]{"server.keepalive.max_connection_age_grace", s.Keepalive.MaxConnectionAgeGrace},
	); err != nil {
		return err
	}
	if s.MaxRequestSizeMiB <= 0 {
		return errors.New("server.max_request_size_mib must be > 0")
	}
	return nil
}

func validateTLS(t TLSConfig) error {
	for _, f := range []named[string]{
		{"tls.cert_file", t.CertFile},
		{"tls.key_file", t.KeyFile},
		{"tls.client_ca_file", t.ClientCAFile},
	} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("%s must not be empty", f.name)
		}
	}
	if t.ExpiryWarning.Std() <= 0 {
		return errors.New("tls.expiry_warning must be > 0")
	}
	return nil
}

func validateSMTP(s SMTPConfig) error {
	if s.Timeout.Std() <= 0 {
		return errors.New("smtp.timeout must be > 0")
	}
	if s.MaxSyncConnections <= 0 {
		return errors.New("smtp.max_sync_connections must be > 0")
	}
	if s.SenderPause.Std() <= 0 {
		return errors.New("smtp.sender_pause must be > 0")
	}
	return nil
}

func validateQueue(q QueueConfig) error {
	if strings.TrimSpace(q.SpoolDir) == "" {
		return errors.New("queue.spool_dir must not be empty")
	}
	if err := requirePositive(
		named[Duration]{"queue.scan_interval", q.ScanInterval},
		named[Duration]{"queue.retry_initial_interval", q.RetryInitialInterval},
		named[Duration]{"queue.retry_max_interval", q.RetryMaxInterval},
		named[Duration]{"queue.max_schedule_ahead", q.MaxScheduleAhead},
	); err != nil {
		return err
	}
	if q.RetryMaxInterval.Std() < q.RetryInitialInterval.Std() {
		return fmt.Errorf("queue.retry_max_interval (%s) must be >= retry_initial_interval (%s)",
			q.RetryMaxInterval.Std(), q.RetryInitialInterval.Std())
	}
	return requirePositive(
		named[int]{"queue.workers", q.Workers},
		named[int]{"queue.max_attempts", q.MaxAttempts},
		named[int]{"queue.max_queued_mails", q.MaxQueuedMails},
	)
}

func validateMail(m MailConfig) error {
	return requirePositive(
		named[int]{"mail.max_recipients", m.MaxRecipients},
		named[int]{"mail.max_attachments_size_mib", m.MaxAttachmentsSizeMiB},
		named[int]{"mail.max_labels", m.MaxLabels},
		named[int]{"mail.max_label_key_length", m.MaxLabelKeyLength},
		named[int]{"mail.max_label_value_length", m.MaxLabelValueLength},
	)
}

// named 讓驗證錯誤能指出欄位名稱，並依宣告順序逐一檢查，錯誤訊息才會穩定。
type named[T any] struct {
	name  string
	value T
}

type positive interface{ ~int | ~int64 }

func requirePositive[T positive](fields ...named[T]) error {
	for _, f := range fields {
		if f.value <= 0 {
			return fmt.Errorf("%s must be > 0", f.name)
		}
	}
	return nil
}

// estimateMetaLine 估算佇列檔第一行在最壞情況下的長度。json.Marshal 最多把一個字元
// 展開成 6 bytes（例如 U+2028 會寫成  ），ASCII 的引號與反斜線則是 2 bytes；
// 收件者地址只會是 ASCII，labels 的 key 只會是英數字與少數符號。
func estimateMetaLine(m MailConfig) int {
	const fixedOverhead = 2048 // mail_id、client、sender、from、message_id 等固定欄位
	recipients := m.MaxRecipients * (message.MaxAddressLength*2 + 3)
	subject := MetaSubjectLimit * 6
	labels := m.MaxLabels * (m.MaxLabelKeyLength + m.MaxLabelValueLength*6 + 6)
	return fixedOverhead + recipients + subject + labels
}

func validateSenders(senders map[string]SenderConfig) error {
	if len(senders) == 0 {
		return errors.New("senders: at least one sender must be defined")
	}
	for _, alias := range slices.Sorted(maps.Keys(senders)) {
		s := senders[alias]
		if !senderAliasPattern.MatchString(alias) {
			return fmt.Errorf("senders.%s: alias may only contain letters, digits, '_' and '-'", alias)
		}
		if err := message.CheckAddress(s.Address); err != nil {
			return fmt.Errorf("senders.%s.address %q: %w", alias, s.Address, err)
		}
		if message.HasControl(s.Name) {
			return fmt.Errorf("senders.%s.name must not contain control characters", alias)
		}
		if err := validateSenderSMTP(alias, s.SMTP); err != nil {
			return err
		}
	}
	return nil
}

func validateSenderSMTP(alias string, s SenderSMTPConfig) error {
	prefix := "senders." + alias + ".smtp"
	if strings.TrimSpace(s.Host) == "" {
		return fmt.Errorf("%s.host must not be empty (every sender needs a [%s] table)", prefix, prefix)
	}
	if s.Port <= 0 || s.Port > 65535 {
		return fmt.Errorf("%s.port must be between 1 and 65535", prefix)
	}
	switch s.Security {
	case SecurityStartTLS, SecurityTLS, SecurityNone:
	default:
		return fmt.Errorf("%s.security %q must be one of %q, %q, %q", prefix, s.Security, SecurityStartTLS, SecurityTLS, SecurityNone)
	}
	if (s.Username == "") != (s.Password == "") {
		return fmt.Errorf("%s: username and password must be set together, or both left empty", prefix)
	}
	if s.Security == SecurityNone && s.Username != "" {
		// net/smtp 的 PlainAuth 不會在未加密的連線上送出密碼，設了也無法登入。
		return fmt.Errorf("%s: username and password must be empty when security is %q", prefix, SecurityNone)
	}
	return nil
}

func validateClients(clients map[string]ClientConfig, senders map[string]SenderConfig) error {
	if len(clients) == 0 {
		return errors.New("clients: at least one client must be defined")
	}
	for _, name := range slices.Sorted(maps.Keys(clients)) {
		c := clients[name]
		prefix := fmt.Sprintf("clients.%q", name)
		if strings.TrimSpace(name) == "" || message.HasControl(name) {
			return fmt.Errorf("%s: client name (certificate CN) must be non-empty and contain no control characters", prefix)
		}
		if len(c.Senders) == 0 {
			return fmt.Errorf("%s.senders must not be empty", prefix)
		}
		for i, alias := range c.Senders {
			if _, ok := senders[alias]; !ok {
				return fmt.Errorf("%s.senders[%d] %q is not defined in [senders]", prefix, i, alias)
			}
			if slices.Index(c.Senders, alias) != i {
				return fmt.Errorf("%s.senders lists %q more than once", prefix, alias)
			}
		}
		if !slices.Contains(c.Senders, c.DefaultSender) {
			return fmt.Errorf("%s.default_sender %q must be one of its senders", prefix, c.DefaultSender)
		}
		if c.RateLimit == nil {
			return fmt.Errorf("%s.rate_limit must be set (0 means unlimited)", prefix)
		}
		if *c.RateLimit < 0 {
			return fmt.Errorf("%s.rate_limit must be >= 0", prefix)
		}
		if c.RateLimitWindow.Std() <= 0 {
			return fmt.Errorf("%s.rate_limit_window must be > 0", prefix)
		}
	}
	return nil
}

// DefaultConfigPath 回傳 exe 同目錄底下的 conf/maild.toml 絕對路徑。
func DefaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join(DefaultConfigDir, DefaultConfigFileName)
	}
	return filepath.Join(filepath.Dir(exe), DefaultConfigDir, DefaultConfigFileName)
}
