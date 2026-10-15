package runner

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

// probeConn is the connection a passing http or tcp probe attempt used: the
// probe's own address and the server address that accepted it. The server
// side of that connection is what attribution identifies.
type probeConn struct {
	local, remote netip.AddrPort
}

// probeConnOf returns the addresses of conn, or false when it is not TCP.
func probeConnOf(conn net.Conn) (probeConn, bool) {
	local, ok1 := conn.LocalAddr().(*net.TCPAddr)
	remote, ok2 := conn.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return probeConn{}, false
	}
	return probeConn{local: unmapAddrPort(local.AddrPort()), remote: unmapAddrPort(remote.AddrPort())}, true
}

func unmapAddrPort(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// attributeProbe reports which process holds the server side of c, classified
// against pg, the process group or Job Object the probed incarnation runs in.
//
// The rule is asymmetric, like the shutdown verdicts: owned and foreign are
// claimed only when a holder is positively identified, and everything else is
// unattributable with a reason. A probe that passed without a recorded
// connection is unattributable too.
func attributeProbe(c probeConn, pg ProcessGroup) *ipc.EndpointOwner {
	if !c.remote.IsValid() {
		return &ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "the probe connection was not recorded"}
	}
	owner := attributeSocket(c, pg)
	owner.Address = c.remote.String()
	return &owner
}

// socketEntry is one row of the kernel's TCP socket table.
type socketEntry struct {
	local, remote netip.AddrPort
	state         byte
	uid           int
	inode         uint64
}

// TCP states, as /proc/net/tcp numbers them. The Windows backend maps its
// table's states onto the same numbers.
const (
	tcpEstablished = 0x01
	tcpSynRecv     = 0x03
	tcpListen      = 0x0A
)

// answeringSockets selects the sockets that answered c, the connection a probe
// attempt dialed, from table. It must be called while the probe still holds
// the connection open and before any request was sent on it, so the server
// cannot yet have answered, closed or handed it over.
//
// The answer is the server's own row for this connection: local address the
// probe's remote one and the reverse. With an identity it is exact, even when
// several listeners share the port. Without one it is either still queued for
// accept (established or SYN-received), and then the listener whose queue
// holds it is the answer (closing that listener would have reset the
// connection), or it is past its life (closed or closing), which proves
// nothing about who holds the port now. No row at all means the connection
// does not end in this network namespace: a remote host, a container
// address, or a NAT rule. Then a local listener on the same port is not the
// answer, so nothing is.
func answeringSockets(table []socketEntry, c probeConn) (ids []uint64, uid int, reason string) {
	for _, e := range table {
		if e.local != c.remote || e.remote != c.local || e.state == tcpListen {
			continue
		}
		switch {
		case e.inode != 0:
			return []uint64{e.inode}, e.uid, ""
		case e.state == tcpEstablished || e.state == tcpSynRecv:
			if ids, uid, ok := listenerSockets(table, c); ok {
				return ids, uid, ""
			}
			return nil, 0, "no listener holds the queue of the connection to " + c.remote.String()
		default:
			return nil, 0, "the server side of the connection to " + c.remote.String() + " was already closed"
		}
	}
	return nil, 0, "the connection to " + c.remote.String() + " does not end in this network namespace"
}

// listenerSockets finds the listeners whose queue can hold c: the most
// specific ones bound to the probed port, as the kernel chooses. Several at
// that level (SO_REUSEPORT) are all returned. uid is the first one's owner.
// answeringSockets uses it only for a connection proven to be queued here.
func listenerSockets(table []socketEntry, c probeConn) (inodes []uint64, uid int, found bool) {
	// 0: exact address; 1: the same family's wildcard; 2: an IPv6 wildcard
	// reached through an IPv4 address (a dual-stack listener).
	best := 3
	for _, e := range table {
		if e.state != tcpListen || e.inode == 0 || e.local.Port() != c.remote.Port() {
			continue
		}
		rank := 3
		switch addr := e.local.Addr(); {
		case addr == c.remote.Addr():
			rank = 0
		case addr.IsUnspecified() && addr.Is4() == c.remote.Addr().Is4():
			rank = 1
		case addr.IsUnspecified() && addr.Is6() && c.remote.Addr().Is4():
			rank = 2
		}
		switch {
		case rank < best:
			best, inodes, uid = rank, []uint64{e.inode}, e.uid
		case rank == best && rank < 3:
			inodes = append(inodes, e.inode)
		}
	}
	return inodes, uid, len(inodes) > 0
}

// socketHolder is a process that holds an answering socket.
type socketHolder struct {
	pid      int
	command  string
	member   bool // in the probed incarnation's process group
	sameUser bool // runs as this process's user; only such outsiders are named
}

