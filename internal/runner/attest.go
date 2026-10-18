package runner

import (
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/registry"
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

// describeOwner is the operator-facing summary of a foreign owner, naming
// the other session of a cross_session one.
func describeOwner(o *ipc.EndpointOwner) string {
	who := fmt.Sprintf("PID %d", o.PID)
	if o.Command != "" {
		who += " (" + o.Command + ")"
	}
	if o.Scope == ipc.ScopeCrossSession && o.Session != nil {
		who += " " + o.Session.Describe()
	}
	return who
}

// attribute identifies who answered conn for p's current incarnation, whose
// process group exec registered under p's name before any probe started, and
// places a foreign answer against the live-session registry. A result
// computed against a group a restart has since replaced is dropped by the
// generation guards that record it.
func (r *Runner) attribute(p procInfo, conn probeConn) *ipc.EndpointOwner {
	r.groupsMu.Lock()
	pg := r.groups[p.name]
	r.groupsMu.Unlock()
	if pg == nil {
		return &ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "the process has no group"}
	}
	owner := attributeProbe(conn, pg)
	r.placeForeign(owner)
	return owner
}

// placeForeign sets the scope of a foreign owner from the live-session
// registry, read now, so every session's liveness is re-validated when the
// answer is judged. Anything else is left as it is. A foreign owner is left
// without a scope only if listing the registry fails outright; a runtime
// directory that cannot be read lists no sessions, so its foreign answers
// are outside_session.
func (r *Runner) placeForeign(owner *ipc.EndpointOwner) {
	if owner == nil || owner.Result != ipc.OwnerForeign {
		return
	}
	sessions, err := registry.Discover()
	if err != nil {
		return
	}
	runID := ""
	if r.Project != nil {
		runID = r.Project.RunID
	}
	owner.Scope, owner.Session = placeAnswer(sessions, runID, owner.PID, owner.Address)
}

// placeAnswer finds the session an answer at address, held by holder, belongs
// to. This run's own session is recognised by its run ID or by a PID file
// naming this process; any other session counts only while its supervisor is
// proven live by PID plus start time (daemon.StartProven), so a stale or
// crashed session never does. A holder in a process group a session
// recorded, with the leader's identity proven (recordedGroupOf), is that
// session's whatever the port. Otherwise an answered port that a session was
// assigned, and so leases, is that session's. A match with this run's own
// session is no other session's and gets no scope; a match with another
// session is cross_session; no match is outside_session.
func placeAnswer(sessions []registry.Session, runID string, holder int, address string) (string, *ipc.OwnerSession) {
	port := 0
	if ap, err := netip.ParseAddrPort(address); err == nil {
		port = int(ap.Port())
	}
	var lease *ipc.OwnerSession
	leaseOwn := false
	for _, s := range sessions {
		own := (runID != "" && s.Project.RunID == runID) || (s.Record != nil && s.Record.PID == os.Getpid())
		if !own && (s.Record == nil || !daemon.StartProven(*s.Record)) {
			continue
		}
		process, portName, leased := s.PortAssignment(port)
		if s.Record != nil && s.Record.Groups != nil {
			if g, ok := recordedGroupOf(holder, *s.Record.Groups, s.Record.Host); ok {
				if own {
					return "", nil
				}
				if process != g.Name {
					portName = ""
				}
				return ipc.ScopeCrossSession, ownerSession(s, g.Name, portName, ipc.EvidenceHolder)
			}
		}
		if leased && lease == nil {
			lease, leaseOwn = ownerSession(s, process, portName, ipc.EvidencePortLease), own
		}
	}
	switch {
	case lease == nil:
		return ipc.ScopeOutsideSession, nil
	case leaseOwn:
		return "", nil
	}
	return ipc.ScopeCrossSession, lease
}

// ownerSession describes s as the session an answer belongs to.
func ownerSession(s registry.Session, process, port, evidence string) *ipc.OwnerSession {
	o := &ipc.OwnerSession{Name: s.Name(), RunID: s.Project.RunID, ConfigPath: s.Project.ConfigPath, Process: process, Port: port, Evidence: evidence}
	if s.Project.Owner != nil {
		o.Worktree = s.Project.Owner.Worktree
	}
	return o
}

