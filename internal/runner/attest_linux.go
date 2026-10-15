//go:build linux

package runner

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/anivaryam/proc-compose/internal/ipc"
)

// attributeSocket attributes c against pg, a process group created with
// Setpgid, whose ID is therefore its leader's PID.
func attributeSocket(c probeConn, pg ProcessGroup) ipc.EndpointOwner {
	upg, ok := pg.(*unixProcessGroup)
	if !ok {
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: reasonGroupNotInspectable}
	}
	upg.mu.Lock()
	pgid := upg.pgid
	upg.mu.Unlock()
	if pgid <= 1 {
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: reasonGroupNotInspectable}
	}
	return attributePGID(c, pgid)
}

// attributePGID identifies the holder of the server side of c from procfs:
// the socket table in /proc/net/tcp{,6} gives the answering socket's inode,
// and /proc/<pid>/fd says which processes hold it. Members of pgid are read
// first, so an owned answer costs one table read and the members' fd lists;
// only a socket no member holds leads to a walk of every visible process.
//
// /proc/net is this process's network namespace, which is the one its probes
// dial from, so a connection with no server row in it (a remote or container
// address, a NAT rule) is unattributable, never a local listener's; see
// answeringSockets for the rules and for when it must be called. Holders are
// only ever observed, never signalled.
func attributePGID(c probeConn, pgid int) ipc.EndpointOwner {
	if !procfsIsOwn() {
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "/proc belongs to another pid namespace"}
	}
	var table []socketEntry
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		entries, err := readSocketTable(path)
		if err != nil && !os.IsNotExist(err) {
			return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "socket table unreadable: " + err.Error()}
		}
		table = append(table, entries...)
	}
	inodes, uid, reason := answeringSockets(table, c)
	if len(inodes) == 0 {
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: reason}
	}
	holders, known := findHolders(inodes, pgid)
	if !known {
		return unknownMembership
	}
	return classifyHolders(inodes, holders, hiddenOwner(uid))
}

// hiddenOwner is the reason when no visible process holds a socket.
func hiddenOwner(uid int) string {
	return "no visible process holds the socket (owner uid " + strconv.Itoa(uid) + ")"
}

// unknownMembership is the answer when the group cannot be enumerated: with
// no members known, every holder would look foreign.
var unknownMembership = ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "process group membership could not be enumerated"}

// findHolders returns the processes holding each of inodes. Members of pgid
// are read first; every other visible process is walked only when some
// socket has no member holding it. known is false when the members could
// not be listed.
func findHolders(inodes []uint64, pgid int) (holders map[uint64][]socketHolder, known bool) {
	want := make(map[uint64]bool, len(inodes))
	for _, inode := range inodes {
		want[inode] = true
	}
	holders = map[uint64][]socketHolder{}
	members, known := groupMemberPIDs(pgid)
	if !known {
		return nil, false
	}
	isMember := make(map[int]bool, len(members))
	for _, pid := range members {
		isMember[pid] = true
		collectHolders(pid, true, want, holders)
	}
	if len(holders) == len(inodes) {
		return holders, true
	}
	dir, err := os.ReadDir("/proc")
	if err != nil {
		return holders, true
	}
	for _, e := range dir {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || isMember[pid] {
			continue
		}
		collectHolders(pid, false, want, holders)
	}
	return holders, true
}

// collectHolders records pid in holders for every wanted socket inode among
// its open descriptors. A process whose fd directory cannot be read (another
// user's, or gone) contributes nothing. A non-member is marked sameUser only
// when its effective uid is this process's: running as root lets every fd
// directory be read, and another user's process must still not be named.
func collectHolders(pid int, member bool, want map[uint64]bool, holders map[uint64][]socketHolder) {
	fdDir := "/proc/" + strconv.Itoa(pid) + "/fd"
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return
	}
	command, sameUser, inspected := "", false, false
	for _, fd := range fds {
		link, err := os.Readlink(fdDir + "/" + fd.Name())
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 64)
		if err != nil || !want[inode] {
			continue
		}
		if !inspected {
			inspected = true
			sameUser = member || processEUID(pid) == os.Geteuid()
			if sameUser {
				command = processName(pid)
			}
		}
		holders[inode] = append(holders[inode], socketHolder{pid: pid, command: command, member: member, sameUser: sameUser})
	}
}

// processEUID is pid's effective uid from /proc/<pid>/status, or -1 when it
// cannot be read, which never equals a real uid.
func processEUID(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(rest); len(f) >= 2 {
				if euid, err := strconv.Atoi(f[1]); err == nil {
					return euid
				}
			}
		}
	}
	return -1
}

// processName is the kernel's name for pid (its comm), or "" when unknown.
// Only the name is read: a foreign process's command line, environment and
// working directory are never inspected.
func processName(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readSocketTable parses one /proc/net/tcp-format file.
func readSocketTable(path string) ([]socketEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []socketEntry
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		if e, ok := parseSocketLine(sc.Text()); ok {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// parseSocketLine parses one row: "sl local rem st tx:rx tr:when retrnsmt
// uid timeout inode ...". Addresses are hex words in host byte order and
// ports are hex numbers.
func parseSocketLine(line string) (socketEntry, bool) {
	f := strings.Fields(line)
	if len(f) < 10 {
		return socketEntry{}, false
	}
	local, ok1 := parseSocketAddr(f[1])
	remote, ok2 := parseSocketAddr(f[2])
	state, err1 := strconv.ParseUint(f[3], 16, 8)
	uid, err2 := strconv.Atoi(f[7])
	inode, err3 := strconv.ParseUint(f[9], 10, 64)
	if !ok1 || !ok2 || err1 != nil || err2 != nil || err3 != nil {
		return socketEntry{}, false
	}
	return socketEntry{local: local, remote: remote, state: byte(state), uid: uid, inode: inode}, true
}

// parseSocketAddr parses "0100007F:1F90" (IPv4) or a 32-digit IPv6 form.
// IPv4-mapped IPv6 addresses are unmapped, so a dual-stack socket compares
// equal to the IPv4 address it serves.
func parseSocketAddr(s string) (netip.AddrPort, bool) {
	host, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	raw, err := hex.DecodeString(host)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	// Each 32-bit word was printed as a host-order integer; restoring that
	// integer's in-memory bytes gives network order.
	b := make([]byte, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(b[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	addr, ok := netip.AddrFromSlice(b)
	if !ok {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), true
}
