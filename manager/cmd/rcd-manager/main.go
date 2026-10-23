package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"rcd_manager/internal/api"
	"rcd_manager/internal/app"
	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/config"
	"rcd_manager/internal/platform/secrets"
	"rcd_manager/internal/platform/service"
	"rcd_manager/internal/processagent"
	"rcd_manager/internal/store"
)

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rcd-manager:", err)
		os.Exit(1)
	}
}

func execute(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-version":
			return writeVersion(os.Stdout, "manager", args[1:])
		case "help", "--help", "-h":
			printUsage()
			return nil
		case "service":
			return serviceCommand(args[1:])
		case "web-reset-password":
			cfg, err := config.Load(args[1:])
			if err != nil {
				return err
			}
			repo, err := store.Open(context.Background(), cfg.DatabasePath, secrets.NewMachineProtector())
			if err != nil {
				return err
			}
			defer repo.Close()
			return api.ResetWebPassword(context.Background(), repo, os.Getenv("RCD_WEB_PASSWORD"))
		case "agent":
			return agentCommand(args[1:])
		case "run":
			args = args[1:]
		}
	}
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	runner := func(ctx context.Context) error { return app.RunProgram(ctx, cfg) }
	isService, err := service.IsService()
	if err != nil {
		return fmt.Errorf("detect Windows service context: %w", err)
	}
	if isService {
		return service.Run(runner)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runner(ctx)
}

func agentCommand(args []string) error {
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		return writeVersion(os.Stdout, "agent", args[1:])
	}
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	cfg, err := processagent.Load(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return processagent.Run(ctx, cfg)
}

func writeVersion(w io.Writer, component string, args []string) error {
	info := buildinfo.ForComponent(component)
	if len(args) == 1 && args[0] == "--json" {
		return json.NewEncoder(w).Encode(info)
	}
	if len(args) != 0 {
		return errors.New("usage: version [--json]")
	}
	label := "Manager"
	if component == "agent" {
		label = "Agent"
	}
	_, err := fmt.Fprintf(w, "RCD %s %s (%s, %s)\n", label, info.Version, info.Commit, info.BuiltAt)
	return err
}

func serviceCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("service action is required: install, start, stop, or uninstall")
	}
	switch strings.ToLower(args[0]) {
	case "install":
		// Validate the persisted process arguments before registering the service.
		cfg, err := config.Load(args[1:])
		if err != nil {
			return err
		}
		serviceArgs, err := cfg.ServiceArguments()
		if err != nil {
			return err
		}
		return service.Install(serviceArgs...)
	case "start":
		return service.Start()
	case "stop":
		return service.Stop()
	case "uninstall":
		return service.Uninstall()
	default:
		return fmt.Errorf("unknown service action %q", args[0])
	}
}

func printUsage() {
	fmt.Println(`RCD Manager

Usage:
  rcd-manager [run] [options]
  rcd-manager service install [options]
  rcd-manager service start|stop|uninstall
  rcd-manager agent run --instance-id ID --executable PATH --data-dir PATH [agent options]
  rcd-manager version [--json]
  rcd-manager agent version [--json]
  rcd-manager web-reset-password --data-dir PATH (stop manager first; use RCD_WEB_PASSWORD)

Runtime options:
  --data-dir PATH
  --opc-listen HOST:PORT
  --opc-advertise-host HOST
  --opc-username NAME
  --http-listen HOST:PORT
  --command-timeout DURATION
  --command-pending-timeout DURATION
  --shutdown-timeout DURATION
  --dev

Linux process-agent options:
  --work-dir PATH
  --http-listen HOST:PORT             (default 0.0.0.0:8090)
  --ready-address HOST:PORT           (optional TCP readiness, not OPC health)
  --auto-start=true|false             (first run only; restores desired state later)
  --restart-policy never|on-failure   (default never; no source failover)
  --stop-timeout DURATION
  --start-timeout DURATION

Secrets such as the upper OPC password are read from RCD_MANAGER_OPC_PASSWORD.`)
}
