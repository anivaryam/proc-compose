// Package logrotate provides log file rotation when size thresholds are exceeded.
package logrotate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/anivaryam/proc-compose/internal/sharedfile"
)

// StagingSuffix names the temporary name the live log has while it is rotated.
//
// A rotation renames the live file aside before it shifts the rotated copies,
// so until that rename has succeeded nothing has changed and a failure costs
// nothing. From then until the staged file becomes the newest copy, the live
// path is absent and the newest bytes are in path+StagingSuffix; a reader of
// the log reads the staged file in place of the live one then.
const StagingSuffix = ".staging"

// openFile opens the live log for appending, and rename renames one file of
// the log. They are package-level variables so a test can make one step of a
// rotation fail.
var (
	openFile = sharedfile.OpenAppend
	rename   = os.Rename
)

// Writer wraps an os.File and rotates it when it exceeds maxSize bytes.
// It keeps up to maxFiles rotated copies (file.1, file.2, etc.).
//
// The file is opened through sharedfile, so on Windows it can be renamed while
// it is open and while readers have it open, as it can on Unix.
type Writer struct {
	path     string
	maxSize  int64
	maxFiles int

	mu      sync.Mutex
	file    *os.File
	written int64
	// displaced is where the open file is when a failed rotation could not
	// put it back at the live path. The next rotation puts it back first.
	displaced string
}

// New creates a rotating Writer that writes to path and rotates when the
// file exceeds maxSize bytes. Up to maxFiles rotated copies are kept.
//
// A rotation interrupted between staging the live file and making it the
// newest copy, by a crash for example, leaves the live file staged and the
// live path absent. New puts the staged file back before it opens the log, so
// its bytes stay in the log instead of being overwritten by the next rotation.
func New(path string, maxSize int64, maxFiles int) (*Writer, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if err := rename(path+StagingSuffix, path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("restore staged log %s: %w", path, err)
		}
	}
	f, err := openFile(path, 0600)
	if err != nil {
		return nil, err
	}
	// Account for existing file size.
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{
		path:     path,
		maxSize:  maxSize,
		maxFiles: maxFiles,
		file:     f,
		written:  info.Size(),
	}, nil
}

// Write writes p to the live log, rotating it when it has grown past the
// configured size.
//
// The bytes of a write that triggers a rotation are written either way. If
// the rotation then fails, the write reports that failure as well: the
// journal's owner has to learn that the record is degraded. A failed rotation
// is rolled back, so the writer keeps appending to the live file and every
// later write retries the rotation, reporting the failure again, until a
// rotation succeeds.
func (w *Writer) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err = w.file.Write(p)
	w.written += int64(n)

	if w.written >= w.maxSize {
		if rerr := w.rotate(); rerr != nil && err == nil {
			err = rerr
		}
	}
	return n, err
}

// Close closes the underlying file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// rotate rotates the current file: the live file is staged under a temporary
// name, the rotated copies are shifted, the staged file becomes the newest
// copy and a fresh empty live file takes its place.
//
// A failure rolls the rotation back: the open file goes back to the live path,
// so the writer keeps appending to it, no byte leaves the files a reader reads,
// and the next write retries. The rotated copies keep the shift that was done;
// the retry's shift continues from the first free slot, so retrying never
// drops another copy. rotate never assigns a nil handle and never resets the
// size counter unless a new live file was installed.
//
// When the live file has been removed by another process, rotate installs a
// new live file instead and reports that the bytes written to the removed one
// are not in the log.
//
// Must be called with w.mu held.
func (w *Writer) rotate() error {
	if w.displaced != "" {
		if err := rename(w.displaced, w.path); err != nil {
			return warn(fmt.Errorf("restore live log %s from %s: %w", w.path, w.displaced, err))
		}
		w.displaced = ""
	}

	// Stage the live file first. Nothing else has happened yet, so a failure
	// here costs nothing.
	staging := w.path + StagingSuffix
	if err := rename(w.path, staging); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return w.recreate()
		}
		return warn(fmt.Errorf("stage current log %s: %w", w.path, err))
	}
	if err := w.shift(); err != nil {
		return w.rollback(staging, err)
	}
	rotated := w.copyName(1)
	if err := rename(staging, rotated); err != nil {
		return w.rollback(staging, fmt.Errorf("rotate current log to %s: %w", rotated, err))
	}
	// The live path is absent, so the new live file starts empty.
	f, err := openFile(w.path, 0600)
	if err != nil {
		return w.rollback(rotated, fmt.Errorf("reopen log file %s: %w", w.path, err))
	}
	w.install(f)
	return nil
}

// shift frees the slot of the newest rotated copy, .1. It moves the copies up
// only as far as the first free slot, so the oldest copy is dropped only when
// every slot is taken: a rotation retried after a rollback finds .1 free and
// drops nothing more.
//
// A failed step stops the shift, since going on would rename a copy over one
// that is still there.
func (w *Writer) shift() error {
	slots := max(w.maxFiles, 1)
	free := 0
	for i := 1; i <= slots; i++ {
		if _, err := os.Lstat(w.copyName(i)); errors.Is(err, fs.ErrNotExist) {
			free = i
			break
		}
	}
	if free == 0 {
		free = slots
		if err := os.Remove(w.copyName(slots)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove old log %s: %w", w.copyName(slots), err)
		}
	}
	for i := free - 1; i >= 1; i-- {
		if err := rename(w.copyName(i), w.copyName(i+1)); err != nil {
			return fmt.Errorf("rotate log %s -> %s: %w", w.copyName(i), w.copyName(i+1), err)
		}
	}
	return nil
}

// rollback puts the open file, which a failed rotation step left at name,
// back at the live path and returns cause. When that rename fails too, the
// file stays where it is: a reader still finds it (as the staged file or as
// the newest copy) and the next rotation puts it back first.
func (w *Writer) rollback(name string, cause error) error {
	if err := rename(name, w.path); err != nil {
		w.displaced = name
		cause = errors.Join(cause, fmt.Errorf("restore live log %s from %s: %w", w.path, name, err))
	}
	return warn(cause)
}

// recreate installs a new live file after the live file was removed or
// renamed by another process. The bytes written since then are not in the
// log; the error says so once, and the writer carries on in the new file.
func (w *Writer) recreate() error {
	f, err := openFile(w.path, 0600)
	if err != nil {
		return warn(fmt.Errorf("recreate removed log %s: %w", w.path, err))
	}
	w.install(f)
	return warn(fmt.Errorf("log %s was removed while it was written: recreated it, but the bytes written to the removed file are not in the log", w.path))
}

// install makes f the live file. The old handle is closed last: until the
// replacement exists the writer keeps a usable handle.
func (w *Writer) install(f *os.File) {
	old := w.file
	w.file = f
	w.written = 0
	old.Close()
}

func (w *Writer) copyName(i int) string {
	return fmt.Sprintf("%s.%d", w.path, i)
}

// warn reports a rotation failure on stderr as well, since the journal's
// owner may surface the returned error only at the end of the run.
func warn(err error) error {
	fmt.Fprintf(os.Stderr, "WARN: %v\n", err)
	return err
}
