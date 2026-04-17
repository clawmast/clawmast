// Command clawmast-release is the internal release tool.
//
// It builds, signs, and uploads ClawMast release artefacts. Invoked by
// the GitHub Actions release workflow and never shipped to end users.
package main

import "fmt"

func main() {
	fmt.Println("clawmast-release: implemented in T0-13")
}
