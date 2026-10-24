// SPDX-License-Identifier: MIT
package rcd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

type BuildObservation struct {
	SchemaVersion int    `json:"schema_version"`
	Component     string `json:"component"`
	Version       string `json:"version"`
	ReleaseID     string `json:"release_id"`
	Commit        string `json:"commit"`
	BuiltAt       string `json:"built_at"`
	SourceSHA256  string `json:"source_sha256"`
	Dirty         bool   `json:"dirty"`
	Target        string `json:"target"`
}
type ComponentObservation struct {
	Compatibility *CompatibilityObservation `json:"compatibility,omitempty"`
	Component     string                    `json:"component"`
	State         string                    `json:"state"`
	RunningBuild  *BuildObservation         `json:"running_build"`
	DiskBuild     *BuildObservation         `json:"disk_build"`
	RunningSHA256 string                    `json:"running_sha256"`
	DiskSHA256    string                    `json:"disk_sha256"`
	DiskDiffers   bool                      `json:"disk_differs"`
	Error         string                    `json:"error"`
}
type CompatibilityObservation struct {
	InstanceID        string         `json:"instance_id,omitempty"`
	GatewayInstanceID string         `json:"gateway_instance_id,omitempty"`
	IdentityError     string         `json:"identity_error,omitempty"`
	StateFormats      map[string]int `json:"state_formats"`
	AdapterVersion    string         `json:"adapter_version"`
	Error             string         `json:"error"`
}
type ReleaseInspection struct {
	SchemaVersion int                                 `json:"schema_version"`
	Compatibility map[string]CompatibilityObservation `json:"compatibility"`
	ReleaseReport json.RawMessage                     `json:"release_report"`
}
type RuntimeProcess struct {
	Component     string            `json:"component"`
	Running       *bool             `json:"running"`
	RunningBuild  *BuildObservation `json:"running_build"`
	EntryBuild    *BuildObservation `json:"entry_build"`
	RunningSHA256 string            `json:"running_sha256"`
	EntrySHA256   string            `json:"entry_sha256"`
	RunningError  string            `json:"running_error"`
	EntryError    string            `json:"entry_error"`
	Installed     *bool             `json:"installed,omitempty"`
}
type RuntimeProbe struct {
	SchemaVersion   int              `json:"schema_version"`
	Component       string           `json:"component"`
	InspectionError string           `json:"inspection_error"`
	Processes       []RuntimeProcess `json:"processes"`
	Service         struct {
		LoadState string `json:"LoadState"`
	} `json:"service"`
}
type InspectionReport struct {
	ReleaseInfo   *ReleaseInspection `json:"release_info,omitempty"`
	SchemaVersion int                `json:"schema_version"`
	DeviceID      string             `json:"device_id"`
	TaskID        int                `json:"task_id"`
	HostAlias     string             `json:"host_alias"`
	Probes        []RuntimeProbe     `json:"probes"`
	HostStatus    json.RawMessage    `json:"host_status,omitempty"`
}

