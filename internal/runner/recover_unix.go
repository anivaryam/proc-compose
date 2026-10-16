//go:build !windows

package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/anivaryam/proc-compose/internal/daemon"
)

// errSignalUnavailable reports a platform where recovery has no race-free way
// to signal a process it has proven ours. Only the non-Linux pidfd stubs
// return it, so it is unused in a Linux build.
var errSignalUnavailable = errors.New("race-free process signalling is unavailable on this platform") //nolint:unused // returned by the macOS and other-Unix pidfd stubs

// pidfdOpen and pidfdSend are the only way crash recovery signals anything.
// They are indirected, like groupKill, so tests can model a kernel without
// pidfds and can record signals instead of sending them.
var (
	pidfdOpen = openPidfd
	pidfdSend = sendPidfd
	// procOwn is indirected so tests can model a /proc that belongs to
	// another pid namespace.
	procOwn = procfsIsOwn
	// currentHost is indirected so tests can model an unreadable boot id.
	currentHost = daemon.HostIdentity
	// procTasks counts /proc/<pid>/task; indirected so tests can model a
	// zombie thread-group leader whose other threads still run.
	procTasks = countProcTasks
)

// pinnedProcess is a process proven to belong to a recorded group, held by a
// pidfd so that a later signal reaches that exact process or nothing.
type pinnedProcess struct {
	fd  int
	pid int
}

// recovery is the working state of one recorded group.
type recovery struct {
	RecoveredGroup
	leader  int             // pidfd of the proven leader, -1 when none
	pins    []pinnedProcess // proven members, pinned before any signal
	proven  bool            // identity proven: the group may be signalled
	emptied bool            // proven empty after a signal; never re-probed
}

// RecoverGroups resolves the process groups a crashed daemon recorded: it
// proves each one empty, proves it ours and (unless mode is RecoverAssessOnly)
// ends it, or reports it unknown. host is the identity stored with the record.
//
// A group is only ever signalled after proof that it is the recorded one: its
// leader is alive (possibly a zombie) with the recorded start time and still
// leads the group, checked through a pidfd so the check and the signal name the
// same process. Every member is pinned by its own pidfd before the first
// signal. Without pidfds, without a start time (macOS), or with the leader
// reaped, the group is reported unknown and nothing is signalled.
//
// Graceful recovery spends until deadline on SIGTERM, then sends SIGKILL and
// waits up to killSettle more, so the total is bounded by deadline+killSettle.
func RecoverGroups(groups []daemon.Group, host daemon.Host, mode RecoveryMode, deadline time.Time) []RecoveredGroup {
	rs := make([]*recovery, len(groups))
	for i, g := range groups {
		rs[i] = &recovery{RecoveredGroup: RecoveredGroup{GroupResult: GroupResult{Name: g.Name, PGID: g.PGID}, LeaderStart: g.LeaderStart}, leader: -1}
	}
	defer func() {
		for _, r := range rs {
			r.release()
		}
	}()

	if outcome, reason := hostVerdict(host); reason != "" {
		for _, r := range rs {
			r.finish(outcome, reason)
		}
		return recoveryResults(rs)
	}

	var proven []*recovery
	for _, r := range rs {
		if r.classify(mode) {
			proven = append(proven, r)
		}
	}
	if mode == RecoverAssessOnly || len(proven) == 0 {
		return recoveryResults(rs)
	}

	if mode == RecoverGraceful {
		for _, r := range proven {
			r.signal(syscall.SIGTERM, "SIGTERM")
		}
		awaitEmpty(proven, deadline)
	}
	var leftover []*recovery
	for _, r := range proven {
		if !r.emptied {
			r.repin()
			r.signal(syscall.SIGKILL, "SIGKILL")
			leftover = append(leftover, r)
		}
	}
	// The SIGKILL settle is outside the deadline: a SIGKILL cannot be
	// declined, so waiting for it to land is bounded bookkeeping.
	awaitEmpty(leftover, time.Now().Add(killSettle))
	for _, r := range proven {
		r.conclude()
	}
	return recoveryResults(rs)
}

