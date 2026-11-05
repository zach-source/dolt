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
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	filelocks "github.com/dolthub/file-locks"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dolt/go/store/util/tempfiles"
)

// The locks belong to the open file rather than the process, so another Lock in this
// process stands in for another process.
func otherProcess(t *testing.T, processDir string) *filelocks.Lock {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(processDir), 0o755))
	lock, err := filelocks.New(spoolProcessLockPath(processDir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Close() })
	return lock
}

func newTestSpoolDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spools")
	t.Cleanup(func() {
		spoolDirHoldsMu.Lock()
		h, ok := spoolDirHolds[dir]
		delete(spoolDirHolds, dir)
		spoolDirHoldsMu.Unlock()
		if ok && h.lock != nil {
			_ = h.lock.Close()
		}
	})
	return dir
}

// writeProcessDir makes a subdirectory of |dir| holding a spool, with nobody holding its lock.
func writeProcessDir(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	processDir := filepath.Join(dir, "proc-"+name)
	require.NoError(t, os.MkdirAll(processDir, 0o755))
	require.NoError(t, os.WriteFile(spoolProcessLockPath(processDir), nil, 0o644))
	spool := filepath.Join(processDir, "nbs-spool-"+name)
	require.NoError(t, os.WriteFile(spool, []byte("spool"), 0o644))
	return processDir, spool
}

func closeSpoolFile(t *testing.T, f *os.File) {
	t.Helper()
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
}

func TestNewSpoolFile_LocksProcessDir(t *testing.T) {
	dir := newTestSpoolDir(t)
	f, err := newSpoolFile(dir)
	require.NoError(t, err)
	defer closeSpoolFile(t, f)

	processDir := filepath.Dir(f.Name())
	require.Equal(t, dir, filepath.Dir(processDir), "spools go in a subdirectory of the spool directory")
	require.Equal(t, filelocks.ErrLocked, otherProcess(t, processDir).TryLock())
}

func TestNewSpoolFile_ReusesProcessDir(t *testing.T) {
	dir := newTestSpoolDir(t)
	f1, err := newSpoolFile(dir)
	require.NoError(t, err)
	defer closeSpoolFile(t, f1)
	f2, err := newSpoolFile(dir)
	require.NoError(t, err)
	defer closeSpoolFile(t, f2)
	require.Equal(t, filepath.Dir(f1.Name()), filepath.Dir(f2.Name()))
}

func TestNewSpoolFile_DoesNotTouchOtherProcesses(t *testing.T) {
	dir := newTestSpoolDir(t)
	_, spool := writeProcessDir(t, dir, "other")
	f, err := newSpoolFile(dir)
	require.NoError(t, err)
	defer closeSpoolFile(t, f)
	_, err = os.Stat(spool)
	require.NoError(t, err)
}

func TestNewSpoolFile_RecoversFromRemovedDir(t *testing.T) {
	dir := newTestSpoolDir(t)
	f1, err := newSpoolFile(dir)
	require.NoError(t, err)
	require.NoError(t, f1.Close())
	require.NoError(t, os.RemoveAll(dir))

	f2, err := newSpoolFile(dir)
	require.NoError(t, err)
	defer closeSpoolFile(t, f2)
	require.Equal(t, filelocks.ErrLocked, otherProcess(t, filepath.Dir(f2.Name())).TryLock(), "the new lock file must be held")
}

func TestNewSpoolFile_ReturnsErrors(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user cannot write to")
	}
	dir := newTestSpoolDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, err := newSpoolFile(dir)
	require.ErrorIs(t, err, os.ErrPermission)
}

func TestUnlockSpoolDir_PanicsOnError(t *testing.T) {
	dir := t.TempDir()
	lock, err := filelocks.New(filepath.Join(dir, "LOCK"))
	require.NoError(t, err)
	defer lock.Close()
	// Unlocking a lock which is not held is an error.
	require.Panics(t, func() { unlockSpoolDir(lock, dir) })
}

