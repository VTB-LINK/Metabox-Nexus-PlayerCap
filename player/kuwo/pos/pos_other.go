//go:build !windows

// 非 Windows 下的桩：本包靠 ReadProcessMemory，只有 Windows 实现。
// 保留同一套 API 是为了让整个仓库在任何平台都能 `go build ./...`（CI 的 vet/构建面）。

package pos

import "errors"

// ErrNotSupported 表示当前平台没有实现。
var ErrNotSupported = errors.New("kuwo pos: only supported on windows")

// Reader 在非 Windows 下恒为不可用。
type Reader struct{}

// Open 恒返回 ErrNotSupported。
func Open(pid int, rid string, durationMs int32) (*Reader, error) {
	return nil, ErrNotSupported
}

func OpenSnapshot(pid int, rid string, durationMs int32) (*Reader, error) {
	return nil, ErrNotSupported
}

// Song 返回空结构。
func (r *Reader) Song() Song { return Song{} }

// Position 恒返回错误。
func (r *Reader) Position() (int32, error) { return 0, ErrNotSupported }

// Close 是空操作。
func (r *Reader) Close() {}