// hostVerdict applies the record-wide identity rules to host, the identity
// stored with a record of process groups. It returns no reason when the
// record's pids and start times name processes here; otherwise the outcome
// every group of the record gets, and why.
func hostVerdict(host daemon.Host) (outcome, reason string) {
	current := currentHost()
	switch {
	case runtime.GOOS == "linux" && (host.BootID == "" || current.BootID == ""):
		// Without both boot ids a record from a previous boot cannot be told
		// apart: the namespace check below does not catch a reboot, because
		// the initial namespaces keep their inode numbers across boots.
		return OutcomeUnknown, "boot_unknown"
	case host.BootID != "" && current.BootID != "" && host.BootID != current.BootID:
		// Nothing from another boot survives it; nothing is enumerated.
		return OutcomeEmpty, "rebooted"
	case host.PIDNS != current.PIDNS || host.TimeNS != current.TimeNS || !procOwn():
		// Pids from another pid namespace, and start times from another time
		// namespace, name nothing here. Neither does a /proc mounted for
		// another pid namespace: every proof reads it, while a pidfd opens
		// the pid in ours, so a proof would be about one process and the
		// signal would reach another.
		return OutcomeUnknown, "namespace_mismatch"
	}
	return "", ""
}

// recordedGroupOf returns the group among groups, recorded with host, that the
// running process pid is a member of. The group is proven as RecoverAssessOnly
// proves one, without a pidfd or a signal: the record passes hostVerdict, and
// the group's leader is alive with the recorded start time and still leads
// it, so pid's group is the recorded one and not a later group that reused
// the number. A group whose leader is gone, or that has no recorded start
// time (macOS), is never claimed, whatever its members.
func recordedGroupOf(pid int, groups []daemon.Group, host daemon.Host) (daemon.Group, bool) {
	if pid <= 0 || len(groups) == 0 {
		return daemon.Group{}, false
	}
	if _, reason := hostVerdict(host); reason != "" {
		return daemon.Group{}, false
	}
	info, err := daemon.ProcStat(pid)
	if err != nil || info.PGID <= 1 {
		return daemon.Group{}, false
	}
	for _, g := range groups {
		if g.PGID != info.PGID || g.LeaderStart == 0 {
			continue
		}
		r := recovery{RecoveredGroup: RecoveredGroup{GroupResult: GroupResult{Name: g.Name, PGID: g.PGID}, LeaderStart: g.LeaderStart}, leader: -1}
		if r.leaderIdentity() == "" {
			return g, true
		}
	}
	return daemon.Group{}, false
}

// assessCrashedRun decides whether a new run may take over pidPath; see
// CheckCrashedRun. stale reports a PID file left by a daemon that is gone and
// that nothing it recorded can still be running. It never signals or removes
// anything.
//
// It refuses with the already-running error for a live daemon, with a
// *CrashedRunError when a recorded group is live or cannot be proven empty, and
// with a NOT verified error for an unreadable PID file. A PID file without a
// record (an older daemon, or recording disabled) is stale, as before records
// existed.
func assessCrashedRun(pidPath string) (stale bool, err error) {
	if _, err := os.Stat(pidPath); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if alive, pid := daemon.IsAliveFromPIDFile(pidPath); alive {
		return false, alreadyRunningError(pid)
	}
	rec, err := daemon.ReadRecord(pidPath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("the PID file of a previous daemon is unreadable, so the process groups it may record " +
			"are NOT verified as terminated.\n" +
			"  inspect for leftovers: ps -eo pid,ppid,pgid,args\n" +
			"  then clear it: proc-compose stop --force")
	}
	if rec.Groups != nil {
		groups := RecoverGroups(*rec.Groups, rec.Host, RecoverAssessOnly, time.Now())
		for _, g := range groups {
			if g.Outcome != OutcomeEmpty {
				return false, &CrashedRunError{PID: rec.PID, Groups: groups}
			}
		}
	}
	return true, nil
}

