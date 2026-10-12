package kettlingar

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// DefaultDataDir returns where the service called name keeps its data by
// default: $XDG_DATA_HOME/name, or ~/.local/share/name, on Unix;
// ~/Library/Application Support/name on macOS; %LocalAppData%\name on
// Windows. Tools that read a service's data without it running can call
// this with the service's name to find it.
func DefaultDataDir(name string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LocalAppData"); dir != "" {
			return filepath.Join(dir, name), nil
		}
		return "", errors.New("kettlingar: %LocalAppData% is not set")
	case "darwin", "ios":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", name), nil
	}
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", name), nil
}

// DataDir returns the service's default data directory, as DefaultDataDir
// gives it for the service's name.
func (ks *KettlingarService) DataDir() (string, error) {
	return DefaultDataDir(ks.Name)
}
