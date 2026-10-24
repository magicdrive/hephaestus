//go:build linux

package hephaestus

import "syscall"

// prSetDumpable is PR_SET_DUMPABLE from <linux/prctl.h>.
const prSetDumpable = 4

// disableDumpable marks the process non-dumpable (prctl PR_SET_DUMPABLE 0).
// Linux then lets only root (CAP_SYS_PTRACE) read its /proc/PID/environ,
// mem and similar files, or attach to it, so build code running as the same
// user cannot read the release and tap tokens from Hephaestus's
// environment. Commands Hephaestus executes are dumpable again after exec,
// and the setting does not protect the process's ancestors (the shell of an
// Actions step, the runner), whose environment may hold the same tokens:
// keep building and publishing in separate jobs. A failure is ignored; the
// protection is best-effort.
func disableDumpable() {
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0) // best-effort, see above
}
