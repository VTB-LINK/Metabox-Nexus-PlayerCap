package kuwo

import (
	"context"
	"errors"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
)

type snapshotFunc func(context.Context) (cdp.Snapshot, error)

func (f snapshotFunc) Snapshot(ctx context.Context) (cdp.Snapshot, error) { return f(ctx) }

func TestSessionFailureClearsState(t *testing.T) {
	for _, tt := range []struct {
		name string
		snap cdp.Snapshot
		err  error
	}{
		{"disconnected", cdp.Snapshot{}, errors.New("closed")},
		{"unknown_status", cdp.Snapshot{RID: "MUSIC_1", Status: 99}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := New(0, 50)
			err := p.runSession(context.Background(), snapshotFunc(func(context.Context) (cdp.Snapshot, error) { return tt.snap, tt.err }), func() (positionSource, error) { t.Fatal("unexpected core open"); return nil, nil })
			if err == nil {
				t.Fatal("failure swallowed")
			}
			e := drainEvents(p)
			if len(e) != 2 || e[0].Type != player.EventClearSongData || e[1].Data.(*player.StatusInfo).Status != "waiting_process" {
				t.Fatalf("stale state: %+v", e)
			}
		})
	}
}

func TestSessionCancellationUnblocksSnapshot(t *testing.T) {
	p := New(0, 50)
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- p.runSession(ctx, snapshotFunc(func(ctx context.Context) (cdp.Snapshot, error) {
			close(entered)
			<-ctx.Done()
			return cdp.Snapshot{}, ctx.Err()
		}), nil)
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("session leaked on cancel")
	}
}

func TestEffectivePollFloorAndIdempotentStop(t *testing.T) {
	for _, ms := range []int{-1, 0, 30, 50} {
		if EffectivePollInterval(ms) != 50*time.Millisecond {
			t.Fatal(ms)
		}
	}
	if EffectivePollInterval(250) != 250*time.Millisecond {
		t.Fatal("configured interval lost")
	}
	p := New(0, 50)
	p.Stop()
	p.Stop()
}
