//go:build windows

package main

import "os"

// inodeOf is a Windows stub. Returning 0 makes followFile treat every
// stat cycle as "same identity" (0 == 0), so the log-rotation re-open
// branch stays dormant on Windows. The worker is Tier 2 on Windows and
// does not run under launchd / systemd, so in-place rename rotation
// does not happen for this install tree — the simple fallback is fine.
func inodeOf(_ os.FileInfo) uint64 { return 0 }
