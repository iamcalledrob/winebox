package winebox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PidRefCounter creates references between pids and a given resource (path), allowing
// for tracking concurrent "users" of a resource and auto-releasing on process death.
type PidRefCounter struct {
	path string
}

func NewPidRefCounter(path string) *PidRefCounter {
	return &PidRefCounter{path: path}
}

func (c *PidRefCounter) Retain() error {
	err := os.MkdirAll(c.path, 0755)
	if err != nil {
		return fmt.Errorf("creating path: %w", err)
	}

	err = createEmptyFile(filepath.Join(c.path, strconv.Itoa(os.Getpid())+".pid"), 0644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("process already retained reference")
	}
	if err != nil {
		return fmt.Errorf("writing pid file: %w", err)
	}

	return nil
}

func (c *PidRefCounter) Release() error {
	return os.RemoveAll(filepath.Join(c.path, strconv.Itoa(os.Getpid())+".pid"))
}

// ReferenceCount returns the number of living processes with retained references
// Racey: protect Retain/Release/ReferenceCount with an inter-process lock if necessary.
func (c *PidRefCounter) ReferenceCount() (int, error) {
	entries, err := os.ReadDir(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading pids dir: %w", err)
	}

	// Filter to only .pid files in case of .DS_Store-style mangling etc...
	pids := make([]int, 0)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}

		pid, _ := strconv.Atoi(strings.TrimSuffix(e.Name(), ".pid"))
		if !processAlive(pid) {
			continue
		}

		pids = append(pids, pid)
	}
	return len(pids), nil
}
