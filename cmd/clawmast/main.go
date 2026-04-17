// Command clawmast is the ClawMast worker process.
//
// It will host the local HTTP API, the embedded React+Vite UI, the PTY
// bridge, the gateway orchestrator, and (optionally) the cloud tunnel
// client. It is supervised by clawmastd.
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
	fmt.Printf("clawmast worker %s\n", version.Full())
	fmt.Println("(Iteration 0 walking skeleton — HTTP server lands in T0-03)")
}
