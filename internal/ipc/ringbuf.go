package ipc

import "encoding/json"

// logRing is a fixed-capacity ring buffer of log entries. We use it instead
// of a sliding slice (`s = s[1:]`) because the sliding form forces a memmove
// every push once the cap is reached, and BroadcastLog runs once per line
// of every managed process — easily thousands per second under load.
//
// Push is O(1). Snapshot is O(n) only when called.
type logRing struct {
	buf   []LogEntry
	head  int // index of next write
	n     int // number of valid entries (≤ cap(buf))
	sizes []int
	bytes int
}

func newLogRing(capacity int) *logRing {
	return &logRing{buf: make([]LogEntry, capacity), sizes: make([]int, capacity)}
}

func (r *logRing) push(e LogEntry, encodedSize ...int) {
	const budget = 256 << 10
	capacity := len(r.buf)
	if capacity == 0 {
		return
	}
	size := 0
	if len(encodedSize) > 0 {
		size = encodedSize[0]
	} else {
		data, _ := json.Marshal(e)
		size = len(data)
	}
	if size > budget {
		return
	}
	for r.n > 0 && (r.n == capacity || r.bytes+size > budget) {
		oldest := (r.head - r.n + capacity) % capacity
		r.bytes -= r.sizes[oldest]
		r.buf[oldest] = LogEntry{}
		r.n--
	}
	r.buf[r.head], r.sizes[r.head] = e, size
	r.bytes += size
	r.n++
	r.head = (r.head + 1) % capacity
}

// snapshot returns a copy of the entries in oldest-to-newest order.
func (r *logRing) snapshot() []LogEntry {
	out := make([]LogEntry, r.n)
	if r.n == 0 {
		return out
	}
	oldest := (r.head - r.n + len(r.buf)) % len(r.buf)
	for i := range out {
		out[i] = r.buf[(oldest+i)%len(r.buf)]
	}
	return out
}
