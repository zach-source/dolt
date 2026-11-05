// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nbs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	filelocks "github.com/dolthub/file-locks"

	"github.com/dolthub/dolt/go/store/util/tempfiles"
)

// Spool files are removed when their reader closes, or at a clean exit. A process that
// dies first (OOM kill, SIGKILL, power loss) leaves its spools behind.
//
// So every process keeps its spools in its own subdirectory of the spool directory, and
// holds an exclusive lock on the lock file next to it for as long as it uses it:
//
//	spools/proc-123.lock
//	spools/proc-123/nbs-spool-*
//
// The kernel drops the lock when the process exits, however it exits. CleanSpoolDir tries
// once to take each lock. If it gets it, the owner is gone, so it removes the subdirectory.
// If not, the owner is alive, so it moves on without waiting.
//
// A lock file is the last thing removed, while its lock is still held. Opening a lock file
// makes it if it is not there, so one inside the subdirectory could reappear partway through
// the removal and stop the subdirectory from being removed.
//
// The dolt binary calls CleanSpoolDir when it starts.
//
// Reads do not touch the lock. They use files which are already open, in a subdirectory
// that cannot be cleaned while this process holds its lock.
const (
	spoolProcessPattern    = "proc-*.lock"
	spoolProcessLockSuffix = ".lock"

	// A cleanup can take a lock between the creation of its file and its owner taking it.
	// That takes a cleanup running at just that moment, so a few attempts are plenty.
	spoolProcessDirAttempts = 10
)

// spoolProcessLockPath returns the lock file of the subdirectory |processDir|.
func spoolProcessLockPath(processDir string) string {
	return processDir + spoolProcessLockSuffix
}

// unlockSpoolDir releases |lock|, which this process holds on the spool directory |dir|.
// A lock which cannot be released would keep a directory from being cleaned for as long as
// this process lives, so a failure panics.
func unlockSpoolDir(lock *filelocks.Lock, dir string) {
	if err := lock.Unlock(); err != nil {
		panic(fmt.Sprintf("failed to unlock spool directory %s: %v", dir, err))
	}
}

// spoolDirHold is this process's subdirectory of a spool directory, and its lock.
type spoolDirHold struct {
	mu         sync.Mutex
	processDir string
	lock       *filelocks.Lock
}

var (
	spoolDirHoldsMu sync.Mutex
	spoolDirHolds   = map[string]*spoolDirHold{}
)

// spoolDirHoldFor returns the hold for |dir|, making one if there is none.
func spoolDirHoldFor(dir string) *spoolDirHold {
	spoolDirHoldsMu.Lock()
	defer spoolDirHoldsMu.Unlock()
	h, ok := spoolDirHolds[dir]
	if !ok {
		h = &spoolDirHold{}
		spoolDirHolds[dir] = h
	}
	return h
}

// newSpoolFile makes an empty spool file in this process's subdirectory of |dir|. The file is
// made under h.mu so that ReleaseSpoolDir either sees it or runs before the subdirectory is
// made again. So while this process does not hold a lock, it has no spools in |dir|.
func newSpoolFile(dir string) (*os.File, error) {
	h := spoolDirHoldFor(dir)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.holdLocked(dir); err != nil {
		return nil, err
	}
	return tempfiles.MovableTempFileProvider.NewFile(h.processDir, "nbs-spool-")
}

// holdLocked makes sure this process has its locked subdirectory of |dir|. The caller holds
// h.mu.
func (h *spoolDirHold) holdLocked(dir string) error {
	if h.lock != nil {
		_, err := os.Stat(h.processDir)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		// The subdirectory was removed under us, so our lock excludes nobody now.
		unlockSpoolDir(h.lock, h.processDir)
		err = h.lock.Close()
		h.processDir, h.lock = "", nil
		if err != nil {
			return err
		}
	}
	processDir, lock, err := newSpoolProcessDir(dir)
	if err != nil {
		return err
	}
	h.processDir, h.lock = processDir, lock
	return nil
}

// newSpoolProcessDir makes a new subdirectory of |dir| and takes its lock.
func newSpoolProcessDir(dir string) (string, *filelocks.Lock, error) {
	var err error
	for i := 0; i < spoolProcessDirAttempts; i++ {
		var processDir string
		var lock *filelocks.Lock
		processDir, lock, err = tryNewSpoolProcessDir(dir)
		if err == nil {
			return processDir, lock, nil
		}
		if !errors.Is(err, filelocks.ErrLocked) && !errors.Is(err, os.ErrNotExist) {
			return "", nil, err
		}
	}
	return "", nil, err
}

// tryNewSpoolProcessDir makes one attempt at newSpoolProcessDir. A cleanup can get to the new
// lock file first, which shows up as its lock being held.
func tryNewSpoolProcessDir(dir string) (string, *filelocks.Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(dir, spoolProcessPattern)
	if err != nil {
		return "", nil, err
	}
	lockPath := f.Name()
	if err := f.Close(); err != nil {
		return "", nil, errors.Join(err, os.Remove(lockPath))
	}
	lock, err := filelocks.New(lockPath)
	if err != nil {
		return "", nil, err
	}
	if err := lock.TryLock(); err != nil {
		return "", nil, errors.Join(err, lock.Close())
	}
	processDir := strings.TrimSuffix(lockPath, spoolProcessLockSuffix)
	if err := os.Mkdir(processDir, 0o755); err != nil {
		err = errors.Join(err, os.Remove(lockPath))
		unlockSpoolDir(lock, processDir)
		return "", nil, errors.Join(err, lock.Close())
	}
	return processDir, lock, nil
}

// CleanSpoolDir removes the subdirectories of |dir| left by dead processes. It does not wait
// for a lock, and it does nothing if |dir| does not exist.
func CleanSpoolDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), spoolProcessLockSuffix) {
			errs = append(errs, cleanSpoolProcess(filepath.Join(dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

// cleanSpoolProcess removes the subdirectory locked by |lockPath| and then the lock file, if
// it can take the lock right away.
func cleanSpoolProcess(lockPath string) error {
	lock, err := filelocks.New(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	err = lock.TryLock()
	if errors.Is(err, filelocks.ErrLocked) {
		return lock.Close()
	}
	if err != nil {
		return errors.Join(err, lock.Close())
	}
	processDir := strings.TrimSuffix(lockPath, spoolProcessLockSuffix)
	err = removeSpoolProcess(processDir)
	unlockSpoolDir(lock, processDir)
	return errors.Join(err, lock.Close())
}

// removeSpoolProcess removes |processDir| and then its lock file. The caller holds the lock.
func removeSpoolProcess(processDir string) error {
	if err := os.RemoveAll(processDir); err != nil {
		return err
	}
	return os.Remove(spoolProcessLockPath(processDir))
}

// ReleaseSpoolDir gives up this process's lock on its subdirectory of |dir| and removes it,
// for callers which have closed the stores spooling into it. If spools are still there, they
// may still be in use, so the lock is kept until the process exits.
func ReleaseSpoolDir(dir string) error {
	spoolDirHoldsMu.Lock()
	h, ok := spoolDirHolds[dir]
	spoolDirHoldsMu.Unlock()
	if !ok {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lock == nil {
		return nil
	}
	entries, err := os.ReadDir(h.processDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(entries) > 0 {
		return nil
	}
	err = removeSpoolProcess(h.processDir)
	unlockSpoolDir(h.lock, h.processDir)
	err = errors.Join(err, h.lock.Close())
	h.processDir, h.lock = "", nil
	return err
}
