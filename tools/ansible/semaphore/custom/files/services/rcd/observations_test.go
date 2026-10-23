// SPDX-License-Identifier: MIT
package rcd

import (
	"strings"
	"testing"
	"time"
)

func TestRCDObservationEvidence(t *testing.T) {
	yes, no := true, false
	build := &BuildObservation{SchemaVersion: 1, Component: "manager", Version: "1.2.3", ReleaseID: "r1", Commit: strings.Repeat("a", 40), SourceSHA256: strings.Repeat("b", 64), Target: "linux/amd64"}
	d := Device{ID: "device", DeviceBinding: DeviceBinding{HostAlias: "test"}}
	report := InspectionReport{SchemaVersion: 1, TaskID: 12, DeviceID: d.ID, HostAlias: d.HostAlias, Probes: []RuntimeProbe{
		{SchemaVersion: 1, Component: "manager", Processes: []RuntimeProcess{{Component: "manager", Running: &yes, RunningBuild: build, EntryBuild: build, RunningSHA256: strings.Repeat("a", 64), EntrySHA256: strings.Repeat("b", 64)}}},
		{SchemaVersion: 1, Component: "lower-stack", Processes: []RuntimeProcess{{Component: "agent", Running: &no, EntryBuild: build}, {Component: "lower", Running: nil}}},
	}}
	values, err := report.Normalize(d, 12)
	if err != nil {
		t.Fatal(err)
	}
	if values[0].State != "running" || !values[0].DiskDiffers || values[1].State != "stopped" || values[1].RunningBuild != nil || values[1].DiskBuild != nil || values[2].State != "unknown" {
		t.Fatal(values)
	}
	report.Probes[0].Processes[0].RunningBuild = nil
	values, err = report.Normalize(d, 12)
	if err != nil || values[0].State != "running_version_unknown" || values[0].RunningBuild != nil {
		t.Fatal(values, err)
	}
	report.Probes[0].InspectionError = "service changed during inspection"
	values, err = report.Normalize(d, 12)
	if err != nil || values[0].State != "unknown" || values[0].RunningBuild != nil {
		t.Fatal(values, err)
	}
	if _, err = report.Normalize(d, 11); err == nil {
		t.Fatal("wrong task accepted")
	}
	report.Probes[1].Processes = append(report.Probes[1].Processes, report.Probes[1].Processes[0])
	if _, err = report.Normalize(d, 12); err == nil {
		t.Fatal("duplicate process accepted")
	}
}

func TestRCDDeviceFreshness(t *testing.T) {
	now := time.Now().UTC()
	d := Device{}
	d.SetFreshness(now)
	if d.State != "never_checked" {
		t.Fatal(d)
	}
	d.LastSuccessAt = now.Add(-181 * time.Second).Format(time.RFC3339Nano)
	d.SetFreshness(now)
	if d.State != "stale" {
		t.Fatal(d)
	}
	d.LastSuccessAt = now.Format(time.RFC3339Nano)
	d.SetFreshness(now)
	if d.State != "checked" {
		t.Fatal(d)
	}
	d.LastError = "SSH failed"
	d.SetFreshness(now)
	if d.State != "check_failed" {
		t.Fatal(d)
	}
}