// Policies that refuse a foreign answer, named in its warning.
const (
	refusedByReject       = "foreign_listener: reject"
	refusedAsCrossSession = "cross_session"
)

// refusalOf says which policy refuses owner, or "" when the answer counts.
// foreign_listener: reject refuses every foreign answer; refuseCrossSession
// (verify's default) refuses one that belongs to another live session. An
// owned, unattributable or outside_session answer is never refused by the
// latter.
func refusalOf(owner *ipc.EndpointOwner, reject, refuseCrossSession bool) string {
	switch {
	case owner == nil || owner.Result != ipc.OwnerForeign:
		return ""
	case reject:
		return refusedByReject
	case refuseCrossSession && owner.Scope == ipc.ScopeCrossSession:
		return refusedAsCrossSession
	}
	return ""
}

// foreignListenerMessage is the warning for a probe a foreign process
// answered. refusal is the policy that refused the answer, if any.
func foreignListenerMessage(name, probe string, o *ipc.EndpointOwner, refusal string) string {
	msg := fmt.Sprintf("%s was answered at %s by %s, which is not part of %s's process tree; another server holds this address",
		probe, o.Address, describeOwner(o), name)
	switch refusal {
	case refusedByReject:
		return msg + "; foreign_listener: reject does not count that answer"
	case refusedAsCrossSession:
		return msg + "; verify does not count another live session's answer (verify --allow-cross-session counts it)"
	}
	return msg + ", so the probe does not show that " + name + " is serving"
}

// foreignListenerError is the failed-attempt detail for a refused foreign
// answer: one foreign_listener: reject refused, or another live session's
// answer that Runner.RefuseCrossSession refused, which describeOwner names.
func foreignListenerError(name string, o *ipc.EndpointOwner) error {
	return fmt.Errorf("answered at %s by %s, outside %s's process tree", o.Address, describeOwner(o), name)
}

// ownerCheck attributes every attempt of one readiness probe run, for
// incarnation gen of st, when its connection is dialed, and judges the
// answered ones. Under foreign_listener: reject it refuses an answer
// attributed to a foreign process, and with Runner.RefuseCrossSession one
// that belongs to another live session: the refusal is recorded as the
// incarnation's readiness owner, and the first one is logged and broadcast.
// An unattributable answer is always taken. owner is who answered the
// attempt that was taken. A probe run calls it from one goroutine only.
type ownerCheck struct {
	r                  *Runner
	p                  procInfo
	st                 *procState
	gen                uint64
	probe              string
	reject             bool
	refuseCrossSession bool
	warned             bool
	owner              *ipc.EndpointOwner
}

func (r *Runner) newOwnerCheck(p procInfo, st *procState, gen uint64, kind string) *ownerCheck {
	return &ownerCheck{r: r, p: p, st: st, gen: gen, probe: "readiness probe (" + kind + ")",
		reject:             p.proc.EffectiveForeignListener() == config.ForeignListenerReject,
		refuseCrossSession: r.RefuseCrossSession}
}

func (c *ownerCheck) attribute(conn probeConn) *ipc.EndpointOwner { return c.r.attribute(c.p, conn) }

func (c *ownerCheck) accept(owner *ipc.EndpointOwner) error {
	if owner == nil {
		owner = &ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "the probe connection was not recorded"}
	}
	refusal := refusalOf(owner, c.reject, c.refuseCrossSession)
	if refusal == "" {
		c.owner = owner
		return nil
	}
	if c.st.recordRefusedOwner(c.gen, owner) && !c.warned {
		c.warned = true
		c.r.broadcastState(c.p, c.st)
		c.r.processLog(c.p, "warn", "readiness.foreign_listener", foreignListenerMessage(c.p.name, c.probe, owner, refusal))
	}
	return foreignListenerError(c.p.name, owner)
}
