// Command hephaestus forges release artifacts into reliable distributions.
package main

import (
	"runtime/debug"

	"github.com/magicdrive/hephaestus/cmd/hephaestus"
)

// version is set at link time: -ldflags "-X main.version=v1.2.3".
var version string

func main() {
	hephaestus.Execute(Version())
}

// Version returns the link-time version, else the module version recorded
// by go install, else "dev".
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
