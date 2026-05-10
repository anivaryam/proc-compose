package ipc

// logRing is a fixed-capacity ring buffer of log entries. We use it instead
// of a sliding slice (`s = s[1:]`) because the sliding form forces a memmove
// every push once the cap is reached, and BroadcastLog runs once per line
// of every managed process — easily thousands per second under load.
//
// Push is O(1). Snapshot is O(n) only when called.
type logRing struct {
	buf  []LogEntry
	head int // index of next write
	n    int // number of valid entries (≤ cap(buf))
}

func newLogRing(capacity int) *logRing {
	return &logRing{buf: make([]LogEntry, 0, capacity)}
}

func (r *logRing) push(e LogEntry) {
	cap := cap(r.buf)
	if r.n < cap {
		r.buf = append(r.buf, e)
		r.n++
		r.head = r.n % cap
		return
	}
	r.buf[r.head] = e
	r.head = (r.head + 1) % cap
}

// snapshot returns a copy of the entries in oldest-to-newest order.
func (r *logRing) snapshot() []LogEntry {
	out := make([]LogEntry, r.n)
	if r.n == 0 {
		return out
	}
	if r.n < cap(r.buf) {
		copy(out, r.buf[:r.n])
		return out
	}
	// Buffer is full: oldest entry is at head.
	tail := len(r.buf) - r.head
	copy(out, r.buf[r.head:])
	copy(out[tail:], r.buf[:r.head])
	return out
}

