package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/config"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/logging"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/platform/pki"
	"rcd_manager/internal/platform/secrets"
	"rcd_manager/internal/store"
	webassets "rcd_manager/web"
)

// RunProgram starts the lower clients, upper OPC UA server and embedded web
// console, and keeps them alive until ctx is cancelled or a listener fails.
func RunProgram(ctx context.Context, cfg config.Config) error {
	if err := cfg.EnsureDirectories(); err != nil {
		return err
	}
	logger, logCloser, err := logging.Setup(cfg.LogDir, cfg.Development)
	if err != nil {
		return err
	}
	defer logCloser.Close()
	slog.SetDefault(logger)

	pair, err := pki.Ensure(cfg.PKIDir, cfg.OPCAdvertiseHost)
	if err != nil {
		return err
	}
	repository, err := store.Open(ctx, cfg.DatabasePath, secrets.NewMachineProtector())
	if err != nil {
		return err
	}
	defer repository.Close()

	endpoint, err := advertisedEndpoint(cfg.OPCListen, cfg.OPCAdvertiseHost)
	if err != nil {
		return err
	}
	build := buildinfo.Current()
	runtime, err := NewRuntime(ctx, repository, RuntimeOptions{
		OPCEndpoint: endpoint, HTTPListen: cfg.HTTPListen, DataDir: cfg.DataDir, DatabasePath: cfg.DatabasePath, LogDir: cfg.LogDir,
		CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
		Version: build.Version, GitSHA: build.Commit, MaxSources: cfg.MaxSources, MaxSessions: cfg.MaxUpstreamSessions,
		CommandTimeout: cfg.CommandTimeout, CommandPendingTTL: cfg.CommandPendingTTL,
		PublishingInterval: cfg.LowerPublishingInterval, SamplingInterval: cfg.LowerSamplingInterval,
		AuditRetention: cfg.AuditRetention, MaxAuditRows: cfg.MaxAuditRows,
	})
	if err != nil {
		return err
	}
	backend := OPCBackend{Runtime: runtime}
	upstream, err := opc.NewUpstreamServer(opc.UpstreamConfig{
		EndpointURL: endpoint, ListenAddress: cfg.OPCListen,
		CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
		Username: cfg.OPCUsername, Password: cfg.OPCPassword, CommandTimeout: cfg.CommandTimeout,
		MaxSessions: uint32(cfg.MaxUpstreamSessions), MaxSubscriptions: uint32(cfg.MaxUpstreamSessions * 8),
		MaxWorkerThreads: 32, InsecureSkipVerify: true,
	}, backend)
	if err != nil {
		return fmt.Errorf("create upstream OPC server: %w", err)
	}
	runtime.SetRefreshHandler(func(sessionID, groupID string) {
		refreshed := upstream.ForceSessionRefresh(sessionID, groupID)
		if refreshed == 0 {
			slog.Debug("session route refresh matched no monitored items", "session_id", sessionID, "group_id", groupID)
			return
		}
		slog.Info("session route refresh queued", "session_id", sessionID, "group_id", groupID, "monitored_items", refreshed)
	})
	runtime.SetCalculationRefreshHandler(func(identifiers []string) {
		refreshed := upstream.ForceCalculationRefresh(identifiers)
		if refreshed > 0 {
			slog.Debug("calculation refresh queued", "changed_nodes", len(identifiers), "monitored_items", refreshed)
		}
	})
	runtime.SetSubscriptionCountHandler(upstream.SubscriptionCount)
	if err := runtime.SetCatalogHandlers(upstream.ValidateCatalog, upstream.ReconcileCatalog); err != nil {
		_ = upstream.Close()
		return fmt.Errorf("reconcile initial upstream catalog: %w", err)
	}
	runtime.SetSessionViewHandler(func() map[string]api.SessionView {
		views := make(map[string]api.SessionView)
		for _, session := range upstream.Sessions() {
			id := fmt.Sprint(session.SessionID)
			views[id] = api.SessionView{Session: domain.Session{LastSeenAt: session.LastAccess}, ClientName: session.SessionName,
				ClientApplicationURI: session.ClientApplicationURI, RemoteAddress: session.RemoteAddress}
		}
		return views
	})

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := runtime.Start(runContext); err != nil {
		_ = upstream.Close()
		return err
	}

	apiHandler := api.New(runtime, api.Options{Logger: logger})
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/healthz", apiHandler)
	mux.Handle("/readyz", apiHandler)
	mux.Handle("/metrics", apiHandler)
	mux.Handle("/", webassets.Handler())
	httpServer := &http.Server{
		Addr: cfg.HTTPListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20,
	}

	type serverResult struct {
		name string
		err  error
	}
	results := make(chan serverResult, 2)
	go func() { results <- serverResult{name: "http", err: httpServer.ListenAndServe()} }()
	go func() { results <- serverResult{name: "opc", err: upstream.ListenAndServe()} }()
	logger.Info("RCD Manager started", "opc_endpoint", endpoint, "http_listen", cfg.HTTPListen,
		"version", build.Version, "commit", build.Commit)

	var runErr error
	select {
	case <-ctx.Done():
	case result := <-results:
		if result.err != nil && !errors.Is(result.err, http.ErrServerClosed) {
			runErr = fmt.Errorf("%s server stopped: %w", result.name, result.err)
		}
	}
	cancel()
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()
	type shutdownResult struct {
		name string
		err  error
	}
	shutdowns := make(chan shutdownResult, 3)
	go func() { shutdowns <- shutdownResult{name: "http", err: httpServer.Shutdown(shutdownContext)} }()
	go func() { shutdowns <- shutdownResult{name: "opc", err: upstream.Close()} }()
	go func() { shutdowns <- shutdownResult{name: "runtime", err: runtime.CloseContext(shutdownContext)} }()
	for completed := 0; completed < 3; completed++ {
		select {
		case result := <-shutdowns:
			if result.err != nil && runErr == nil {
				runErr = fmt.Errorf("%s shutdown: %w", result.name, result.err)
			}
		case <-shutdownContext.Done():
			if runErr == nil {
				runErr = fmt.Errorf("shutdown exceeded %s: %w", cfg.ShutdownTimeout, shutdownContext.Err())
			}
			completed = 3
		}
	}
	logger.Info("RCD Manager stopped")
	return runErr
}

func advertisedEndpoint(listen, advertisedHost string) (string, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	host := net.JoinHostPort(advertisedHost, port)
	return (&url.URL{Scheme: "opc.tcp", Host: host}).String(), nil
}
