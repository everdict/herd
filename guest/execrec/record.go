// Package execrec is Everdict's detached-exec record, kept inside the target container's filesystem:
//
//	/everdict/exec/<id>/cmd.sh   the command, as a POSIX shell script (cwd, extra env, argv)
//	/everdict/exec/<id>/pgid     the process group id, written once the group runs
//	/everdict/exec/<id>/out      stdout, appended by the process itself
//	/everdict/exec/<id>/err      stderr, appended by the process itself
//	/everdict/exec/<id>/exit     the exit status (code, or 128+signal), written once it ended
//	/everdict/exec/<id>/in       a FIFO feeding stdin (only when stdin was asked for)
//
// herd-guest adds meta.json (the start request) and in.closed (stdin was closed on purpose). The record is the
// durable truth: the process writes out/err directly and a shim inside the container (Shim) writes pgid and exit,
// so nothing depends on the herd-guest process that started it. A restarted guest reads the record back and
// follows out/err from byte offsets.
package execrec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Dir is where records live inside a container.
const Dir = "/everdict/exec"

// File names inside one record.
const (
	FileCmd      = "cmd.sh"
	FilePgid     = "pgid"
	FileOut      = "out"
	FileErr      = "err"
	FileExit     = "exit"
	FileIn       = "in"
	FileMeta     = "meta.json"
	FileInClosed = "in.closed"
)

// ErrNotFound means no record has the id.
var ErrNotFound = errors.New("execrec: no such exec record")

