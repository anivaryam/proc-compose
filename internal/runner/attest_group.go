//go:build linux || windows

package runner

// reasonGroupNotInspectable is the reason when attribution cannot read the
// group, such as a test double. Only the platforms that attribute sockets
// inspect groups.
const reasonGroupNotInspectable = "the process group cannot be inspected"
