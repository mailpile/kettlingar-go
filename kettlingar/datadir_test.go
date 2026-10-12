package kettlingar

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultDataDir(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		t.Skip("XDG layout only")
	}
	t.Setenv("XDG_DATA_HOME", "/data")
	if got, err := DefaultDataDir("svc"); err != nil || got != filepath.FromSlash("/data/svc") {
		t.Errorf("with XDG_DATA_HOME: %q, %v", got, err)
	}
	t.Setenv("XDG_DATA_HOME", "relative")
	t.Setenv("HOME", "/home/u")
	if got, err := DefaultDataDir("svc"); err != nil || got != "/home/u/.local/share/svc" {
		t.Errorf("without: %q, %v", got, err)
	}
}
