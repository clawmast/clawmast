// Command clawmastd is the ClawMast supervisor.
//
// It spawns and supervises the clawmast worker, health-checks it,
// activates new worker versions atomically via symlink swap, and rolls
// back on health failures or repeated crashes. It uses the Go standard
// library only, so that it stays frozen while worker functionality
// evolves behind it.
package main

import (
	"fmt"
	"os"

	"github.com/clawmast/clawmast/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.Full())
		return
	}
	fmt.Printf("clawmastd supervisor %s\n", version.Full())
	fmt.Println("(Iteration 0 walking skeleton — spawn/health lands in T0-05)")
}