// Meta is the start request, kept so a restarted guest can describe the process.
type Meta struct {
	ID        string            `json:"id"`
	Command   []string          `json:"command"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Stdin     bool              `json:"stdin"`
	Timeout   time.Duration     `json:"timeout"`
	StartedAt time.Time         `json:"startedAt"`
}

// ValidID reports whether id can name a record: 1 to 64 of [a-zA-Z0-9_-].
func ValidID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// RelDir is the record's directory relative to the container root (for os.Root).
func RelDir(id string) string { return strings.TrimPrefix(path.Join(Dir, id), "/") }

// Store reads and writes records under one container root. Every access goes through os.Root, so a symlink the
// container planted under /everdict cannot make the guest touch a path outside the container.
type Store struct {
	root *os.Root
}

// OpenStore opens the store of the container whose root filesystem is at rootfs (as the guest sees it).
func OpenStore(rootfs string) (*Store, error) {
	r, err := os.OpenRoot(rootfs)
	if err != nil {
		return nil, fmt.Errorf("execrec: %w", err)
	}
	return &Store{root: r}, nil
}

// Close releases the root.
func (s *Store) Close() error { return s.root.Close() }

// Create writes a new record's cmd.sh and meta.json. The shim creates the rest when it runs.
func (s *Store) Create(m Meta) error {
	if !ValidID(m.ID) {
		return fmt.Errorf("execrec: invalid id %q", m.ID)
	}
	if len(m.Command) == 0 {
		return errors.New("execrec: empty command")
	}
	dir := RelDir(m.ID)
	if err := s.root.MkdirAll(path.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("execrec: %w", err)
	}
	if err := s.root.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("execrec: %w", err)
	}
	meta, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("execrec: %w", err)
	}
	if err := s.writeAtomic(path.Join(dir, FileMeta), meta, 0o600); err != nil {
		return err
	}
	return s.writeAtomic(path.Join(dir, FileCmd), []byte(Script(m)), 0o700)
}

// Chown hands a created record to uid:gid, the identity its shim runs as in the container: the shim reads the record
// and writes its output, pgid and exit there.
func (s *Store) Chown(id string, uid, gid int) error {
	if !ValidID(id) {
		return fmt.Errorf("execrec: invalid id %q", id)
	}
	dir := RelDir(id)
	for _, name := range []string{dir, path.Join(dir, FileMeta), path.Join(dir, FileCmd)} {
		if err := s.root.Lchown(name, uid, gid); err != nil {
			return fmt.Errorf("execrec: %w", err)
		}
	}
	return nil
}

func (s *Store) writeAtomic(name string, data []byte, mode fs.FileMode) error {
	tmp := name + ".tmp"
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("execrec: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("execrec: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("execrec: %w", err)
	}
	if err := s.root.Rename(tmp, name); err != nil {
		return fmt.Errorf("execrec: %w", err)
	}
	return nil
}

// Script renders the command as the cmd.sh the shim runs with /bin/sh.
func Script(m Meta) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# herd-guest exec " + m.ID + "\n")
	if m.Cwd != "" {
		b.WriteString("cd " + Quote(m.Cwd) + " || exit 126\n")
	}
	keys := make([]string, 0, len(m.Env))
	for k := range m.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("export " + k + "=" + Quote(m.Env[k]) + "\n")
	}
	b.WriteString("exec")
	for _, a := range m.Command {
		b.WriteString(" " + Quote(a))
	}
	b.WriteString("\n")
	return b.String()
}

// Quote quotes s for a POSIX shell.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// State is what the record says about a process.
type State struct {
	Meta Meta
	// Pgid is 0 until the shim has started the process group.
	Pgid int
	// Exited is true once exit is written; ExitCode and FinishedAt are valid then.
	Exited     bool
	ExitCode   int
	FinishedAt time.Time
}

// Exists reports whether a record with this id is in the store.
func (s *Store) Exists(id string) bool {
	if !ValidID(id) {
		return false
	}
	_, err := s.root.Stat(path.Join(RelDir(id), FileMeta))
	return err == nil
}

// List returns the ids of every record in the store.
func (s *Store) List() ([]string, error) {
	f, err := s.root.Open(strings.TrimPrefix(Dir, "/"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("execrec: %w", err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("execrec: %w", err)
	}
	var ids []string
	for _, n := range names {
		if s.Exists(n) {
			ids = append(ids, n)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Read returns the record's state.
func (s *Store) Read(id string) (State, error) {
	if !ValidID(id) {
		return State{}, ErrNotFound
	}
	dir := RelDir(id)
	b, err := s.root.ReadFile(path.Join(dir, FileMeta))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, ErrNotFound
	}
	if err != nil {
		return State{}, fmt.Errorf("execrec: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st.Meta); err != nil {
		return State{}, fmt.Errorf("execrec: %s: %w", FileMeta, err)
	}
	if n, ok, err := s.readInt(path.Join(dir, FilePgid)); err != nil {
		return State{}, err
	} else if ok {
		st.Pgid = n
	}
	n, ok, err := s.readInt(path.Join(dir, FileExit))
	if err != nil {
		return State{}, err
	}
	if ok {
		st.Exited, st.ExitCode = true, n
		if fi, err := s.root.Stat(path.Join(dir, FileExit)); err == nil {
			st.FinishedAt = fi.ModTime()
		}
	}
	return st, nil
}

func (s *Store) readInt(name string) (int, bool, error) {
	b, err := s.root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("execrec: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false, fmt.Errorf("execrec: %s: %q is not a number", name, b)
	}
	return n, true, nil
}

// MaxChunk bounds one ReadOutput result.
const MaxChunk = 64 << 10

// ReadOutput reads stream (FileOut or FileErr) from byte offset off, at most MaxChunk bytes, and returns the bytes
// and the next offset. A stream that does not exist yet reads as empty.
func (s *Store) ReadOutput(id, stream string, off int64) ([]byte, int64, error) {
	if stream != FileOut && stream != FileErr {
		return nil, off, fmt.Errorf("execrec: %q is not an output stream", stream)
	}
	f, err := s.root.Open(path.Join(RelDir(id), stream))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, off, nil
	}
	if err != nil {
		return nil, off, fmt.Errorf("execrec: %w", err)
	}
	defer f.Close()
	buf := make([]byte, MaxChunk)
	n, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, off, fmt.Errorf("execrec: %w", err)
	}
	return buf[:n], off + int64(n), nil
}
