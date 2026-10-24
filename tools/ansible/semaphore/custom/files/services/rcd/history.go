// SPDX-License-Identifier: MIT
package rcd

import (
	"context"
	"math"
	"sort"
	"time"
)

type Installation struct {
	PackageID   string
	OperationID string
	InstalledAt string
	Kind        string
}

func verifiedStartedImage(o Operation, device, component, hash string, installedAt time.Time) bool {
	if !o.ServiceControl() || o.Request.Component != component || o.State != "succeeded" || o.TaskID < 1 ||
		(o.Request.Action != "start" && o.Request.Action != "restart") {
		return false
	}
	started, err := time.Parse(time.RFC3339Nano, o.CreatedAt)
	if err != nil || started.Before(installedAt) {
		return false
	}
	for _, target := range o.Targets {
		if target.DeviceID != device || target.State != "succeeded" || target.ExpectedBinarySHA256 != hash ||
			target.Acceptance["binary_sha256"] != hash || target.Acceptance["state"] != "running" {
			continue
		}
		pid, ok := target.Acceptance["pid"].(float64)
		if ok && !math.IsNaN(pid) && !math.IsInf(pid, 0) && pid > 0 && pid <= math.MaxInt32 && math.Trunc(pid) == pid {
			return true
		}
	}
	return false
}

// First install deliberately preserves stopped state. It becomes a recoverable
// version only after a later native start/restart proves this device's exact
// running image; merely finding a package or a stopped entry is insufficient.
func (r *Repository) Installations(ctx context.Context, project int, device, component string) ([]Installation, error) {
	operations, err := r.Operations(ctx, project)
	if err != nil {
		return nil, err
	}
	result := []Installation{}
	for _, operation := range operations {
		if operation.Package.Component != component || !digestPattern.MatchString(operation.Request.PackageID) {
			continue
		}
		for _, target := range operation.Targets {
			if target.DeviceID == device && target.State == "succeeded" {
				result = append(result, Installation{PackageID: operation.Request.PackageID, OperationID: operation.ID, InstalledAt: operation.CreatedAt, Kind: "release"})
				break
			}
		}
	}
	if IsApplicationComponent(component) {
		firstInstallSeen := map[string]bool{}
		setups, err := r.HostSetups(ctx, project)
		if err != nil {
			return nil, err
		}
		for _, setup := range setups {
			install := setup.Install
			if setup.State != "installed_stopped" || setup.DeviceID != device || install == nil || install.Component != component ||
				!digestPattern.MatchString(install.PackageID) || !digestPattern.MatchString(install.BinarySHA256) || firstInstallSeen[install.PackageID] {
				continue
			}
			installedAt, err := time.Parse(time.RFC3339Nano, setup.UpdatedAt)
			if err != nil {
				continue
			}
			pkg, err := r.Get(ctx, project, install.PackageID)
			if err != nil || pkg.Manifest.Component != component || pkg.Manifest.BinarySHA256 != install.BinarySHA256 {
				continue
			}
			var firstStart *Operation
			for i := range operations {
				operation := &operations[i]
				if !verifiedStartedImage(*operation, device, component, install.BinarySHA256, installedAt) {
					continue
				}
				if firstStart == nil {
					firstStart = operation
					continue
				}
				started, _ := time.Parse(time.RFC3339Nano, operation.CreatedAt)
				previous, _ := time.Parse(time.RFC3339Nano, firstStart.CreatedAt)
				if started.Before(previous) || (started.Equal(previous) && operation.ID < firstStart.ID) {
					firstStart = operation
				}
			}
			if firstStart != nil {
				firstInstallSeen[install.PackageID] = true
				result = append(result, Installation{PackageID: install.PackageID, OperationID: firstStart.ID, InstalledAt: firstStart.CreatedAt, Kind: "first_install_verified_start"})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, result[i].InstalledAt)
		b, _ := time.Parse(time.RFC3339Nano, result[j].InstalledAt)
		if a.Equal(b) {
			return result[i].OperationID > result[j].OperationID
		}
		return a.After(b)
	})
	return result, nil
}
