package execrec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Shim runs the record at dir (an absolute path as the shim sees it, inside the container): it starts cmd.sh with
// /bin/sh as the leader of a new session and process group, with stdout and stderr appended straight to out and
// err, writes pgid, waits, and writes exit. It is herd-guest's `shim` mode, started detached inside the container,
// so the record keeps being written when the herd-guest that started it is restarted.
func Shim(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, FileMeta))
	if err != nil {
		return fmt.Errorf("execrec shim: %w", err)
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("execrec shim: %s: %w", FileMeta, err)
	}
	out, err := os.OpenFile(filepath.Join(dir, FileOut), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("execrec shim: %w", err)
	}
	defer out.Close()
	errf, err := os.OpenFile(filepath.Join(dir, FileErr), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("execrec shim: %w", err)
	}
	defer errf.Close()

	cmd := exec.Command("/bin/sh", filepath.Join(dir, FileCmd))
	cmd.Stdout, cmd.Stderr = out, errf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stdinR, stdinW *os.File
	if m.Stdin {
		if err := syscall.Mkfifo(filepath.Join(dir, FileIn), 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("execrec shim: mkfifo: %w", err)
		}
		if stdinR, stdinW, err = os.Pipe(); err != nil {
			return fmt.Errorf("execrec shim: %w", err)
		}
		cmd.Stdin = stdinR
	}
	if err := cmd.Start(); err != nil {
		// Like a shell that cannot run its command: the reason on stderr, 127 as the exit.
		fmt.Fprintf(errf, "herd-guest: %v\n", err)
		return writeFileAtomic(filepath.Join(dir, FileExit), []byte("127\n"))
	}
	if stdinR != nil {
		stdinR.Close()
		go feedStdin(dir, stdinW)
	}
	if err := writeFileAtomic(filepath.Join(dir, FilePgid), []byte(strconv.Itoa(cmd.Process.Pid)+"\n")); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, FileExit), []byte(strconv.Itoa(exitStatus(cmd.Wait()))+"\n"))
}

// exitStatus is the process's exit code, or 128 + the signal that killed it.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 255
}

// feedStdin copies the in FIFO to the process's stdin. A writer that goes away (a guest restart) is not end of
// input: the FIFO is opened again for the next writer. Input ends when in.closed exists at the end of a writer.
func feedStdin(dir string, w *os.File) {
	defer w.Close()
	for {
		f, err := os.OpenFile(filepath.Join(dir, FileIn), os.O_RDONLY, 0)
		if err != nil {
			return
		}
		_, err = io.Copy(w, f)
		f.Close()
		if err != nil {
			return
		}
		if _, err := os.Stat(filepath.Join(dir, FileInClosed)); err == nil {
			return
		}
	}
}

func writeFileAtomic(name string, data []byte) error {
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("execrec shim: %w", err)
	}
	if err := os.Rename(tmp, name); err != nil {
		return fmt.Errorf("execrec shim: %w", err)
	}
	return nil
}

// ErrStdinClosed means stdin of the process was closed on purpose.
var ErrStdinClosed = errors.New("execrec: stdin is closed")

// OpenInput opens the in FIFO for writing, waiting until the shim has it open for reading (it reopens it between
// writers). It gives up when ctx ends or the process has exited.
func (s *Store) OpenInput(ctx context.Context, id string) (*os.File, error) {
	name := filepath.Join(RelDir(id), FileIn)
	for {
		if s.InputClosed(id) {
			return nil, ErrStdinClosed
		}
		f, err := s.root.OpenFile(name, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.ENXIO) && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("execrec: %w", err)
		}
		if st, rerr := s.Read(id); rerr == nil && st.Exited {
			return nil, ErrExited
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// ErrExited means the process has exited.
var ErrExited = errors.New("execrec: process has exited")

// CloseInput ends the process's stdin: it marks in.closed, then opens and closes the FIFO so the shim sees the end
// of a writer and finds the mark.
func (s *Store) CloseInput(ctx context.Context, id string) error {
	if s.InputClosed(id) {
		return ErrStdinClosed
	}
	f, err := s.OpenInput(ctx, id)
	if err != nil {
		return err
	}
	if err := s.writeAtomic(filepath.Join(RelDir(id), FileInClosed), nil, 0o600); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// InputClosed reports whether stdin was closed on purpose.
func (s *Store) InputClosed(id string) bool {
	_, err := s.root.Stat(filepath.Join(RelDir(id), FileInClosed))
	return err == nil
}
