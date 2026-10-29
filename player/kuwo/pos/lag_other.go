//go:build !windows

package pos

type LagReader struct{}

func OpenLag(mainPID int) (*LagReader, error) { return nil, ErrNotSupported }
func (r *LagReader) Read() (LagSample, OutputState, error) {
	return LagSample{}, OutputState{}, ErrNotSupported
}
func (r *LagReader) Build() string            { return "" }
func (r *LagReader) Close()                   {}