// Only hash-backed, correctly typed build claims can become a displayed version.
// Missing release identity is "unknown", never the on-disk file's version.
func trustworthyBuild(build *BuildObservation, component, hash string) *BuildObservation {
	if build == nil || !digestPattern.MatchString(hash) || build.SchemaVersion != 1 || build.Component != component || !identifier.MatchString(build.Version) || !identifier.MatchString(build.ReleaseID) || !commitPattern.MatchString(build.Commit) || !digestPattern.MatchString(build.SourceSHA256) || Architecture(build.Target) == "" {
		return nil
	}
	return build
}
func (report InspectionReport) Normalize(device Device, task int) ([]ComponentObservation, error) {
	if report.SchemaVersion != 1 || report.TaskID != task || report.DeviceID != device.ID || report.HostAlias != device.HostAlias || len(report.Probes) < 2 || len(report.Probes) > 35 {
		return nil, fmt.Errorf("%w: inspection binding mismatch", ErrInvalid)
	}
	seen := map[string]bool{}
	probes := map[string]bool{}
	out := []ComponentObservation{}
	for _, probe := range report.Probes {
		if probe.SchemaVersion != 1 || (probe.Component != "manager" && probe.Component != "lower-stack" && probe.Component != "communicate" && !IsApplicationComponent(probe.Component)) || probes[probe.Component] {
			return nil, ErrInvalid
		}
		probes[probe.Component] = true
		for _, p := range probe.Processes {
			belongs := (probe.Component == p.Component && probe.Component != "lower-stack") || (probe.Component == "lower-stack" && (p.Component == "lower" || p.Component == "agent"))
			if !IsComponent(p.Component) || seen[p.Component] || !belongs {
				return nil, ErrInvalid
			}
			seen[p.Component] = true
			v := ComponentObservation{Component: p.Component, State: "unknown", DiskSHA256: p.EntrySHA256, DiskBuild: trustworthyBuild(p.EntryBuild, p.Component, p.EntrySHA256), Error: p.RunningError}
			if p.EntryError != "" {
				if v.Error != "" {
					v.Error += "；"
				}
				v.Error += "磁盘程序检查失败：" + p.EntryError
			}
			if len(v.Error) > 4096 {
				v.Error = v.Error[:4096]
			}
			if probe.InspectionError != "" {
				v.Error = probe.InspectionError
			} else if p.Installed != nil && !*p.Installed && probe.Service.LoadState == "not-found" && p.Running != nil && !*p.Running && p.EntrySHA256 == "" && p.RunningSHA256 == "" && p.EntryError == "" && p.RunningError == "" {
				v.State = "not_installed"
			} else if p.Running != nil {
				if !*p.Running {
					v.State = "stopped"
				} else {
					v.State = "running_version_unknown"
					v.RunningSHA256 = p.RunningSHA256
					v.RunningBuild = trustworthyBuild(p.RunningBuild, p.Component, p.RunningSHA256)
					if v.RunningBuild != nil {
						v.State = "running"
					}
					v.DiskDiffers = digestPattern.MatchString(p.EntrySHA256) && digestPattern.MatchString(p.RunningSHA256) && p.EntrySHA256 != p.RunningSHA256
				}
			}
			if report.ReleaseInfo != nil && report.ReleaseInfo.SchemaVersion == 1 {
				if c, ok := report.ReleaseInfo.Compatibility[p.Component]; ok && len(c.StateFormats) <= 3 && len(c.Error) <= 4096 {
					v.Compatibility = &c
				}
			}
			out = append(out, v)
		}
	}
	if !probes["manager"] || !probes["lower-stack"] || !seen["lower"] || !seen["manager"] || !seen["agent"] || len(out) != len(probes)+1 {
		return nil, fmt.Errorf("%w: incomplete component evidence", ErrInvalid)
	}
	return out, nil
}

// The task is allowed to observe only software registered in this project.
// This is checked separately from parsing so legacy reports remain readable.
func (report InspectionReport) ValidateApplications(applications []Application) error {
	allowed := map[string]bool{}
	for _, a := range applications {
		allowed[a.Component] = true
	}
	for _, p := range report.Probes {
		if p.Component != "lower-stack" && IsApplicationComponent(p.Component) && !allowed[p.Component] {
			return ErrInvalid
		}
	}
	return nil
}
func InspectionPath(root string, project, task int) string {
	return filepath.Join(root, "inspections", strconv.Itoa(project), strconv.Itoa(task)+".json")
}

// The report is an Ansible controller artifact written by the fixed playbook,
// not scraped from stdout and not accepted from the user's HTTP request.
func ReadInspection(root string, project int, device Device, task int) ([]ComponentObservation, error) {
	report, err := ReadInspectionReport(root, project, device, task)
	if err != nil {
		return nil, err
	}
	return report.Normalize(device, task)
}
func ReadInspectionReport(root string, project int, device Device, task int) (*InspectionReport, error) {
	path := InspectionPath(root, project, task)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 256<<10 {
		return nil, ErrInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, ErrInvalid
	}
	blob, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(blob) > 256<<10 {
		return nil, ErrTooLarge
	}
	var report InspectionReport
	if err = json.Unmarshal(blob, &report); err != nil {
		return nil, err
	}
	if _, err = report.Normalize(device, task); err != nil {
		return nil, err
	}
	return &report, nil
}
