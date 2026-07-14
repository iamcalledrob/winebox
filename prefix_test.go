package winebox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertUnixPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "absolute path",
			input: "/foo/bar/baz.exe",
			want:  `Z:\foo\bar\baz.exe`,
		},
		{
			name:  "nested path",
			input: "/a/b/c/d",
			want:  `Z:\a\b\c\d`,
		},
		{
			name:  "root",
			input: "/",
			want:  `Z:\`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConvertUnixPath(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("relative path is made absolute", func(t *testing.T) {
		got, err := ConvertUnixPath("some/relative/path")
		require.NoError(t, err)
		assert.True(t, len(got) > 3, "result should be a non-trivial path")
		assert.True(t, got[:2] == `Z:`, "result should start with Z:")
		// Must be longer than a bare drive letter — i.e. abs path was prepended.
		assert.Greater(t, len(got), len(`Z:\some\relative\path`)-1)
	})
}

func TestReplaceUnixPathArgs(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "passthrough non-win-path args",
			input: []string{"--verbose", "hello"},
			want:  []string{"--verbose", "hello"},
		},
		{
			name:  "converts win-path prefix",
			input: []string{"(win-path)/foo/bar/baz.exe"},
			want:  []string{`Z:\foo\bar\baz.exe`},
		},
		{
			name:  "strips surrounding single quotes",
			input: []string{"(win-path)'/foo/bar/baz.exe'"},
			want:  []string{`Z:\foo\bar\baz.exe`},
		},
		{
			name:  "strips surrounding double quotes",
			input: []string{`(win-path)"/foo/bar/baz.exe"`},
			want:  []string{`Z:\foo\bar\baz.exe`},
		},
		{
			name:  "bare win-path marker is dropped",
			input: []string{"(win-path)"},
			want:  nil,
		},
		{
			name:  "whitespace-only win-path is dropped",
			input: []string{"(win-path)   "},
			want:  nil,
		},
		{
			name:  "mixed args preserve order",
			input: []string{"--flag", "(win-path)/a/b", "--other"},
			want:  []string{"--flag", `Z:\a\b`, "--other"},
		},
		{
			name:  "empty input",
			input: []string{},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := replaceUnixPathArgs(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConvertWindowsPath(t *testing.T) {
	// Build a minimal fake wine prefix:
	//   <prefix>/dosdevices/c: -> <prefix>/drive_c
	//   <prefix>/drive_c/Windows/notepad.exe
	prefix := t.TempDir()
	dosdevices := filepath.Join(prefix, "dosdevices")
	driveC := filepath.Join(prefix, "drive_c")
	windowsDir := filepath.Join(driveC, "Windows")

	require.NoError(t, os.MkdirAll(dosdevices, 0755))
	require.NoError(t, os.MkdirAll(windowsDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(windowsDir, "notepad.exe"), nil, 0644))
	require.NoError(t, os.Symlink(driveC, filepath.Join(dosdevices, "c:")))

	tests := []struct {
		name    string
		winPath string
		want    string
	}{
		{
			name:    "basic path",
			winPath: `C:\Windows\notepad.exe`,
			want:    filepath.Join(driveC, "Windows", "notepad.exe"),
		},
		{
			name:    "case insensitive directory",
			winPath: `C:\windows\notepad.exe`,
			want:    filepath.Join(driveC, "Windows", "notepad.exe"),
		},
		{
			name:    "case insensitive filename",
			winPath: `C:\Windows\NOTEPAD.EXE`,
			want:    filepath.Join(driveC, "Windows", "notepad.exe"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConvertWindowsPath(prefix, tt.winPath)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("nonexistent path returns ErrNotExist", func(t *testing.T) {
		_, err := ConvertWindowsPath(prefix, `C:\Windows\missing.exe`)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})
}
