package vz_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/vz"
	"github.com/everdict/herd/vmm/vz/vzfake"
)

func declared(t *testing.T, v *vz.VMM) vmm.Capabilities {
	t.Helper()
	c, err := v.Capabilities(ctxT(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestLockStateDecidesMemory (criterion lock-state): a Mac whose probe passed declares MEMORY only while a user is
// at the console with the screen unlocked. A locked screen, the login window, a session that does not answer and a
// Mac with no session check all declare VOLUMES with the reason, and a MEMORY snapshot is refused while locked.
func TestLockStateDecidesMemory(t *testing.T) {
	con := vzfake.UnlockedConsole()
	v := newVMM(t, vzfake.New(), vz.Config{Host: "nahyun-imac", Probe: probeGuest, Session: con})
	if c := declared(t, v); c.SuspendMaxFidelity != vmm.FidelityMemory || c.RestoreFamily != "microvm-vz-arm64-nahyun-imac" {
		t.Fatalf("unlocked Mac with a passing probe declared %+v, want MEMORY", c)
	}
	spec := specIn(t.TempDir())("lk1")
	if _, err := v.Boot(ctxT(t, 5*time.Second), spec); err != nil {
		t.Fatal(err)
	}

	con.Lock(true)
	c := declared(t, v)
	if c.SuspendMaxFidelity != vmm.FidelityVolumes || c.RestoreFamily != "" || !strings.Contains(c.Reason, "locked") {
		t.Errorf("locked Mac declared %+v, want VOLUMES because locked", c)
	}
	if _, err := v.Snapshot(ctxT(t, 5*time.Second), "lk1", vmm.FidelityMemory); !errors.Is(err, vmm.ErrFidelityUnsupported) {
		t.Errorf("MEMORY snapshot on a locked Mac = %v, want ErrFidelityUnsupported", err)
	}

	con.Lock(false)
	if c := declared(t, v); c.SuspendMaxFidelity != vmm.FidelityMemory {
		t.Errorf("unlocked again: %+v, want MEMORY", c)
	}

	con.Set(vz.Console{})
	if c := declared(t, v); c.SuspendMaxFidelity != vmm.FidelityVolumes || !strings.Contains(c.Reason, "no user") {
		t.Errorf("login window: %+v, want VOLUMES because no user is at the console", c)
	}
	con.Set(vz.Console{User: "herd"})
	con.Fail(errors.New("ioreg: exit status 1"))
	if c := declared(t, v); c.SuspendMaxFidelity != vmm.FidelityVolumes || !strings.Contains(c.Reason, "unknown") {
		t.Errorf("session unanswered: %+v, want VOLUMES because the session is unknown", c)
	}

	// No session check at all: never MEMORY, whatever the probe says.
	nos, err := vz.New(vz.Config{StateDir: t.TempDir(), Host: "mac-n", Probe: probeGuest}, vzfake.New())
	if err != nil {
		t.Fatal(err)
	}
	if c := declared(t, nos); c.SuspendMaxFidelity != vmm.FidelityVolumes {
		t.Errorf("Mac with no session check declared %+v, want VOLUMES", c)
	}
	if r := nos.Probe(ctxT(t, 5*time.Second)); !r.Passed() {
		t.Errorf("probe = %+v; the probe passing is what makes the VOLUMES above the session's doing", r)
	}
}

// TestProbeIsReEvaluated (criterion lock-state): the probe is run again on wake (InvalidateProbe), once its result
// is older than ProbeInterval, and when the console becomes unlocked after a probe that ran while it was locked —
// the R0 spike's "permission denied" came from sessions that could not reach the keychain.
func TestProbeIsReEvaluated(t *testing.T) {
	hv := vzfake.New()
	con := vzfake.UnlockedConsole()
	con.Lock(true)
	hv.Fail(vzfake.CallSave, vzfake.ErrPermissionDenied)
	v := newVMM(t, hv, vz.Config{Probe: probeGuest, Session: con, ProbeInterval: time.Hour})
	if c := declared(t, v); c.SuspendMaxFidelity != vmm.FidelityVolumes || !strings.Contains(c.Reason, "permission denied") {
		t.Fatalf("locked Mac whose save is refused: %+v, want VOLUMES (probe: permission denied)", c)
	}
	probes := len(hv.Machines())
	if c := declared(t, v); c.SuspendMaxFidelity != vmm.FidelityVolumes || len(hv.Machines()) != probes {
		t.Errorf("still locked: %+v after %d machines, want VOLUMES and no new probe", c, len(hv.Machines())-probes)
	}

	// The user unlocks; the keychain is reachable and the save goes through.
	hv.Clear()
	con.Lock(false)
	if c := declared(t, v); c.SuspendMaxFidelity != vmm.FidelityMemory {
		t.Errorf("unlocked after a locked probe: %+v, want the probe re-run and MEMORY", c)
	}
	probes = len(hv.Machines())
	if c := declared(t, v); c.SuspendMaxFidelity != vmm.FidelityMemory || len(hv.Machines()) != probes {
		t.Errorf("no change: %+v, %d new machines, want MEMORY from the standing probe", c, len(hv.Machines())-probes)
	}

	// Wake: the probe runs again and this time the framework refuses the save.
	hv.Fail(vzfake.CallSave, vzfake.ErrPermissionDenied)
	v.InvalidateProbe()
	if c := declared(t, v); c.SuspendMaxFidelity != vmm.FidelityVolumes || len(hv.Machines()) == probes {
		t.Errorf("after wake with save refused: %+v, want the probe re-run and VOLUMES", c)
	}

	// Interval: a passing probe older than ProbeInterval is run again.
	hv.Clear()
	short := newVMM(t, hv, vz.Config{Probe: probeGuest, Session: vzfake.UnlockedConsole(), ProbeInterval: 50 * time.Millisecond})
	declared(t, short)
	probes = len(hv.Machines())
	time.Sleep(80 * time.Millisecond)
	declared(t, short)
	if len(hv.Machines()) == probes {
		t.Errorf("a probe older than ProbeInterval was not run again")
	}
}

// TestIORegConsole reads the console session from ioreg's I/O Registry root, as captured on macOS.
func TestIORegConsole(t *testing.T) {
	for file, want := range map[string]vz.Console{
		"testdata/ioreg-unlocked.plist":     {User: "nahyun"},
		"testdata/ioreg-locked.plist":       {User: "nahyun", Locked: true},
		"testdata/ioreg-rootlocked.plist":   {User: "nahyun", Locked: true},
		"testdata/ioreg-loginwindow.plist":  {Locked: true},
		"testdata/ioreg-fastswitched.plist": {User: "second"},
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		s := vz.IORegSession{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "/usr/sbin/ioreg" || strings.Join(args, " ") != "-n Root -d1 -a" {
				t.Errorf("ran %s %v", name, args)
			}
			return b, nil
		}}
		got, err := s.Console(ctxT(t, time.Second))
		if err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", file, got, err, want)
		}
	}
	for name, out := range map[string]string{
		"not a plist": "IOConsoleUsers = ()",
		"no fields":   `<?xml version="1.0"?><plist version="1.0"><dict><key>IOKitBuildVersion</key><string>x</string></dict></plist>`,
	} {
		if c, err := vz.ParseIORegRoot([]byte(out)); err == nil {
			t.Errorf("%s: parsed as %+v, want an error (unknown is not unlocked)", name, c)
		}
	}
}
