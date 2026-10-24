// Command compatcli is a minimal CLI used only to drive GoReleaser when
// recording the compatibility fixtures under testdata/compat/goreleaser.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: compatcli --version")
	os.Exit(2)
}
