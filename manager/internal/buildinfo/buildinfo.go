package buildinfo

import (
	"runtime"
	"runtime/debug"
)

var (
	Version      = "dev"
	Commit       = "unknown"
	BuiltAt      = "unknown"
	ReleaseID    = "dev"
	SourceSHA256 = "unknown"
	Dirty        = "false"
)

type Info struct {
	SchemaVersion int    `json:"schema_version"`
	Component     string `json:"component"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	BuiltAt       string `json:"built_at"`
	ReleaseID     string `json:"release_id"`
	SourceSHA256  string `json:"source_sha256"`
	Dirty         bool   `json:"dirty"`
	Target        string `json:"target"`
}

func Current() Info {
	return ForComponent("manager")
}

func ForComponent(component string) Info {
	info := Info{SchemaVersion: 1, Component: component, Version: Version, Commit: Commit, BuiltAt: BuiltAt,
		ReleaseID: ReleaseID, SourceSHA256: SourceSHA256, Dirty: Dirty == "true", Target: runtime.GOOS + "/" + runtime.GOARCH}
	if build, ok := debug.ReadBuildInfo(); ok && info.Version == "dev" && build.Main.Version != "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}
	return info
}
