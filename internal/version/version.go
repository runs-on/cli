package version

import "runtime/debug"

// Version is the version of the roc CLI.
// It can be set at build time using ldflags:
//
//	go build -ldflags "-X roc/internal/version.Version=v2.12.0"
var Version = "dev"

func String() string {
	if Version != "" && Version != "dev" {
		return Version
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return Version
}
