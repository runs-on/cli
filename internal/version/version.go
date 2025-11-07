package version

// Version is the version of the roc CLI.
// It can be set at build time using ldflags:
//   go build -ldflags "-X roc/internal/version.Version=v0.1.6"
var Version = "dev"