// classifyHolders turns the holders found for each answering socket into a
// result. A socket is ours when any member holds it, even if a process
// outside the group shares the descriptor. An outsider is named, and makes
// the socket foreign, only when it runs as this process's user; a socket held
// only by another user's process is unattributable and that process is not
// named. When several sockets could have answered (SO_REUSEPORT), every one
// must be ours for owned, and every one a same-user outsider's for foreign;
// anything else is unattributable, as is a socket no inspectable process
// holds, for which unseen says why.
func classifyHolders(inodes []uint64, holders map[uint64][]socketHolder, unseen string) ipc.EndpointOwner {
	if len(inodes) == 0 {
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "no socket answered"}
	}
	var member, other *socketHolder
	ours, theirs, alien := 0, 0, 0
	for _, inode := range inodes {
		var m, o *socketHolder
		otherUser := false
		for i := range holders[inode] {
			h := &holders[inode][i]
			switch {
			case h.member:
				if m == nil {
					m = h
				}
			case h.sameUser:
				if o == nil {
					o = h
				}
			default:
				otherUser = true
			}
		}
		switch {
		case m != nil:
			ours++
			if member == nil {
				member = m
			}
		case o != nil:
			theirs++
			if other == nil {
				other = o
			}
		case otherUser:
			alien++
		}
	}
	switch {
	case ours == len(inodes):
		return ipc.EndpointOwner{Result: ipc.OwnerOwned, PID: member.pid, Command: member.command}
	case theirs == len(inodes):
		return ipc.EndpointOwner{Result: ipc.OwnerForeign, PID: other.pid, Command: other.command}
	case ours+theirs > 0:
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: fmt.Sprintf("%d listeners share the port and not all of their holders are known to be on one side", len(inodes))}
	case alien > 0:
		return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "the socket is held by another user's process, which is not named"}
	}
	return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: unseen}
}

// describeOwner is the operator-facing summary of a foreign owner.
func describeOwner(o *ipc.EndpointOwner) string {
	who := fmt.Sprintf("PID %d", o.PID)
	if o.Command != "" {
		who += " (" + o.Command + ")"
	}
	return who
}

// attribute identifies who answered conn for p's current incarnation, whose
// process group exec registered under p's name before any probe started. A
// result computed against a group a restart has since replaced is dropped
// by the generation guards that record it.
func (r *Runner) attribute(p procInfo, conn probeConn) *ipc.EndpointOwner {
	r.groupsMu.Lock()
	pg := r.groups[p.name]
	r.groupsMu.Unlock()
	if pg == nil {
		return &ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "the process has no group"}
	}
	return attributeProbe(conn, pg)
}

// foreignListenerMessage is the warning for a probe a foreign process
// answered. rejected says that foreign_listener: reject refused the answer.
func foreignListenerMessage(name, probe string, o *ipc.EndpointOwner, rejected bool) string {
	msg := fmt.Sprintf("%s was answered at %s by %s, which is not part of %s's process tree; another server holds this address",
		probe, o.Address, describeOwner(o), name)
	if rejected {
		return msg + "; foreign_listener: reject does not count that answer"
	}
	return msg + ", so the probe does not show that " + name + " is serving"
}

// foreignListenerError is the failed-attempt detail for an answer
// foreign_listener: reject refused.
func foreignListenerError(name string, o *ipc.EndpointOwner) error {
	return fmt.Errorf("answered at %s by %s, outside %s's process tree", o.Address, describeOwner(o), name)
}

// ownerCheck attributes every attempt of one readiness probe run, for
// incarnation gen of st, when its connection is dialed, and judges the
// answered ones. Under foreign_listener: reject it refuses an answer
// attributed to a foreign process: the refusal is recorded as the
// incarnation's readiness owner, and the first one is logged and broadcast.
// An unattributable answer is always taken. owner is who answered the
// attempt that was taken. A probe run calls it from one goroutine only.
type ownerCheck struct {
	r      *Runner
	p      procInfo
	st     *procState
	gen    uint64
	probe  string
	reject bool
	warned bool
	owner  *ipc.EndpointOwner
}

func (r *Runner) newOwnerCheck(p procInfo, st *procState, gen uint64, kind string) *ownerCheck {
	return &ownerCheck{r: r, p: p, st: st, gen: gen, probe: "readiness probe (" + kind + ")",
		reject: p.proc.EffectiveForeignListener() == config.ForeignListenerReject}
}

func (c *ownerCheck) attribute(conn probeConn) *ipc.EndpointOwner { return c.r.attribute(c.p, conn) }

func (c *ownerCheck) accept(owner *ipc.EndpointOwner) error {
	if owner == nil {
		owner = &ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "the probe connection was not recorded"}
	}
	if !c.reject || owner.Result != ipc.OwnerForeign {
		c.owner = owner
		return nil
	}
	if c.st.recordRefusedOwner(c.gen, owner) && !c.warned {
		c.warned = true
		c.r.broadcastState(c.p, c.st)
		c.r.processLog(c.p, "warn", "readiness.foreign_listener", foreignListenerMessage(c.p.name, c.probe, owner, true))
	}
	return foreignListenerError(c.p.name, owner)
}
