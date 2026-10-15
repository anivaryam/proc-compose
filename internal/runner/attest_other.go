//go:build !linux && !windows

package runner

import (
	"runtime"

	"github.com/anivaryam/proc-compose/internal/ipc"
)

// attributeSocket is not implemented here: this platform's socket table is
// not read yet, so every answer is unattributable and changes nothing. macOS
// would need libproc or lsof.
func attributeSocket(probeConn, ProcessGroup) ipc.EndpointOwner {
	return ipc.EndpointOwner{Result: ipc.OwnerUnattributable, Reason: "socket ownership is not observed on " + runtime.GOOS}
}
