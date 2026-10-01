//go:build linux

package runner

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// groupMembers counts the live members of a process group by walking procfs.
//
// It reports false for the second return value when the count could not be
// established, so that callers skip escalation rather than assume a group is
// empty on incomplete information.
//
// A member is live unless its /proc state is a termination state. cmd.Wait
// reaps the command leader only, so a killed descendant whose new parent has not
// collected it stays in procfs as a zombie; treating those as live would turn a
// successful shutdown into a reported failure.
func groupMembers(pgid int) (int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	live := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		gid, state, ok := procStat(pid)
		if !ok || gid != pgid {
			continue
		}
		if isZombieState(state) {
			continue
		}
		live++
	}
	return live, true
}

// procStat returns a process's group ID and its single-letter state.
func procStat(pid int) (pgid int, state byte, ok bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, false
	}
	// The comm field is parenthesised and may itself contain spaces or
	// parentheses, so parse relative to the final ')': the fields after it are
	// state, ppid, pgrp, ... in that fixed order.
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 || i+2 > len(data) {
		return 0, 0, false
	}
	fields := strings.Fields(string(data[i+2:]))
	if len(fields) < 3 {
		return 0, 0, false
	}
	state = fields[0][0]
	gid, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return gid, state, true
}

// isZombieState reports whether a procfs state letter means the process has
// terminated. Z is the ordinary zombie state; X and x are the dead and
// in-death-transit states used during exit.
func isZombieState(state byte) bool {
	return state == 'Z' || state == 'X' || state == 'x'
}
