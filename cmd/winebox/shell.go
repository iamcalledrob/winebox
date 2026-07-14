package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/creack/pty"
	"github.com/muesli/cancelreader"
	"golang.org/x/term"
	"robmason.co.uk/winebox"
)

// Spawns a child shell for invoking commands in the wine prefix
func enterWinePrefixShell(p *winebox.WinePrefix, passthruEnv []string) error {
	// Use BaseEnv to get a 1:1 environment to when `winebox run` is used.
	// Simplifies debugging and avoids "only works in `winebox shell`" type stuff.
	//
	// Prepend the specified wine runner dir to the PATH so other 'wine's
	// in the PATH aren't used.
	env := winebox.BuildEnv(winebox.DefaultPassthruEnvKeys)

	// For basic terminal UX
	env = append(env, winebox.BuildEnv([]string{
		"LOGNAME", "SHELL",
		"TERM", "COLORTERM",
		"LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "LC_TIME",
		"XDG_SESSION_TYPE",
	})...)
	env = append(env, winebox.BuildEnv(passthruEnv)...)
	env = append(env, "WINEPREFIX="+p.PrefixPath)

	if dir := filepath.Dir(p.WinePath); dir != "." {
		env = prependPATH(dir, env)
	}

	dir := filepath.Join(p.PrefixPath, "drive_c")
	return spawnChildShell(dir, env)
}

// spawns a child shell at 'dir' using *only* specified env
func spawnChildShell(dir string, env []string) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return fmt.Errorf("$SHELL is not set")
	}

	cmd := exec.Command(shell, shellNoRCArgs(shell)...)
	cmd.Dir = dir
	cmd.Env = env

	// Ensure death of parent process aborts child shell, which wouldn't
	// happen by default since child shell is in a new process
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}

	// Start the shell under a PTY so it behaves as a real interactive terminal
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}
	defer func() { _ = ptmx.Close() }()

	// Forward terminal resize events
	sigWinch := make(chan os.Signal, 1)
	signal.Notify(sigWinch, syscall.SIGWINCH)
	go func() {
		for range sigWinch {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}()
	defer func() {
		signal.Stop(sigWinch)
		close(sigWinch)
	}()

	// Put the parent terminal in raw mode so keystrokes pass through
	var prevState *term.State
	prevState, err = term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("make raw: %w", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), prevState) }()

	// Set initial size to match parent
	err = pty.InheritSize(os.Stdin, ptmx)
	if err != nil {
		return fmt.Errorf("inherit pty size: %w", err)
	}

	// Reads must be cancellable so that this function can cleanly exit and
	// subsequent logic can use os.Stdin as expected.
	var cr cancelreader.CancelReader
	cr, err = cancelreader.NewReader(os.Stdin)
	if err != nil {
		return fmt.Errorf("making cancel reader: %w", err)
	}
	defer func() { _ = cr.Cancel() }()

	// Forward io from tty to Stdin/Stdout
	go func() { _, _ = io.Copy(ptmx, cr) }()
	go func() { _, _ = io.Copy(os.Stdout, ptmx) }()

	// Block until child shell exits.
	err = cmd.Wait()
	// Ignore errors of type *exec.ExitError
	if _, isExitErr := errors.AsType[*exec.ExitError](err); !isExitErr {
		return err
	}

	return nil
}

// shellNoRCArgs returns flags that suppress rc file sourcing for common shells
func shellNoRCArgs(shell string) []string {
	switch filepath.Base(shell) {
	case "bash", "sh":
		return []string{"--norc", "--noprofile"}
	case "zsh":
		return []string{"--no-rcs"}
	case "fish":
		return []string{"--no-config"}
	default:
		return nil
	}
}

func prependPATH(s string, env []string) []string {
	var found bool
	for i, e := range env {
		if len(e) > 5 && e[:5] == "PATH=" {
			env[i] = "PATH=" + s + ":" + e[5:]
			found = true
			break
		}
	}

	if !found {
		env = append(env, "PATH="+s)
	}
	return env
}
