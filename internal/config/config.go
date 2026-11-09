package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/logging"
)

const (
	Prefix          = "SUNRISE_"
	MinSecretLength = 32
)

type Config struct {
	LogLevel slog.Level
	LogJSON  bool

	BindIP        string
	Port          int
	MetricsPort   int
	EnableMetrics bool
	EnablePprof   bool

	OutboundIP              string
	RequestTimeout          time.Duration
	DisableHTTP2            bool
	AllowConcurrentRequests bool

	GlobalRatelimit int
	BotOverrides    map[string]int
	MaxBearerCount  int

	Disable401Lock        bool
	TokenLockDuration     time.Duration
	Webhook404Duration    time.Duration
	InvalidWarnThresholds []int
	EnableHardBrake       bool
	HardBrakeThreshold    int
	ShutdownTimeout       time.Duration

	NodeName             string
	ClusterPort          int
	ClusterMembers       []string
	ClusterDNS           string
	ClusterDNSInterval   time.Duration
	ClusterSecret        string
	ClusterAdvertiseAddr string
}

func (c *Config) Clustered() bool {
	return len(c.ClusterMembers) > 0 || c.ClusterDNS != ""
}

// Load reads the configuration from the environment, after merging an optional .env file
// in the working directory. Variables already set in the environment win over the file.
func Load() (*Config, *Source, error) {
	src := &Source{}
	keys, err := loadDotEnv(".env")
	if err != nil {
		return nil, nil, err
	}
	if keys != nil {
		src.EnvFile = ".env"
		src.FileKeys = keys
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, Prefix) {
			src.Set = append(src.Set, k)
		}
	}
	sort.Strings(src.Set)
	c, err := Parse(os.LookupEnv)
	return c, src, err
}

// Source says where the configuration came from, by variable name only.
type Source struct {
	EnvFile  string
	FileKeys []string
	Set      []string
}

type parser struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (p *parser) raw(name string) (string, bool) {
	v, ok := p.lookup(Prefix + name)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (p *parser) fail(name, format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf("%s%s: %s", Prefix, name, fmt.Sprintf(format, args...)))
}

func (p *parser) str(name, def string) string {
	if v, ok := p.raw(name); ok {
		return v
	}
	return def
}

func (p *parser) integer(name string, def, minVal, maxVal int) int {
	v, ok := p.raw(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.fail(name, "expected a whole number, got %q", v)
		return def
	}
	if n < minVal || n > maxVal {
		p.fail(name, "must be between %d and %d, got %d", minVal, maxVal, n)
		return def
	}
	return n
}

func (p *parser) millis(name string, def time.Duration, allowZero bool) time.Duration {
	v, ok := p.raw(name)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || (n == 0 && !allowZero) {
		p.fail(name, "expected a positive number of milliseconds, got %q", v)
		return def
	}
	return time.Duration(n) * time.Millisecond
}

