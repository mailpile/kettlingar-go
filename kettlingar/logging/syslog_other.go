//go:build windows || plan9

package logging

import (
	"fmt"
	"io"
	"log/slog"
)

// newSyslogHandler is unsupported on platforms without log/syslog; BaseHandler's
// caller falls back to another target with a warning.
func newSyslogHandler(cfg Config) (slog.Handler, io.Closer, error) {
	return nil, nil, fmt.Errorf("logging: syslog is not supported on this platform")
}
