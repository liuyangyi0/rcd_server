package buildinfo

import "runtime/debug"

var (
	Version = "dev"
	Commit  = "unknown"
	BuiltAt = "unknown"
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"built_at"`
}

func Current() Info {
	info := Info{Version: Version, Commit: Commit, BuiltAt: BuiltAt}
	if build, ok := debug.ReadBuildInfo(); ok && info.Version == "dev" && build.Main.Version != "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}
	return info
}
