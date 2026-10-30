package vz

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/everdict/herd/internal/plist"
)

// Console is the Mac's console session as the host reports it. A save file is sealed with a key of the logged-in
// user's keychain, which a locked session does not release; so a Mac declares MEMORY only while a user is at the
// console with the screen unlocked (spec R-D9).
type Console struct {
	// User is the short name of the user at the console; empty at the login window.
	User string `json:"user,omitempty"`
	// Locked is true while the console's screen is locked.
	Locked bool `json:"locked,omitempty"`
}

// Unlocked reports whether a user is at the console with the screen unlocked.
func (c Console) Unlocked() bool { return c.User != "" && !c.Locked }

func (c Console) String() string {
	switch {
	case c.User == "":
		return "no user at the console"
	case c.Locked:
		return "console of " + c.User + " locked"
	}
	return "console of " + c.User + " unlocked"
}

// Session reports the console session. IORegSession is the Mac's; vzfake.Console is the tests'.
type Session interface {
	Console(ctx context.Context) (Console, error)
}

// IORegSession reads the console session from the I/O Registry root (`ioreg -n Root -d1 -a`): the IOConsoleUsers
// entry on console and its CGSSessionScreenIsLocked, and the root's IOConsoleLocked. It needs no GUI session and
// no cgo, so a launchd daemon can ask it.
type IORegSession struct {
	// Run runs the command and returns its standard output; nil runs it with os/exec.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Console implements Session.
func (s IORegSession) Console(ctx context.Context) (Console, error) {
	run := s.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	out, err := run(ctx, "/usr/sbin/ioreg", "-n", "Root", "-d1", "-a")
	if err != nil {
		return Console{}, fmt.Errorf("vz: console session: ioreg: %w", err)
	}
	return ParseIORegRoot(out)
}

// ParseIORegRoot reads the console session from `ioreg -n Root -d1 -a` output. The screen counts as locked if the
// root says IOConsoleLocked or the console user's session says CGSSessionScreenIsLocked; a reply with neither the
// users array nor IOConsoleLocked is an error, not an unlocked screen.
func ParseIORegRoot(out []byte) (Console, error) {
	v, err := plist.Decode(out)
	if err != nil {
		return Console{}, fmt.Errorf("vz: console session: %w", err)
	}
	// -d1 on Root prints the root entry itself, or an array holding it.
	if a, ok := v.([]any); ok && len(a) > 0 {
		v = a[0]
	}
	root, ok := v.(map[string]any)
	if !ok {
		return Console{}, errors.New("vz: console session: ioreg root is not a dict")
	}
	users, hasUsers := root["IOConsoleUsers"].([]any)
	rootLocked, hasLocked := root["IOConsoleLocked"].(bool)
	if !hasUsers && !hasLocked {
		return Console{}, errors.New("vz: console session: ioreg reported neither IOConsoleUsers nor IOConsoleLocked")
	}
	var c Console
	for _, u := range users {
		d, ok := u.(map[string]any)
		if !ok {
			continue
		}
		if on, _ := d["kCGSSessionOnConsoleKey"].(bool); !on {
			continue
		}
		name, _ := d["kCGSSessionUserNameKey"].(string)
		locked, _ := d["CGSSessionScreenIsLocked"].(bool)
		c = Console{User: name, Locked: locked}
		break
	}
	if rootLocked {
		c.Locked = true
	}
	return c, nil
}