// classify applies the identity rules to one group and reports whether it was
// proven ours with members pinned. Every other group is finished here.
func (r *recovery) classify(mode RecoveryMode) bool {
	pgid, start := r.PGID, r.LeaderStart
	switch {
	case pgid == 0:
		r.finish(OutcomeUnknown, "launch_pending")
		return false
	case pgid <= 1:
		// kill(2) reads 1 and below as "every process", never as one group.
		r.finish(OutcomeUnknown, "invalid_pgid")
		return false
	}

	empty, members, known := groupProvenEmpty(pgid)
	switch {
	case empty:
		r.finish(OutcomeEmpty, "no_members")
		return false
	case !known:
		r.finish(OutcomeUnknown, "enumeration_failed")
		return false
	}
	r.PIDs, r.LiveMembers = members, len(members)

	if start == 0 {
		r.finish(OutcomeUnknown, "leader_start_unknown")
		return false
	}
	if reason := r.leaderIdentity(); reason != "" {
		r.finishUnproven(reason)
		return false
	}
	// Rules E and R have had their say, so a reused or emptied number was
	// already resolved; only proving the group ours is refused for the
	// caller's own process group.
	if pgid == syscall.Getpgrp() {
		r.finish(OutcomeUnknown, "own_group")
		return false
	}
	if mode == RecoverAssessOnly {
		r.finish(OutcomeLive, "leader_proven")
		return false
	}

	lfd, err := pidfdOpen(pgid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			r.finishUnproven("leader_gone")
		} else {
			// ENOSYS, EPERM (seccomp) and anything else: there is no
			// race-free signal, and a bare pid is never signalled instead.
			r.finish(OutcomeUnknown, "signal_unavailable")
		}
		return false
	}
	r.leader = lfd
	// Re-read identity now that the pidfd pins the process, then confirm the
	// pidfd's process is unreaped, so the read describes that process.
	if reason := r.leaderIdentity(); reason != "" {
		r.finishUnproven(reason)
		return false
	}
	if pidfdSend(lfd, 0) != nil {
		r.finishUnproven("leader_gone")
		return false
	}
	pins, ok := pinMembers(pgid, nil)
	// The leader stayed unreaped across the scan, so pgid named our group the
	// whole time and every member found is ours.
	if !ok || pidfdSend(lfd, 0) != nil {
		closePins(pins)
		if !ok {
			r.finish(OutcomeUnknown, "enumeration_failed")
		} else {
			r.finishUnproven("leader_gone")
		}
		return false
	}
	// From here PIDs collects the members actually signalled.
	r.pins, r.proven, r.Reason, r.PIDs, r.LiveMembers = pins, true, "leader_proven", nil, 0
	return true
}

// leaderIdentity checks the recorded leader against procfs. It returns "" when
// the leader is alive with the recorded start time and still leads the group;
// otherwise the reason it cannot be proven, with pgid_reused meaning the pid now
// belongs to a different process.
func (r *recovery) leaderIdentity() string {
	info, err := daemon.ProcStat(r.PGID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "leader_gone"
	case errors.Is(err, errors.ErrUnsupported):
		return "leader_start_unknown"
	case err != nil:
		return "enumeration_failed"
	case info.StartTime != r.LeaderStart:
		// The kernel reallocates a pid only when nothing uses it as a pid,
		// pgid or sid, so the recorded group had already ended.
		return "pgid_reused"
	case info.PGID != r.PGID:
		return "leader_left_group"
	}
	return ""
}

// finishUnproven ends classification for a group whose leader could not be
// proven. A reused pid proves the recorded group empty; otherwise the group is
// re-probed, because the leader may have been reaped only now.
func (r *recovery) finishUnproven(reason string) {
	r.release()
	if reason == "pgid_reused" {
		r.PIDs, r.LiveMembers = nil, 0
		r.finish(OutcomeEmpty, reason)
		return
	}
	empty, members, known := groupProvenEmpty(r.PGID)
	switch {
	case empty:
		r.PIDs, r.LiveMembers = nil, 0
		r.finish(OutcomeEmpty, "no_members")
	case !known:
		r.finish(OutcomeUnknown, "enumeration_failed")
	default:
		r.PIDs, r.LiveMembers = members, len(members)
		r.finish(OutcomeUnknown, reason)
	}
}

