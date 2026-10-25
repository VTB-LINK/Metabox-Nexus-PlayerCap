//go:build !windows

package watchdog

import "context"

func scanProcesses(context.Context) (instance, error) { return instance{}, ErrNotRunning }
func stopExact(context.Context, instance) error       { return ErrVersionUnsupported }
func startExact(string, []string) error               { return ErrVersionUnsupported }
