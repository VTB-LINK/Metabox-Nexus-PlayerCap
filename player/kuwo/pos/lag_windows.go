//go:build windows

package pos

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// LagReader reads the seek-lag fields of in_ffaudio.dll and out_kw_ds.dll in
// the KwService child of the selected kwmusic process, read-only. It is owned
// by one polling goroutine; Close must not race Read.
type LagReader struct {
	h       windows.Handle
	in, out uintptr // in_ffaudio.dll and out_kw_ds.dll module bases
	build   string
}

// lagModule is one plugin module as found at open time.
type lagModule struct {
	name  string
	base  uintptr
	size  uint32 // ModBaseSize
	file  []byte
	hash  string
	stamp uint32 // file TimeDateStamp, logging only
}

// OpenLag admits the build by the executable-section fingerprints of both
// plugin files and checks each mapped image against its file (design §3.1).
// It reads each file once (at most lagFileLimit bytes) and lagHeaderSize
// bytes of each image header. A system call failure returns that error;
// every other failure wraps ErrLagUnsupported. Once both fingerprints are
// known the error text carries them and the files' TimeDateStamps.
func OpenLag(mainPID int) (*LagReader, error) {
	pid, err := servicePID(uint32(mainPID))
	if err != nil {
		return nil, lagOpenErr("KwService", err)
	}
	mods := []lagModule{{name: "in_ffaudio.dll"}, {name: "out_kw_ds.dll"}}
	for i := range mods {
		if err := mods[i].load(pid); err != nil {
			return nil, err
		}
	}
	in, out := &mods[0], &mods[1]
	fp := fmt.Sprintf("%s sha256 %s TimeDateStamp %#x, %s sha256 %s TimeDateStamp %#x",
		in.name, in.hash, in.stamp, out.name, out.hash, out.stamp)
	build, ok := matchLagBuild(in.hash, out.hash)
	if !ok {
		return nil, fmt.Errorf("%w: unknown build: %s", ErrLagUnsupported, fp)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return nil, lagOpenErr("open KwService ("+fp+")", err)
	}
	r := &LagReader{h: h, in: in.base, out: out.base, build: build}
	for _, m := range mods {
		mem, err := readFull(r.read, m.base, lagHeaderSize)
		if err == nil {
			err = checkImageHeader(mem, m.file, m.size)
		}
		if err != nil {
			r.Close()
			return nil, lagOpenErr(fmt.Sprintf("%s (%s)", m.name, fp), err)
		}
	}
	return r, nil
}

// load finds the module in pid, reads its file and computes the fingerprint.
func (m *lagModule) load(pid uint32) error {
	base, size, path, err := findModule(pid, m.name)
	if err != nil {
		return lagOpenErr(m.name, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return lagOpenErr(m.name, err)
	}
	defer f.Close()
	file, err := readLimited(f, lagFileLimit)
	if err == nil {
		m.hash, err = execSectionHash(file)
	}
	if err == nil {
		m.stamp, _, err = peStampSize(file)
	}
	if err != nil {
		return lagOpenErr(m.name+" "+path, err)
	}
	m.base, m.size, m.file = base, size, file
	return nil
}

// lagOpenErr adds context to an OpenLag failure: a system error is kept as
// it is, any other failure also wraps ErrLagUnsupported.
func lagOpenErr(what string, err error) error {
	var errno windows.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("kuwo: seek-lag: %s: %w", what, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrLagUnsupported, what, err)
}

// Read takes one sample: the #131 seek-lag fields (design §3.2) and, best
// effort, the #135 output state (design §3.1, §3.8). A seek-lag sample failing
// any check has Valid false; a failed or short read also returns its error.
// The output state read never produces an error and never changes the
// seek-lag sample: a failure there only yields OutputState{Valid:false}.
func (r *LagReader) Read() (LagSample, OutputState, error) {
	sample, err := readLag(r.read, r.in, r.out)
	return sample, readOutputState(r.read, r.out), err
}

// Build returns the admitted build name, for the connect log.
func (r *LagReader) Build() string { return r.build }

func (r *LagReader) Close() {
	if r.h != 0 {
		windows.CloseHandle(r.h)
		r.h = 0
	}
}

func (r *LagReader) read(addr uintptr, n int) ([]byte, error) { return readAt(r.h, addr, n) }