func TestCleanSpoolDir(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "spools")
		require.NoError(t, CleanSpoolDir(dir))
		_, err := os.Stat(dir)
		require.True(t, os.IsNotExist(err), "cleaning must not make the directory")
	})
	t.Run("dead process", func(t *testing.T) {
		dir := newTestSpoolDir(t)
		processDir, _ := writeProcessDir(t, dir, "dead")
		require.NoError(t, CleanSpoolDir(dir))
		_, err := os.Stat(processDir)
		require.True(t, os.IsNotExist(err), "the dead process's subdirectory must be removed")
		_, err = os.Stat(spoolProcessLockPath(processDir))
		require.True(t, os.IsNotExist(err), "the dead process's lock file must be removed")
	})
	t.Run("live process", func(t *testing.T) {
		dir := newTestSpoolDir(t)
		processDir, spool := writeProcessDir(t, dir, "live")
		require.NoError(t, otherProcess(t, processDir).TryLock())
		require.NoError(t, CleanSpoolDir(dir), "a live process is not an error")
		_, err := os.Stat(spool)
		require.NoError(t, err, "a live process may be using this spool")
	})
	t.Run("dead process next to a live one", func(t *testing.T) {
		dir := newTestSpoolDir(t)
		liveDir, live := writeProcessDir(t, dir, "live")
		require.NoError(t, otherProcess(t, liveDir).TryLock())
		deadDir, _ := writeProcessDir(t, dir, "dead")
		require.NoError(t, CleanSpoolDir(dir))
		_, err := os.Stat(live)
		require.NoError(t, err)
		_, err = os.Stat(deadDir)
		require.True(t, os.IsNotExist(err), "a live process must not keep a dead one's spools around")
	})
	t.Run("this process is using it", func(t *testing.T) {
		dir := newTestSpoolDir(t)
		f, err := newSpoolFile(dir)
		require.NoError(t, err)
		defer closeSpoolFile(t, f)
		require.NoError(t, CleanSpoolDir(dir))
		_, err = os.Stat(f.Name())
		require.NoError(t, err)
	})
	t.Run("cleanup fails", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a directory this user cannot remove files from")
		}
		dir := newTestSpoolDir(t)
		processDir, _ := writeProcessDir(t, dir, "stuck")
		lock := otherProcess(t, processDir)
		require.NoError(t, os.Chmod(processDir, 0o555))
		t.Cleanup(func() { _ = os.Chmod(processDir, 0o755) })
		require.ErrorIs(t, CleanSpoolDir(dir), os.ErrPermission)
		require.NoError(t, lock.TryLock(), "a failed cleanup must not keep the lock")
	})
}

// Several cleanups racing each other and a process making its subdirectory must neither fail
// nor take away a subdirectory whose lock is held.
func TestNewSpoolProcessDir_RacingClean(t *testing.T) {
	dir := newTestSpoolDir(t)

	const cleaners = 4
	stop := make(chan struct{})
	cleaned := make(chan error, cleaners)
	for i := 0; i < cleaners; i++ {
		go func() {
			for {
				select {
				case <-stop:
					cleaned <- nil
					return
				default:
				}
				if err := CleanSpoolDir(dir); err != nil {
					cleaned <- err
					return
				}
			}
		}()
	}
	defer func() {
		close(stop)
		for i := 0; i < cleaners; i++ {
			require.NoError(t, <-cleaned)
		}
	}()

	for i := 0; i < 2000; i++ {
		processDir, lock, err := newSpoolProcessDir(dir)
		require.NoError(t, err, "iteration %d", i)
		_, err = os.Stat(processDir)
		require.NoError(t, err, "iteration %d: a subdirectory whose lock is held must not be removed", i)
		_, err = os.Stat(spoolProcessLockPath(processDir))
		require.NoError(t, err, "iteration %d: a held lock file must not be removed", i)
		require.NoError(t, lock.Close())
	}
}

