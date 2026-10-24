// Command heph-fixture is a tiny CLI used by Hephaestus end-to-end tests.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("heph-fixture", version)
		return
	}
	fmt.Println("forged by hephaestus")
}
