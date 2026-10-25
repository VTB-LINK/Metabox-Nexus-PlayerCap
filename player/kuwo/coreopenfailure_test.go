package kuwo

// Opening failures must not retain a typed-nil reader across polling ticks.
import (
	"context"
	"errors"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

func TestCoreOpenFailureReturnsErrorWithoutPanic(t *testing.T) {
	for _, typedNil := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil", true: "typed_nil"}[typedNil], func(t *testing.T) {
			p := New(0, 50)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			want := errors.New("core unavailable")
			opens := 0
			err := p.runSession(ctx, snapshotFunc(func(context.Context) (cdp.Snapshot, error) {
				return cdp.Snapshot{RID: "MUSIC_1", Status: 1}, nil
			}), func() (positionSource, error) {
				opens++
				if typedNil {
					var reader *pos.CoreReader
					return reader, want // Same interface conversion as failed OpenCore.
				}
				return nil, want
			})
			if !errors.Is(err, want) || opens < 2 {
				t.Fatalf("want bounded retry and original error; opens=%d err=%v", opens, err)
			}
			events := drainEvents(p)
			if len(events) != 2 || events[0].Type != player.EventClearSongData || events[1].Data.(*player.StatusInfo).Status != "waiting_process" {
				t.Fatalf("failure left stale state: %+v", events)
			}
		})
	}
}

type openRecoveryReader struct{ reads, closes int }

func (r *openRecoveryReader) Read(string) (pos.Sample, error) {
	r.reads++
	return pos.Sample{}, errors.New("sample unavailable")
}
func (r *openRecoveryReader) Close() { r.closes++ }

func TestCoreOpenFailureCanRecoverOrExit(t *testing.T) {
	for _, mode := range []string{"recover", "cancel", "empty_song"} {
		t.Run(mode, func(t *testing.T) {
			p := New(0, 50)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			reader := &openRecoveryReader{}
			opens, snapshots := 0, 0
			end := errors.New("snapshot ended")
			err := p.runSession(ctx, snapshotFunc(func(context.Context) (cdp.Snapshot, error) {
				snapshots++
				if snapshots >= 3 {
					return cdp.Snapshot{}, end
				}
				if mode == "empty_song" && snapshots == 2 {
					return cdp.Snapshot{Status: 3}, nil
				}
				return cdp.Snapshot{RID: "MUSIC_1", Status: 1}, nil
			}), func() (positionSource, error) {
				opens++
				if opens > 1 {
					return reader, nil
				}
				if mode == "cancel" {
					cancel()
				}
				var failed *pos.CoreReader
				return failed, errors.New("open failed")
			})
			if mode == "cancel" {
				end = context.Canceled
			}
			if !errors.Is(err, end) {
				t.Fatalf("unexpected exit: %v", err)
			}
			if mode == "recover" {
				if opens != 2 || reader.reads != 1 || reader.closes != 1 {
					t.Fatalf("recovery lifecycle: opens=%d reader=%+v", opens, reader)
				}
			} else if opens != 1 || reader.reads != 0 || reader.closes != 0 {
				t.Fatalf("failed candidate retained: opens=%d reader=%+v", opens, reader)
			}
		})
	}
}
