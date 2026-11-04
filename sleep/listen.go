// Package sleep is a shim for pausing and resuming processes before and after
// system sleep via SIGSTOP and SIGCONT.
//
// Works around a kernel limitation: before sleep, systemd and the kernel freeze all
// user processes in no particular order (including FUSE daemons). A process caught
// mid-request on a FUSE mount then waits uninterruptibly on a frozen daemon.
// This results in a process that can't be frozen (blocked waiting on frozen FUSE),
// and the system suspend fails: "tasks refusing to freeze".
//
// Manually pausing processes before the normal suspend lifecycle runs ensures
// there can be no in-flight requests when the system actually suspends.
//
// https://ratatoskr.run/fuse-devel/2026/08/17431737/t
package sleep

import (
	"fmt"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
)

// InhibitLabel populates the human-readable labels returned by `systemd-inhibit --list`
type InhibitLabel struct {
	Who string
	Why string
}

// Listen listens for system sleep events and invokes beforeSleep and afterWake callbacks.
// It receives these events by registering a systemd sleep inhibitor. The goal is not to really
// inhibit sleep indefinitely, but to instead inhibit sleep just while the callbacks run.
func Listen(label InhibitLabel, beforeSleep func(), afterWake func()) (stop func(), err error) {
	var conn *dbus.Conn
	conn, err = dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connecting to system bus: %w", err)
	}

	// Subscribe before inhibiting so a PrepareForSleep can't slip between the two.
	err = conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.login1.Manager"),
		dbus.WithMatchMember("PrepareForSleep"))
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("subscribing to PrepareForSleep: %w", err)
	}
	signals := make(chan *dbus.Signal, 4)
	conn.Signal(signals)

	var inhibitor *os.File
	inhibitor, err = inhibit(conn, label)
	if err != nil {
		_ = conn.Close()
		return
	}

	// Held while callbacks run, so stop() can't race with them.
	var mu sync.Mutex
	var stopped bool

	go func() {
		for sig := range signals {
			if sig.Name != "org.freedesktop.login1.Manager.PrepareForSleep" || len(sig.Body) != 1 {
				continue
			}

			mu.Lock()
			goingToSleep, _ := sig.Body[0].(bool)
			if stopped {
				mu.Unlock()
				return
			}
			if goingToSleep {
				beforeSleep()
				if inhibitor != nil {
					// let sleep proceed
					_ = inhibitor.Close()
					inhibitor = nil
				}
			} else {
				afterWake()
				if inhibitor == nil {
					// re-arm for next sleep
					inhibitor, _ = inhibit(conn, label)
				}
			}
			mu.Unlock()
		}
	}()

	return func() {
		mu.Lock()
		defer mu.Unlock()
		stopped = true
		if inhibitor != nil {
			_ = inhibitor.Close()
			inhibitor = nil
		}

		// ends the signals goroutine
		_ = conn.Close()
	}, nil
}

// inhibit asks logind (systemd's session manager) to delay sleep. It returns a file
// that holds the delay: sleep waits until the file is closed, or for at most
// InhibitDelayMaxSec (default 5s).
func inhibit(conn *dbus.Conn, label InhibitLabel) (*os.File, error) {
	var fd dbus.UnixFD
	err := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1").
		Call("org.freedesktop.login1.Manager.Inhibit", 0, "sleep", label.Who, label.Why, "delay").
		Store(&fd)
	if err != nil {
		return nil, fmt.Errorf("taking sleep inhibitor: %w", err)
	}
	return os.NewFile(uintptr(fd), "logind-inhibit"), nil
}