// pinMembers opens a pidfd for every live member of pgid that is not this
// process or already in have, keeping those whose procfs entry, read after the
// pidfd was opened, still shows them live in pgid. It reports false when
// membership could not be enumerated.
func pinMembers(pgid int, have []pinnedProcess) ([]pinnedProcess, bool) {
	pids, ok := recoveryMemberPIDs(pgid)
	if !ok {
		return nil, false
	}
	self := os.Getpid()
	var pins []pinnedProcess
	for _, pid := range pids {
		if pid == self || slices.ContainsFunc(have, func(p pinnedProcess) bool { return p.pid == pid }) {
			continue
		}
		fd, err := pidfdOpen(pid)
		if err != nil {
			continue
		}
		info, err := daemon.ProcStat(pid)
		if err != nil || info.PGID != pgid || !recoveryLive(pid, info.State) || pidfdSend(fd, 0) != nil {
			_ = syscall.Close(fd)
			continue
		}
		pins = append(pins, pinnedProcess{fd: fd, pid: pid})
	}
	return pins, true
}

// repin prepares the KILL phase. Pins whose process is gone are dropped. If
// the leader is still proven, members forked since the first scan are pinned
// too; if it is gone (a wrapper that exited on SIGTERM), only the pins taken
// before SIGTERM remain, and each still names exactly a process proven ours.
func (r *recovery) repin() {
	alive := r.pins[:0]
	for _, p := range r.pins {
		if pidfdSend(p.fd, 0) == nil {
			alive = append(alive, p)
		} else {
			_ = syscall.Close(p.fd)
		}
	}
	r.pins = alive
	if r.leader < 0 || pidfdSend(r.leader, 0) != nil || r.leaderIdentity() != "" {
		return
	}
	fresh, ok := pinMembers(r.PGID, r.pins)
	if !ok || pidfdSend(r.leader, 0) != nil {
		closePins(fresh)
		return
	}
	r.pins = append(r.pins, fresh...)
}

// signal delivers sig to every pinned member and records it once if any
// delivery succeeded.
func (r *recovery) signal(sig syscall.Signal, name string) {
	delivered := false
	for _, p := range r.pins {
		if pidfdSend(p.fd, sig) == nil {
			delivered = true
			if !slices.Contains(r.PIDs, p.pid) {
				r.PIDs = append(r.PIDs, p.pid)
			}
		}
	}
	if delivered {
		r.Signals = append(r.Signals, name)
	}
}

// conclude decides a proven group's outcome after signalling.
func (r *recovery) conclude() {
	var members []int
	known := true
	if !r.emptied {
		r.emptied, members, known = groupProvenEmpty(r.PGID)
	}
	if r.emptied {
		r.LiveMembers = 0
		if len(r.Signals) > 0 {
			r.finish(OutcomeTerminated, r.Reason)
		} else {
			r.finish(OutcomeEmpty, "no_members")
		}
		return
	}
	r.PIDs, r.LiveMembers = members, len(members)
	switch {
	case !known:
		r.finish(OutcomeUnknown, "enumeration_failed")
	case allPinned(members, r.pins):
		// Every survivor was proven ours and sent SIGKILL: a process in
		// uninterruptible sleep, for example.
		r.finish(OutcomeUnknown, "survived_sigkill")
	default:
		r.finish(OutcomeUnknown, "unproven_members")
	}
}