func (p *parser) boolean(name string, def bool) bool {
	v, ok := p.raw(name)
	if !ok {
		return def
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	p.fail(name, "expected true or false, got %q", v)
	return def
}

func (p *parser) list(name string) []string {
	v, ok := p.raw(name)
	if !ok {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (p *parser) ip(name string) string {
	v, ok := p.raw(name)
	if !ok {
		return ""
	}
	if net.ParseIP(v) == nil {
		p.fail(name, "expected an IP address, got %q", v)
		return ""
	}
	return v
}

func Parse(lookup func(string) (string, bool)) (*Config, error) {
	p := &parser{lookup: lookup}
	c := &Config{}

	level, err := logging.ParseLevel(p.str("LOG_LEVEL", "info"))
	if err != nil {
		p.fail("LOG_LEVEL", "%v", err)
	}
	c.LogLevel = level
	switch f := strings.ToLower(p.str("LOG_FORMAT", "text")); f {
	case "text":
	case "json":
		c.LogJSON = true
	default:
		p.fail("LOG_FORMAT", "expected text or json, got %q", f)
	}

	c.BindIP = p.str("BIND_IP", "0.0.0.0")
	if net.ParseIP(c.BindIP) == nil {
		p.fail("BIND_IP", "expected an IP address, got %q", c.BindIP)
	}
	c.Port = p.integer("PORT", 8080, 1, 65535)
	c.MetricsPort = p.integer("METRICS_PORT", 9000, 1, 65535)
	c.EnableMetrics = p.boolean("ENABLE_METRICS", true)
	c.EnablePprof = p.boolean("ENABLE_PPROF", false)
	if c.EnableMetrics && c.Port == c.MetricsPort {
		p.fail("METRICS_PORT", "must differ from %sPORT (%d)", Prefix, c.Port)
	}
	if c.EnablePprof && !c.EnableMetrics {
		p.fail("ENABLE_PPROF", "needs %sENABLE_METRICS=true because profiling is served on the metrics port", Prefix)
	}

	c.OutboundIP = p.ip("OUTBOUND_IP")
	c.RequestTimeout = p.millis("REQUEST_TIMEOUT", 15*time.Second, false)
	c.DisableHTTP2 = p.boolean("DISABLE_HTTP_2", true)
	c.AllowConcurrentRequests = p.boolean("ALLOW_CONCURRENT_REQUESTS", true)

	c.GlobalRatelimit = p.integer("GLOBAL_RATELIMIT", 50, 1, 1_000_000)
	c.BotOverrides = parseOverrides(p)
	c.MaxBearerCount = p.integer("MAX_BEARER_COUNT", 1024, 1, 10_000_000)

	c.Disable401Lock = p.boolean("DISABLE_401_LOCK", false)
	c.TokenLockDuration = p.millis("TOKEN_LOCK_DURATION", 0, true)
	c.Webhook404Duration = p.millis("WEBHOOK_404_DURATION", 24*time.Hour, false)
	c.InvalidWarnThresholds = parseThresholds(p)
	c.EnableHardBrake = p.boolean("ENABLE_HARD_BRAKE", false)
	c.HardBrakeThreshold = p.integer("HARD_BRAKE_THRESHOLD", 9500, 1, 10_000)
	c.ShutdownTimeout = p.millis("SHUTDOWN_TIMEOUT", 20*time.Second, false)

	host, _ := os.Hostname()
	c.NodeName = p.str("NODE_NAME", host)
	c.ClusterPort = p.integer("CLUSTER_PORT", 7946, 1, 65535)
	c.ClusterMembers = p.list("CLUSTER_MEMBERS")
	c.ClusterDNS = p.str("CLUSTER_DNS", "")
	c.ClusterDNSInterval = p.millis("CLUSTER_DNS_INTERVAL", 30*time.Second, false)
	c.ClusterSecret, _ = p.raw("CLUSTER_SECRET")
	c.ClusterAdvertiseAddr = p.ip("CLUSTER_ADVERTISE_ADDR")

	if c.Clustered() {
		if c.ClusterSecret == "" {
			p.fail("CLUSTER_SECRET", "is required in cluster mode (%sCLUSTER_MEMBERS or %sCLUSTER_DNS is set)", Prefix, Prefix)
		} else if len(c.ClusterSecret) < MinSecretLength {
			p.fail("CLUSTER_SECRET", "must be at least %d characters long, got %d", MinSecretLength, len(c.ClusterSecret))
		}
		if c.NodeName == "" {
			p.fail("NODE_NAME", "is empty and the hostname could not be read")
		}
		if c.ClusterPort == c.Port || (c.EnableMetrics && c.ClusterPort == c.MetricsPort) {
			p.fail("CLUSTER_PORT", "must differ from the proxy and metrics ports")
		}
	}

	if len(p.errs) > 0 {
		return nil, errors.Join(p.errs...)
	}
	return c, nil
}

func parseOverrides(p *parser) map[string]int {
	out := map[string]int{}
	for _, entry := range p.list("BOT_RATELIMIT_OVERRIDES") {
		id, limit, ok := strings.Cut(entry, ":")
		if !ok {
			p.fail("BOT_RATELIMIT_OVERRIDES", "entry %q is not in the form botid:limit", entry)
			continue
		}
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			p.fail("BOT_RATELIMIT_OVERRIDES", "bot id %q is not a number", id)
			continue
		}
		n, err := strconv.Atoi(limit)
		if err != nil || n < 1 {
			p.fail("BOT_RATELIMIT_OVERRIDES", "limit %q for bot %s is not a positive number", limit, id)
			continue
		}
		out[id] = n
	}
	return out
}

func parseThresholds(p *parser) []int {
	raw := p.list("INVALID_REQUEST_WARN_THRESHOLDS")
	if raw == nil {
		return []int{2500, 5000, 7500}
	}
	var out []int
	for _, s := range raw {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 10_000 {
			p.fail("INVALID_REQUEST_WARN_THRESHOLDS", "%q is not a number between 1 and 10000", s)
			continue
		}
		if len(out) > 0 && n <= out[len(out)-1] {
			p.fail("INVALID_REQUEST_WARN_THRESHOLDS", "values must be ascending")
			continue
		}
		out = append(out, n)
	}
	return out
}

// Section is one group of settings for the boot log. Secrets never appear here.
type Section struct {
	Name  string
	Attrs []any
}

func (c *Config) Sections() []Section {
	tokenLock := "until restart"
	if c.TokenLockDuration > 0 {
		tokenLock = c.TokenLockDuration.String()
	}
	if c.Disable401Lock {
		tokenLock = "disabled"
	}
	out := []Section{
		{"listeners", []any{
			"bind_ip", c.BindIP, "port", c.Port, "metrics", c.EnableMetrics, "metrics_port", c.MetricsPort, "pprof", c.EnablePprof,
		}},
		{"upstream", []any{
			"request_timeout", c.RequestTimeout, "http2", !c.DisableHTTP2, "outbound_ip", orDefault(c.OutboundIP, "system default"),
		}},
		{"ratelimits", []any{
			"global_per_second", c.GlobalRatelimit, "bot_overrides", len(c.BotOverrides),
			"concurrent_requests", c.AllowConcurrentRequests, "max_bearer_count", c.MaxBearerCount,
		}},
		{"protection", []any{
			"token_lock_after_401", tokenLock, "webhook_404_for", c.Webhook404Duration,
			"invalid_warn_at", fmt.Sprint(c.InvalidWarnThresholds), "hard_brake", c.EnableHardBrake,
			"hard_brake_at", c.HardBrakeThreshold,
		}},
		{"logging", []any{"level", logging.LevelName(c.LogLevel), "format", map[bool]string{true: "json", false: "text"}[c.LogJSON]}},
		{"shutdown", []any{"timeout", c.ShutdownTimeout}},
	}
	if c.Clustered() {
		secret := "unset"
		if c.ClusterSecret != "" {
			secret = fmt.Sprintf("set (%d characters, redacted)", len(c.ClusterSecret))
		}
		out = append(out, Section{"cluster", []any{
			"node", c.NodeName, "gossip_port", c.ClusterPort,
			"members", orDefault(strings.Join(c.ClusterMembers, ","), "none"),
			"dns", orDefault(c.ClusterDNS, "none"), "dns_interval", c.ClusterDNSInterval,
			"advertise", orDefault(c.ClusterAdvertiseAddr, "auto"), "secret", secret,
		}})
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func loadDotEnv(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	line := 0
	var applied []string
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		key, value, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("%s line %d: expected KEY=VALUE", path, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return nil, fmt.Errorf("%s line %d: %w", path, line, err)
			}
			applied = append(applied, key)
		}
	}
	if applied == nil {
		applied = []string{}
	}
	return applied, sc.Err()
}
