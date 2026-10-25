//go:build windows

// Command kuwoprobe samples the running Kuwo playback core using read-only
// process rights. It never starts, restarts or controls any player. -cover also
// fetches public artwork for the observed RID; no account data is used.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/player/kuwo/cover"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"

	"github.com/shirou/gopsutil/v3/process"
)

func main() {
	pid := flag.Int("pid", 0, "kwmusic.exe PID; required when multiple instances exist")
	duration := flag.Duration("duration", 10*time.Second, "sample duration (max 5m)")
	art := flag.Bool("cover", false, "fetch and validate cover for the initial RID")
	flag.Parse()
	if err := run(*pid, *duration, *art); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(pid int, duration time.Duration, art bool) error {
	if duration <= 0 || duration > 5*time.Minute {
		return fmt.Errorf("duration must be in (0,5m]")
	}
	if pid == 0 {
		all, err := process.Processes()
		if err != nil {
			return err
		}
		for _, p := range all {
			name, err := p.Name()
			if err == nil && strings.EqualFold(name, "kwmusic.exe") {
				if pid != 0 {
					return fmt.Errorf("multiple Kuwo processes; specify -pid")
				}
				pid = int(p.Pid)
			}
		}
	}
	if pid == 0 {
		return fmt.Errorf("kwmusic.exe not found")
	}
	r, err := pos.OpenCore(pid)
	if err != nil {
		return err
	}
	defer r.Close()
	first, err := r.Read("")
	if err != nil {
		return err
	}
	fmt.Printf("pid=%d rid=%s duration_ms=%d position_ms=%d\n", pid, first.RID, first.DurationMs, first.PositionMs)
	if art {
		a, err := cover.Fetch(context.Background(), first.RID, 10*time.Second)
		if err != nil {
			return err
		}
		current, err := r.Read(first.RID)
		if err != nil {
			return err
		}
		fmt.Printf("cover_rid=%s url=%s data_uri_bytes=%d\n", current.RID, a.URL, len(a.DataURI))
	}
	var clock pos.CoreClock
	start := time.Now()
	lastLog := time.Time{}
	changes := 0
	previous := first.PositionMs
	errors := 0
	for time.Since(start) < duration {
		s, err := r.Read("")
		if err != nil {
			errors++
			if errors <= 3 {
				fmt.Printf("sample skipped: %v\n", err)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		seek := clock.Observe(s)
		if s.PositionMs != previous {
			changes++
		}
		previous = s.PositionMs
		if seek || time.Since(lastLog) >= time.Second {
			fmt.Printf("t=%.3f rid=%s position_ms=%d duration_ms=%d advancing=%v seek=%v\n", time.Since(start).Seconds(), s.RID, s.PositionMs, s.DurationMs, clock.Advancing(s.ObservedAt), seek)
			lastLog = time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("changes=%d skipped=%d\n", changes, errors)
	return nil
}