// awaitEmpty polls the groups until each is proven empty or deadline passes.
// A group proven empty is never probed again: once empty it cannot refill, and
// a later probe could see an unrelated group that reused the number.
func awaitEmpty(rs []*recovery, deadline time.Time) {
	for {
		pending := false
		for _, r := range rs {
			if r.emptied {
				continue
			}
			if r.emptied, _, _ = groupProvenEmpty(r.PGID); !r.emptied {
				pending = true
			}
		}
		if !pending || !time.Now().Before(deadline) {
			return
		}
		time.Sleep(memberPollInterval)
	}
}

// groupProvenEmpty applies the emptiness rule, which needs no identity: kill
// returning ESRCH proves atomically that nothing holds pgid, and otherwise two
// membership scans memberPollInterval apart must both find no live member. A
// live member of the recorded group would keep the pgid allocated, so finding
// no live holder proves the group ended. members are the live members found;
// known is false when membership could not be enumerated.
//
// Where an EPERM probe proves a member exists (probeEPERMProvesMember), that
// member may be one the scan cannot see, such as another user's process under
// a procfs mounted hidepid=2, so scans that find nothing leave membership
// unknown rather than proving the group empty.
//
// A probe that succeeds proves a holder this process may signal. With no live
// member in the scans, only a visible dead holder (a zombie awaiting its
// reaper) explains it. Without one the holder is hidden from the scan, such as
// a setuid process that keeps this user's real uid under hidepid, so
// membership stays unknown.
func groupProvenEmpty(pgid int) (empty bool, members []int, known bool) {
	if pgid <= 1 {
		return false, nil, false
	}
	probe := groupKill(-pgid, 0)
	if errors.Is(probe, syscall.ESRCH) {
		return true, nil, true
	}
	hiddenMember := probeEPERMProvesMember && errors.Is(probe, syscall.EPERM)
	for scan := 0; scan < 2; scan++ {
		if scan > 0 {
			time.Sleep(memberPollInterval)
		}
		pids, ok := recoveryMemberPIDs(pgid)
		if !ok {
			return false, nil, false
		}
		if len(pids) > 0 {
			return false, pids, true
		}
		if hiddenMember {
			return false, nil, false
		}
	}
	if probe == nil {
		if n, ok := recoveryGroupHolders(pgid); !ok || n == 0 {
			return false, nil, false
		}
	}
	return true, nil, true
}

// recoveryLive reports whether a group member in procfs state is live for
// crash recovery. A Z or X state is dead only for a process with a single
// task: when a thread-group leader's main thread exits while other threads
// run, /proc/<tgid>/stat reports Z for the whole process. A task list that
// cannot be read proves nothing, so that member counts as live.
func recoveryLive(pid int, state byte) bool {
	if !isZombieState(state) {
		return true
	}
	n, err := procTasks(pid)
	return err != nil || n > 1
}

// countProcTasks counts the threads procfs lists for pid.
func countProcTasks(pid int) (int, error) {
	entries, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	return len(entries), err
}

func (r *recovery) finish(outcome, reason string) {
	r.Outcome, r.Reason = outcome, reason
	switch {
	case outcome == OutcomeEmpty || outcome == OutcomeTerminated:
		r.State = GroupEmpty
	case r.LiveMembers > 0:
		r.State = GroupOwned
	default:
		r.State = GroupUnavailable
	}
}

// release closes every pidfd the group holds.
func (r *recovery) release() {
	if r.leader >= 0 {
		_ = syscall.Close(r.leader)
		r.leader = -1
	}
	closePins(r.pins)
	r.pins = nil
}

func closePins(pins []pinnedProcess) {
	for _, p := range pins {
		_ = syscall.Close(p.fd)
	}
}

func allPinned(pids []int, pins []pinnedProcess) bool {
	for _, pid := range pids {
		if !slices.ContainsFunc(pins, func(p pinnedProcess) bool { return p.pid == pid }) {
			return false
		}
	}
	return true
}

func recoveryResults(rs []*recovery) []RecoveredGroup {
	out := make([]RecoveredGroup, len(rs))
	for i, r := range rs {
		out[i] = r.RecoveredGroup
		sort.Ints(out[i].PIDs)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].PGID < out[j].PGID
	})
	return out
}
