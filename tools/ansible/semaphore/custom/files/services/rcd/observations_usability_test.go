// SPDX-License-Identifier: MIT
package rcd

import (
	"strings"
	"testing"
)

func TestRCDObservationPreservesDiskFailureAlongsideRuntimeEvidence(t *testing.T) {
	yes, no := true, false
	device := Device{ID: "device", DeviceBinding: DeviceBinding{HostAlias: "fixture"}}
	build := &BuildObservation{SchemaVersion: 1, Component: "manager", Version: "1.0.0", ReleaseID: "fixture", Commit: strings.Repeat("a", 40), SourceSHA256: strings.Repeat("b", 64), Target: "linux/amd64"}
	for _, scenario := range []struct {
		name          string
		process       RuntimeProcess
		load          string
		state         string
		errorFragment string
	}{
		{"stopped_permission_denied", RuntimeProcess{Component: "manager", Running: &no, EntryError: "permission denied"}, "loaded", "stopped", "磁盘程序检查失败：permission denied"},
		{"stopped_damaged_entry", RuntimeProcess{Component: "manager", Running: &no, EntryError: "not a regular file"}, "loaded", "stopped", "not a regular file"},
		{"running_disk_unreadable", RuntimeProcess{Component: "manager", Running: &yes, RunningBuild: build, RunningSHA256: strings.Repeat("a", 64), EntryError: "disk unreadable"}, "loaded", "running", "disk unreadable"},
		{"two_distinct_failures", RuntimeProcess{Component: "manager", Running: &yes, RunningError: "runtime denied", EntryError: "disk denied"}, "loaded", "running_version_unknown", "runtime denied；磁盘程序检查失败：disk denied"},
		{"genuinely_not_installed", RuntimeProcess{Component: "manager", Running: &no, Installed: &no}, "not-found", "not_installed", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			report := InspectionReport{SchemaVersion: 1, DeviceID: device.ID, HostAlias: device.HostAlias, TaskID: 12, Probes: []RuntimeProbe{
				{SchemaVersion: 1, Component: "manager", Processes: []RuntimeProcess{scenario.process}},
				{SchemaVersion: 1, Component: "lower-stack", Processes: []RuntimeProcess{{Component: "lower", Running: &no}, {Component: "agent", Running: &no}}},
			}}
			report.Probes[0].Service.LoadState = scenario.load
			values, err := report.Normalize(device, 12)
			if err != nil || values[0].State != scenario.state || !strings.Contains(values[0].Error, scenario.errorFragment) {
				t.Fatal(values, err)
			}
			if scenario.name == "genuinely_not_installed" && values[0].Error != "" {
				t.Fatal(values[0])
			}
			if scenario.name == "running_disk_unreadable" && (values[0].RunningBuild == nil || values[0].DiskBuild != nil || values[0].DiskSHA256 != "") {
				t.Fatal("disk failure replaced actual running identity", values[0])
			}
		})
	}
}
