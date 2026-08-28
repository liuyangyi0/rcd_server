package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"rcd_manager/internal/app"
	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/config"
	"rcd_manager/internal/platform/service"
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
			info := buildinfo.Current()
			fmt.Printf("RCD Manager %s (%s, %s)\n", info.Version, info.Commit, info.BuiltAt)
			return nil
		case "help", "--help", "-h":
			printUsage()
			return nil
		case "service":
			return serviceCommand(args[1:])
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
  rcd-manager version

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

Secrets such as the upper OPC password are read from RCD_MANAGER_OPC_PASSWORD.`)
}
