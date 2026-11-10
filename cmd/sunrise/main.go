package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/admin"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/buildinfo"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/cluster"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/config"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/logging"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/metrics"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/proxy"
)

const banner = `
  ____                       _
 / ___|  _   _  _ __   _ __ (_) ___   ___
 \___ \ | | | || '_ \ | '__|| |/ __| / _ \
  ___) || |_| || | | || |   | |\__ \|  __/
 |____/  \__,_||_| |_||_|   |_||___/ \___|
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sunrise:", err)
		os.Exit(1)
	}
}

func run() error {
	bootStart := time.Now()
	cfg, src, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}

	root := logging.New(logging.Options{
		Level: cfg.LogLevel,
		JSON:  cfg.LogJSON,
		Color: !cfg.LogJSON && logging.IsTerminal(os.Stdout),
	})
	log := root.With("component", "main")

	if !cfg.LogJSON {
		_, _ = fmt.Fprint(os.Stdout, banner+"\n")
	}
	mode := "standalone"
	if cfg.Clustered() {
		mode = "cluster"
	}
	log.Info("starting Sunrise",
		"version", buildinfo.Version, "commit", buildinfo.CommitHash(), "go", buildinfo.GoVersion(),
		"mode", mode, "node", cfg.NodeName)
	cfgLog := root.With("component", "config")
	if src.EnvFile != "" {
		cfgLog.Info("env file loaded", "path", src.EnvFile, "applied", len(src.FileKeys), "variables", strings.Join(src.FileKeys, ","))
	} else {
		cfgLog.Info("no .env file found, reading the environment only")
	}
	cfgLog.Info("variables set", "count", len(src.Set), "names", strings.Join(src.Set, ","), "others", "defaults")
	for _, sec := range cfg.Sections() {
		cfgLog.Info("effective "+sec.Name, sec.Attrs...)
	}

	srv := proxy.New(proxy.Options{
		RequestTimeout:     cfg.RequestTimeout,
		OutboundIP:         cfg.OutboundIP,
		DisableHTTP2:       cfg.DisableHTTP2,
		Concurrent:         cfg.AllowConcurrentRequests,
		MaxBearers:         cfg.MaxBearerCount,
		GlobalLimit:        cfg.GlobalRatelimit,
		Overrides:          cfg.BotOverrides,
		Disable401Lock:     cfg.Disable401Lock,
		TokenLockDuration:  cfg.TokenLockDuration,
		Webhook404Duration: cfg.Webhook404Duration,
		WarnThresholds:     cfg.InvalidWarnThresholds,
		HardBrake:          cfg.EnableHardBrake,
		HardBrakeAt:        cfg.HardBrakeThreshold,
		Clustered:          cfg.Clustered(),
	}, root)

	var node atomic.Pointer[cluster.Node]
	m := metrics.New(buildinfo.Version, buildinfo.CommitHash(), buildinfo.GoVersion(), metrics.Sources{
		Buckets:       srv.Registry.Stats,
		InvalidWindow: func() int { return srv.Invalid.Count(time.Now()) },
		LockedTokens:  srv.Locks.Len,
		DeadWebhooks:  srv.Webhooks.Len,
		Members: func() int {
			if n := node.Load(); n != nil {
				return n.Ring().Len()
			}
			return 1
		},
	})
	srv.SetMetrics(m)
	srv.LogBoot()
	if families, err := m.Registry.Gather(); err == nil {
		endpoints := "/metrics,/healthz,/readyz"
		if cfg.EnablePprof {
			endpoints += ",/debug/pprof/"
		}
		root.With("component", "metrics").Info("metrics registered", "families", len(families), "endpoints", endpoints, "enabled", cfg.EnableMetrics)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bg, cancelBG := context.WithCancel(context.Background())
	defer cancelBG()
	srv.Run(bg)
	log.Info("background tasks started", "tasks", "bucket sweeper (every 1m), invalid response window (every 1s)")

	var ready atomic.Bool
	readyFn := func() bool {
		if !ready.Load() {
			return false
		}
		if cfg.Clustered() {
			n := node.Load()
			return n != nil && n.Joined()
		}
		return true
	}

	proxyAddr := net.JoinHostPort(cfg.BindIP, strconv.Itoa(cfg.Port))
	proxyLn, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", proxyAddr, err)
	}
	proxyServer := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(root.With("component", "http").Handler(), slog.LevelDebug),
	}
	errs := make(chan error, 2)
	go func() { errs <- serve(proxyServer, proxyLn) }()
	log.Info("proxy listening", "address", proxyLn.Addr().String())

	var adminServer *http.Server
	if cfg.EnableMetrics {
		adminAddr := net.JoinHostPort(cfg.BindIP, strconv.Itoa(cfg.MetricsPort))
		adminLn, err := net.Listen("tcp", adminAddr)
		if err != nil {
			return fmt.Errorf("listening on %s: %w", adminAddr, err)
		}
		adminServer = &http.Server{
			Handler:           admin.Handler(m.Registry, readyFn, cfg.EnablePprof),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() { errs <- serve(adminServer, adminLn) }()
		log.Info("metrics listening", "address", adminLn.Addr().String(), "pprof", cfg.EnablePprof)
	}
	ready.Store(true)

	if cfg.Clustered() {
		clLog := root.With("component", "cluster")
		n, err := cluster.Start(cluster.Options{
			Name:          cfg.NodeName,
			BindIP:        cfg.BindIP,
			Port:          cfg.ClusterPort,
			AdvertiseAddr: cfg.ClusterAdvertiseAddr,
			ProxyPort:     cfg.Port,
			Version:       buildinfo.Version,
			Secret:        cfg.ClusterSecret,
			Members:       cfg.ClusterMembers,
			DNS:           cfg.ClusterDNS,
			DNSInterval:   cfg.ClusterDNSInterval,
			Log:           clLog,
			GossipLog:     root.With("component", "gossip"),
			OnChange:      srv.MembershipChanged,
		})
		if err != nil {
			return err
		}
		node.Store(n)
		srv.AttachCluster(n, cluster.NewSigner(cfg.ClusterSecret, cfg.NodeName))
		clLog.Info("request signing ready", "algorithm", "HMAC-SHA256", "max_age", cluster.MaxSignatureAge,
			"internal_prefix", "/_sunrise/")
		clLog.Info("routing ready", "strategy", "rendezvous hashing",
			"owner_key", "bot + channel/guild/webhook, else bot + route; Bearer by token",
			"handled_locally", "interaction callbacks, GET /gateway")
		n.Join()
		go n.Run(bg)
		clLog.Info("discovery loop started", "every", cfg.ClusterDNSInterval)
	}
	log.Info("Sunrise is ready", "startup", time.Since(bootStart).Round(time.Millisecond))

	select {
	case <-ctx.Done():
	case err := <-errs:
		return err
	}

	log.Info("shutdown signal received")
	ready.Store(false)
	if n := node.Load(); n != nil {
		log.Info("leaving the cluster")
		n.Leave(5 * time.Second)
	}
	log.Info("waiting for in-flight requests", "timeout", cfg.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := proxyServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("in-flight requests did not finish in time", "error", err)
	}
	if adminServer != nil {
		_ = adminServer.Shutdown(shutdownCtx)
	}
	log.Info("stopped")
	return nil
}

func serve(s *http.Server, ln net.Listener) error {
	if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthcheck asks the local metrics port for readiness. The image has no shell or
// curl, so container health checks call the binary itself.
func healthcheck() int {
	port := os.Getenv(config.Prefix + "METRICS_PORT")
	if port == "" {
		port = "9000"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/readyz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "not ready:", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "not ready: status", resp.StatusCode)
		return 1
	}
	return 0
}
