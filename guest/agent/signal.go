package agent

import (
	"syscall"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
)

// signals maps the API's signals to the platform's by name, as agent-substrate/env's guest does.
var signals = map[ateenv.Signal]syscall.Signal{
	ateenv.Signal_SIGNAL_HUP: syscall.SIGHUP, ateenv.Signal_SIGNAL_INT: syscall.SIGINT,
	ateenv.Signal_SIGNAL_QUIT: syscall.SIGQUIT, ateenv.Signal_SIGNAL_ILL: syscall.SIGILL,
	ateenv.Signal_SIGNAL_TRAP: syscall.SIGTRAP, ateenv.Signal_SIGNAL_ABRT: syscall.SIGABRT,
	ateenv.Signal_SIGNAL_BUS: syscall.SIGBUS, ateenv.Signal_SIGNAL_FPE: syscall.SIGFPE,
	ateenv.Signal_SIGNAL_KILL: syscall.SIGKILL, ateenv.Signal_SIGNAL_USR1: syscall.SIGUSR1,
	ateenv.Signal_SIGNAL_SEGV: syscall.SIGSEGV, ateenv.Signal_SIGNAL_USR2: syscall.SIGUSR2,
	ateenv.Signal_SIGNAL_PIPE: syscall.SIGPIPE, ateenv.Signal_SIGNAL_ALRM: syscall.SIGALRM,
	ateenv.Signal_SIGNAL_TERM: syscall.SIGTERM, ateenv.Signal_SIGNAL_CHLD: syscall.SIGCHLD,
	ateenv.Signal_SIGNAL_CONT: syscall.SIGCONT, ateenv.Signal_SIGNAL_STOP: syscall.SIGSTOP,
	ateenv.Signal_SIGNAL_TSTP: syscall.SIGTSTP, ateenv.Signal_SIGNAL_TTIN: syscall.SIGTTIN,
	ateenv.Signal_SIGNAL_TTOU: syscall.SIGTTOU, ateenv.Signal_SIGNAL_URG: syscall.SIGURG,
	ateenv.Signal_SIGNAL_XCPU: syscall.SIGXCPU, ateenv.Signal_SIGNAL_XFSZ: syscall.SIGXFSZ,
	ateenv.Signal_SIGNAL_VTALRM: syscall.SIGVTALRM, ateenv.Signal_SIGNAL_PROF: syscall.SIGPROF,
	ateenv.Signal_SIGNAL_WINCH: syscall.SIGWINCH, ateenv.Signal_SIGNAL_IO: syscall.SIGIO,
	ateenv.Signal_SIGNAL_SYS: syscall.SIGSYS,
}

func toSyscallSignal(s ateenv.Signal) (syscall.Signal, bool) {
	sig, ok := signals[s]
	return sig, ok
}
