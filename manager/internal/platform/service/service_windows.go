//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func IsService() (bool, error) { return svc.IsWindowsService() }

func Run(runner Runner) error {
	return svc.Run(Name, &handler{runner: runner})
}

func Install(extraArgs ...string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()
	if existing, openErr := m.OpenService(Name); openErr == nil {
		existing.Close()
		return fmt.Errorf("service %s is already installed", Name)
	}
	args := append([]string{"run"}, extraArgs...)
	s, err := m.CreateService(Name, executable, mgr.Config{
		DisplayName:      DisplayName,
		Description:      "Aggregates RCD OPC UA servers and exposes a session-aware management endpoint.",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
	}, args...)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	return s.Close()
}

func Uninstall() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	status, queryErr := s.Query()
	if queryErr == nil && status.State != svc.Stopped {
		return fmt.Errorf("service must be stopped before uninstall (state %v)", status.State)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	return nil
}

func Start() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return waitForState(s, svc.Running, 30*time.Second)
}

func Stop() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	status, err := s.Query()
	if err != nil {
		return fmt.Errorf("query service: %w", err)
	}
	if status.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	return waitForState(s, svc.Stopped, 30*time.Second)
}

func openService() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("connect service manager: %w", err)
	}
	s, err := m.OpenService(Name)
	if err != nil {
		m.Disconnect()
		return nil, nil, fmt.Errorf("open service %s: %w", Name, err)
	}
	return m, s, nil
}

func waitForState(s *mgr.Service, target svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil {
			return fmt.Errorf("query service: %w", err)
		}
		if status.State == target {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("service did not reach state %v within %s", target, timeout)
}

type handler struct {
	runner Runner
}

func (h *handler) Execute(_ []string, changes <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	status <- svc.Status{State: svc.StartPending}
	go func() { done <- h.runner(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			status <- svc.Status{State: svc.StopPending}
			if err != nil && !errors.Is(err, context.Canceled) {
				return true, 1
			}
			return false, 0
		case request := <-changes:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				err := <-done
				if err != nil && !errors.Is(err, context.Canceled) {
					return true, 1
				}
				return false, 0
			}
		}
	}
}
