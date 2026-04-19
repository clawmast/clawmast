package openclaw

import (
	"os"
	"testing"
)

// TestMain disables the HTTP liveness short-circuit for the entire test
// suite. Every test in this package that exercises probe() does so with
// a writeFakeBin-backed Runner to drive CLI-authoritative behaviour; a
// real openclaw gateway happening to listen on the default port would
// otherwise flip Alive=true before the fake CLI ever ran and make the
// assertions machine-dependent. Tests that want to cover the HTTP path
// restore the endpoint locally.
func TestMain(m *testing.M) {
	probeFastDisabled = true
	os.Exit(m.Run())
}
