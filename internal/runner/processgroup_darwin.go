//go:build darwin

package runner

import "golang.org/x/sys/unix"

// szomb is Darwin's process state for a process that has terminated and is
// awaiting collection by its parent.
//
// XNU bsd/sys/proc.h defines the extern_proc state values as:
//
//	SIDL 1  Process being created by fork.
//	SRUN 2  Currently runnable.
//	SSLEEP 3  Sleeping on an address.
//	SSTOP 4  Process debugging or suspension.
//	SZOMB 5  Awaiting collection by parent.
//
// golang.org/x/sys/unix does not export this constant for darwin, so it is
// defined here alongside that citation. processGroupMembers matches it against
// KinfoProc.Proc.P_stat, which mirrors extern_proc.p_stat.
const szomb = 5

// groupMembers counts the live members of a process group from the kernel
// process table.
//
// It reports false for the second return value when the table could not be read,
// so that callers skip escalation rather than assume a group is empty on
// incomplete information.
//
// Membership comes from KinfoProc.Eproc.Pgid, the numeric process-group ID.
// KinfoProc.Proc.P_pgrp is deliberately not used: it is a pointer-sized field
// holding a kernel address, not a process-group ID.
//
// A member is live unless its state is SZOMB. cmd.Wait reaps the command leader
// only, so a killed descendant whose new parent has not collected it remains in
// the table as a zombie; treating those as live would turn a successful shutdown
// into a reported failure.
func groupMembers(pgid int) (int, bool) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return 0, false
	}
	want := int32(pgid)
	live := 0
	for i := range procs {
		if procs[i].Eproc.Pgid != want {
			continue
		}
		if procs[i].Proc.P_stat == szomb {
			continue
		}
		live++
	}
	return live, true
}
