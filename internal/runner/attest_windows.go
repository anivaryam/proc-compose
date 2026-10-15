//go:build windows

package runner

import (
	"context"
	"net/netip"
	"path/filepath"
	"unsafe"

	gnet "github.com/shirou/gopsutil/v3/net"
	"golang.org/x/sys/windows"

	"github.com/anivaryam/proc-compose/internal/ipc"
)

// attributeSocket identifies the holder of the server side of c from the TCP
// table (GetExtendedTcpTable, which names each socket's owning PID) and
// classifies it against pg's Job Object, which holds every process of the
// incarnation's tree. A holder this process cannot open, such as a service
// or the System process behind http.sys, is unattributable, not foreign, and
// so is one that runs as another user: only same-user outsiders are named.
// The rules for which row answered are answeringSockets'.
func attributeSocket(c probeConn, pg ProcessGroup) ipc.EndpointOwner {
	wpg, ok := pg.(*windowsProcessGroup)
	if !ok {
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: reasonGroupNotInspectable}
	}
	conns, err := gnet.ConnectionsWithContext(context.Background(), "tcp")
	if err != nil {
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "socket table unreadable: " + err.Error()}
	}
	table := make([]socketEntry, 0, len(conns))
	owners := make(map[uint64]int, len(conns))
	for i, conn := range conns {
		local, ok1 := tableAddr(conn.Laddr)
		remote, ok2 := tableAddr(conn.Raddr)
		if !ok1 || !ok2 {
			continue
		}
		e := socketEntry{local: local, remote: remote, state: windowsTCPState(conn.Status)}
		// A row without an owner (TIME_WAIT reports PID 0) is left without an
		// identity; answeringSockets then decides from its state.
		if conn.Pid > 0 {
			e.inode = uint64(i + 1)
			owners[e.inode] = int(conn.Pid)
		}
		table = append(table, e)
	}
	ids, _, reason := answeringSockets(table, c)
	if len(ids) == 0 {
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: reason}
	}
	holders := map[uint64][]socketHolder{}
	for _, id := range ids {
		pid := owners[id]
		member, sameUser, name, known := wpg.inspect(pid)
		if !known {
			continue
		}
		holders[id] = []socketHolder{{pid: pid, command: name, member: member, sameUser: sameUser}}
	}
	// A holder that cannot be opened is not known to be this user's, so,
	// like another user's process, it is not identified: not even its PID.
	return classifyHolders(ids, holders, "the holder cannot be inspected")
}

// windowsTCPState maps gopsutil's TCP state names onto the procfs numbers
// answeringSockets uses; every other state counts as past its life.
func windowsTCPState(status string) byte {
	switch status {
	case "LISTEN":
		return tcpListen
	case "ESTABLISHED":
		return tcpEstablished
	case "SYN_RECEIVED":
		return tcpSynRecv
	}
	return 0x07 // TCP_CLOSE
}

func tableAddr(a gnet.Addr) (netip.AddrPort, bool) {
	addr, err := netip.ParseAddr(a.IP)
	if err != nil || a.Port > 65535 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(a.Port)), true
}

// inspect reports whether pid is in the group's Job Object and whether it
// runs as this process's user, with its image name when it may be named (a
// member, or a same-user process). known is false when the process cannot be
// opened or the job is closed: membership is then unknown, never assumed. A
// process whose user cannot be read counts as another user's. The job handle
// is used under pg.mu, like every other handle-dependent operation.
func (pg *windowsProcessGroup) inspect(pid int) (member, sameUser bool, name string, known bool) {
	if pid <= 0 {
		return false, false, "", false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false, false, "", false
	}
	defer windows.CloseHandle(h)
	sameUser = processSameUser(h)
	pg.mu.Lock()
	if pg.job == nil {
		pg.mu.Unlock()
		return false, false, "", false
	}
	member, err = isProcessInJob(h, windows.Handle(pg.job.Handle))
	pg.mu.Unlock()
	if err != nil {
		return false, false, "", false
	}
	if member || sameUser {
		var buf [windows.MAX_PATH]uint16
		size := uint32(len(buf))
		if windows.QueryFullProcessImageName(h, 0, &buf[0], &size) == nil {
			name = filepath.Base(windows.UTF16ToString(buf[:size]))
		}
	}
	return member, sameUser, name, true
}

// processSameUser reports whether the process h runs as the user of this
// process, by comparing their token user SIDs. Any failure reads as false.
func processSameUser(h windows.Handle) bool {
	self, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	var token windows.Token
	if windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token) != nil {
		return false
	}
	defer token.Close()
	other, err := token.GetTokenUser()
	if err != nil {
		return false
	}
	return self.User.Sid.Equals(other.User.Sid)
}

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

// isProcessInJob calls IsProcessInJob. Its result is a Win32 BOOL, four
// bytes, so it is received in an int32. go-winjob's jobapi.IsProcessInJob
// passes a one-byte Go bool there, and the kernel's write then overwrites
// the three bytes after it; that corrupted heap memory under -race on
// Windows, so it must not be used.
func isProcessInJob(process, job windows.Handle) (bool, error) {
	var result int32
	r, _, err := procIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&result)))
	if r == 0 {
		return false, err
	}
	return result != 0, nil
}
