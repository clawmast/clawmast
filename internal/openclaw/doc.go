// Package openclaw is clawmast's integration layer for the upstream
// openclaw gateway. It spawns the `openclaw` CLI out-of-process (never
// the in-tree Node runtime) to read health and to run the Fix cascade
// documented in architecture/refactor.md §10 decision #20 and the
// contract in clawmast-devdocs/integrations/openclaw.md.
//
// Design notes:
//
//   - CLI-spawn only. We do not open a WebSocket to the gateway in I3;
//     the upstream protocol mandates device-keypair signing that is
//     out of scope for this iteration (see integrations/openclaw.md §7).
//   - Every spawn is wrapped in a context with timeout. Upstream issue
//     #11843 documents that status/health CLIs sometimes fail to exit
//     after writing output, so Cmd.Run() alone would hang the poller.
//     runCmd SIGKILLs the process when the context fires.
//   - Status is served from an in-memory Snapshot updated by a
//     background Poller. Callers never wait on a CLI spawn.
package openclaw
