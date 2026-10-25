//go:build !windows

package pos

type CoreReader struct{}

func OpenCore(mainPID int) (*CoreReader, error)               { return nil, ErrNotSupported }
func (r *CoreReader) Read(expectedRID string) (Sample, error) { return Sample{}, ErrNotSupported }
func (r *CoreReader) Build() (timestamp uint32, slot int)     { return 0, 0 }
func (r *CoreReader) Close()                                  {}
