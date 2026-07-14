package winebox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// AppRuntimeDir is dir the app uses for mounts points.
// Slightly hacky approach, can improve later.
var AppRuntimeDir = filepath.Join(runtimeDir(), "winebox")

func BuildEnv(keys []string) []string {
	var env []string
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// ContextWithCancelSignal self-cancels when one of the specified signals is received.
func ContextWithCancelSignal(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)

	// Intercept signals so that we can gracefully shut down (kill child, unmount)
	// rather than just terminating in a mounted state.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, signals...)

	go func() {
		for sig := range sigCh {
			cancel(fmt.Errorf("signal received: %v", sig))
		}
		cancel(context.Canceled)
	}()

	return ctx, func() {
		signal.Stop(sigCh)
		close(sigCh)
	}
}

func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return fmt.Sprintf("/run/user/%d", os.Getuid())
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func canonicalPath(path string) (string, error) {
	var err error
	path, err = evalExistingSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("evaluating symlinks: %w", err)
	}

	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("getting absolute path: %w", err)
	}

	return path, nil
}

// evalExistingSymlinks is like filepath.EvalSymlinks but only evaluates the path up
// to the point that it exists. Makes it possible to evaluate symlinks on a *proposed*
// path, e.g. a dir to be created, without erroring out.
func evalExistingSymlinks(path string) (string, error) {
	tail := ""
	p := path
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = filepath.Join(filepath.Base(p), tail)
		p = parent
	}
}

func createEmptyFile(path string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, mode)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("close: %w", err)
	}

	return nil
}

func writeTemp(dir string, pattern string, s string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	defer func() { _ = f.Close() }()

	_, err = f.WriteString(s)
	if err != nil {
		return "", fmt.Errorf("write string: %w", err)
	}

	return f.Name(), nil
}