func TestReleaseSpoolDir(t *testing.T) {
	require.NoError(t, ReleaseSpoolDir(filepath.Join(t.TempDir(), "never-held")))

	dir := newTestSpoolDir(t)
	f, err := newSpoolFile(dir)
	require.NoError(t, err)
	processDir := filepath.Dir(f.Name())
	other := otherProcess(t, processDir)

	require.NoError(t, ReleaseSpoolDir(dir))
	require.Equal(t, filelocks.ErrLocked, other.TryLock(), "a spool may still be in use, so the lock stays")

	closeSpoolFile(t, f)
	require.NoError(t, ReleaseSpoolDir(dir))
	_, err = os.Stat(processDir)
	require.True(t, os.IsNotExist(err), "a released subdirectory is removed")
	_, err = os.Stat(spoolProcessLockPath(processDir))
	require.True(t, os.IsNotExist(err), "a released lock file is removed")
	require.NoError(t, ReleaseSpoolDir(dir), "releasing twice is harmless")

	// The next spool makes a new subdirectory.
	f, err = newSpoolFile(dir)
	require.NoError(t, err)
	defer closeSpoolFile(t, f)
	require.NotEqual(t, processDir, filepath.Dir(f.Name()))
	require.Equal(t, filelocks.ErrLocked, otherProcess(t, filepath.Dir(f.Name())).TryLock())
}

// blockingTempFileProvider holds NewFile until the test lets it go on.
type blockingTempFileProvider struct {
	tempfiles.TempFileProvider
	entered chan struct{}
	proceed chan struct{}
}

func (p blockingTempFileProvider) NewFile(dir, pattern string) (*os.File, error) {
	close(p.entered)
	<-p.proceed
	return p.TempFileProvider.NewFile(dir, pattern)
}

// A release that runs while a spool is being created has to wait for it, and then see it.
// If it did not wait, it would remove the subdirectory the spool is about to go in.
func TestNewSpoolFile_ReleaseWaitsForSpoolBeingCreated(t *testing.T) {
	dir := newTestSpoolDir(t)
	f, err := newSpoolFile(dir)
	require.NoError(t, err)
	processDir := filepath.Dir(f.Name())
	closeSpoolFile(t, f)

	provider := blockingTempFileProvider{tempfiles.MovableTempFileProvider, make(chan struct{}), make(chan struct{})}
	tempfiles.MovableTempFileProvider = provider
	t.Cleanup(func() { tempfiles.MovableTempFileProvider = provider.TempFileProvider })

	type result struct {
		f   *os.File
		err error
	}
	created := make(chan result, 1)
	go func() {
		f, err := newSpoolFile(dir)
		created <- result{f, err}
	}()
	<-provider.entered

	released := make(chan error, 1)
	go func() { released <- ReleaseSpoolDir(dir) }()
	select {
	case <-released:
		close(provider.proceed)
		<-created
		require.Fail(t, "a release ran while a spool was being created")
	case <-time.After(100 * time.Millisecond):
	}

	close(provider.proceed)
	res := <-created
	require.NoError(t, res.err)
	defer closeSpoolFile(t, res.f)
	require.NoError(t, <-released)
	require.Equal(t, processDir, filepath.Dir(res.f.Name()))
	require.Equal(t, filelocks.ErrLocked, otherProcess(t, processDir).TryLock(), "the release must see the spool and keep the lock")
}

func TestCreateSpoolFile_UsesProcessDir(t *testing.T) {
	dir := newTestSpoolDir(t)
	f, _, err := createSpoolFile(dir, bytes.NewReader([]byte("table")))
	require.NoError(t, err)
	require.Equal(t, filelocks.ErrLocked, otherProcess(t, filepath.Dir(f.Name())).TryLock())

	require.NoError(t, removeSpoolFile(f))
	_, err = os.Stat(f.Name())
	require.True(t, os.IsNotExist(err))
}
