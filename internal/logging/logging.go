package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const LevelTrace = slog.Level(-8)

type Options struct {
	Level  slog.Level
	JSON   bool
	Color  bool
	Output io.Writer
}

func New(opts Options) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = os.Stdout
	}
	if opts.JSON {
		return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{
			Level:       opts.Level,
			ReplaceAttr: jsonReplace,
		}))
	}
	return slog.New(&textHandler{
		mu:    &sync.Mutex{},
		out:   out,
		level: opts.Level,
		color: opts.Color,
	})
}

// IsTerminal reports whether f is a character device, which is how we decide to color output.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (use trace, debug, info, warn or error)", s)
}

func LevelName(l slog.Level) string {
	switch {
	case l < slog.LevelDebug:
		return "TRACE"
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARN"
	}
	return "ERROR"
}

var (
	pathTokenRe = regexp.MustCompile(`(/(?:webhooks|interactions)/[^/\s?"]+/)[^/\s?"]+`)
	authRe      = regexp.MustCompile(`\b(Bot|Bearer) [A-Za-z0-9._~+/=-]{20,}`)
	botTokenRe  = regexp.MustCompile(`[A-Za-z0-9_-]{18,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{20,}`)
)

// Redact removes webhook and interaction tokens from paths and anything that looks like
// an Authorization value or a bot token. Every log line passes through it.
func Redact(s string) string {
	if strings.Contains(s, "/webhooks/") || strings.Contains(s, "/interactions/") {
		s = pathTokenRe.ReplaceAllString(s, "${1}:token")
	}
	if strings.Contains(s, "Bot ") || strings.Contains(s, "Bearer ") {
		s = authRe.ReplaceAllString(s, "${1} :token")
	}
	if strings.Count(s, ".") >= 2 {
		s = botTokenRe.ReplaceAllString(s, ":token")
	}
	return s
}

func jsonReplace(_ []string, a slog.Attr) slog.Attr {
	switch {
	case a.Key == slog.LevelKey:
		if l, ok := a.Value.Any().(slog.Level); ok {
			return slog.String(slog.LevelKey, strings.ToLower(LevelName(l)))
		}
	case a.Value.Kind() == slog.KindString:
		return slog.String(a.Key, Redact(a.Value.String()))
	case a.Value.Kind() == slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, Redact(err.Error()))
		}
	}
	return a
}

type textHandler struct {
	mu        *sync.Mutex
	out       io.Writer
	level     slog.Level
	color     bool
	component string
	attrs     []byte
	prefix    string
}

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[90m"
	ansiCyan   = "\x1b[1;36m"
	ansiBlue   = "\x1b[34m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[1;31m"
)

func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	buf := make([]byte, 0, 256)
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	buf = h.paint(buf, ansiDim, t.Format("15:04:05.000"))
	buf = append(buf, ' ')

	component := h.component
	if component == "" {
		component = "sunrise"
	}
	buf = h.paint(buf, ansiCyan, pad(component, 9))
	buf = append(buf, ' ')

	name := LevelName(r.Level)
	buf = h.paint(buf, levelColor(name), pad(name, 5))
	buf = append(buf, ' ')
	buf = append(buf, Redact(r.Message)...)

	buf = append(buf, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, h.prefix, a)
		return true
	})
	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.out.Write(buf)
	return err
}

func (h *textHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append([]byte(nil), h.attrs...)
	for _, a := range as {
		if a.Key == "component" && h.prefix == "" {
			n.component = a.Value.String()
			continue
		}
		n.attrs = n.appendAttr(n.attrs, h.prefix, a)
	}
	return &n
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.prefix = h.prefix + name + "."
	return &n
}

func (h *textHandler) appendAttr(buf []byte, prefix string, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			buf = h.appendAttr(buf, p, ga)
		}
		return buf
	}
	buf = append(buf, ' ')
	buf = h.paint(buf, ansiDim, prefix+a.Key+"=")
	return append(buf, formatValue(a.Value)...)
}

func formatValue(v slog.Value) string {
	var s string
	switch v.Kind() {
	case slog.KindString:
		s = v.String()
	case slog.KindDuration:
		return v.Duration().Round(time.Microsecond).String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			s = err.Error()
		} else {
			s = fmt.Sprint(v.Any())
		}
	default:
		return v.String()
	}
	s = Redact(s)
	if s == "" || strings.ContainsAny(s, " \t\n\"=") || !utf8.ValidString(s) {
		return strconv.Quote(s)
	}
	return s
}

func (h *textHandler) paint(buf []byte, color, s string) []byte {
	if !h.color {
		return append(buf, s...)
	}
	buf = append(buf, color...)
	buf = append(buf, s...)
	return append(buf, ansiReset...)
}

func levelColor(name string) string {
	switch name {
	case "TRACE":
		return ansiDim
	case "DEBUG":
		return ansiBlue
	case "INFO":
		return ansiGreen
	case "WARN":
		return ansiYellow
	}
	return ansiRed
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}
