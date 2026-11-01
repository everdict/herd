package oci

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ReportFlag makes herd-guest's container init (cinit) report on the descriptors Crun.Run preserves for it: its
// command's stdout and stderr go to OutputFD, and how the command ended is written to StatusFD (StatusLine) before
// cinit exits.
const (
	ReportFlag = "-report"
	OutputFD   = 3
	StatusFD   = 4
)

// The output a Watch keeps: the last TailLines lines, and at most TailBytes of them.
const (
	TailLines = 20
	TailBytes = 2 << 10
)

// StatusLine is what cinit writes on StatusFD for a command that exited with code, or (signal non-zero) was killed by
// signal.
func StatusLine(code int, signal syscall.Signal) string {
	if signal != 0 {
		return "signal " + strconv.Itoa(int(signal)) + "\n"
	}
	return "exit " + strconv.Itoa(code) + "\n"
}

// ContainerExit is how a container's main process ended, as its init reported it, with its last output.
type ContainerExit struct {
	Container string
	// Known is false when the init ended without reporting (it was killed, or never ran).
	Known  bool
	Code   int
	Signal syscall.Signal
	// Output is the end of the command's output (stdout and stderr together), at most TailLines lines.
	Output []string
}

// Status names how the process ended: "exited with status 1", "was killed by signal 9 (killed)".
func (e *ContainerExit) Status() string {
	switch {
	case !e.Known:
		return "ended without reporting a status"
	case e.Signal != 0:
		return fmt.Sprintf("was killed by signal %d (%v)", int(e.Signal), e.Signal)
	}
	return fmt.Sprintf("exited with status %d", e.Code)
}

func (e *ContainerExit) Error() string {
	s := fmt.Sprintf("container %s %s", e.Container, e.Status())
	if len(e.Output) == 0 {
		return s + "; it wrote no output"
	}
	return s + "; its last output:\n" + strings.Join(e.Output, "\n")
}

// Watch follows one container's main process through what its init reports (ReportFlag): it keeps the end of the
// output and learns the exit when the status descriptor closes, which happens only when the init itself is gone.
type Watch struct {
	name string
	done chan struct{}

	mu     sync.Mutex
	tail   []byte
	status string
	exit   *ContainerExit
}

// outputGrace bounds how long a Watch waits, once the init is gone, for the rest of the output to be read.
const outputGrace = 200 * time.Millisecond

// NewWatch follows container name's init through the read ends of its output and status descriptors; it reads
// both until they close, and closes them.
func NewWatch(name string, output, status io.ReadCloser) *Watch {
	w := &Watch{name: name, done: make(chan struct{})}
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		defer output.Close()
		buf := make([]byte, 4<<10)
		for {
			n, err := output.Read(buf)
			if n > 0 {
				w.keep(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer status.Close()
		line, _ := bufio.NewReader(io.LimitReader(status, 64)).ReadString('\n')
		_, _ = io.Copy(io.Discard, status)
		select {
		case <-outDone:
		case <-time.After(outputGrace):
		}
		w.finish(strings.TrimSpace(line))
		close(w.done)
	}()
	return w
}

func (w *Watch) keep(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tail = append(w.tail, b...)
	if over := len(w.tail) - 2*TailBytes; over > 0 {
		w.tail = append(w.tail[:0], w.tail[over:]...)
	}
}

func (w *Watch) finish(status string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := &ContainerExit{Container: w.name}
	if kind, n, ok := strings.Cut(status, " "); ok {
		if v, err := strconv.Atoi(n); err == nil {
			switch kind {
			case "exit":
				e.Known, e.Code = true, v
			case "signal":
				e.Known, e.Signal = true, syscall.Signal(v)
			}
		}
	}
	e.Output = lastLines(w.tail)
	w.exit, w.tail = e, nil
}

// lastLines is the end of out: at most TailLines lines, at most TailBytes, as valid UTF-8.
func lastLines(out []byte) []string {
	s := strings.TrimRight(string(out), "\n")
	if len(s) > TailBytes {
		s = s[len(s)-TailBytes:]
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
			s = s[i+1:]
		}
	}
	s = strings.ToValidUTF8(s, "")
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > TailLines {
		lines = lines[len(lines)-TailLines:]
	}
	return lines
}

// Done is closed once the container's init is gone.
func (w *Watch) Done() <-chan struct{} { return w.done }

// Exited answers how the main process ended, or nil while the container runs.
func (w *Watch) Exited() *ContainerExit {
	select {
	case <-w.done:
	default:
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exit
}
