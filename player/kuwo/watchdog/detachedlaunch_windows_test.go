package watchdog

// Check the restart adapter without starting or terminating any player.
import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStartExactUsesDetachedArgumentsAndErrors(t *testing.T) {
	original := detachFn
	t.Cleanup(func() { detachFn = original })
	exe := `C:\Program Files\Kuwo\kwmusic.exe`
	args := []string{"--remote-debugging-port=9224", "argument with spaces"}
	wantErr := errors.New("launch failed")
	for _, tc := range []struct {
		detached bool
		err      error
	}{{true, nil}, {false, nil}, {false, wantErr}} {
		calls := 0
		detachFn = func(gotExe string, gotArgs []string, dir string) (bool, error) {
			calls++
			if gotExe != exe || !reflect.DeepEqual(gotArgs, args) || dir != filepath.Dir(exe) {
				t.Fatalf("launch parameters changed: %q %q %q", gotExe, gotArgs, dir)
			}
			return tc.detached, tc.err
		}
		if err := startExact(exe, args); !errors.Is(err, tc.err) || calls != 1 {
			t.Fatalf("calls=%d err=%v want=%v", calls, err, tc.err)
		}
	}
}
