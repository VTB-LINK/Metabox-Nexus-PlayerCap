//go:build windows

package session

import (
	"testing"

	"golang.org/x/sys/windows"
)

// 本文件只测一件事：session_windows.go 里手排字段的 GUID 与规范字符串一致。写错一个字节，Activate 或
// QueryInterface 会每拍失败，信号恒为未知而不报任何编译错误。只做字符串格式化，不调用任何 COM。
//
// 变异自证（实测）：iidIAudioSessionControl2 的 Data4 末字节 0x6D 改为 0x6E → 红。
func TestGUIDs(t *testing.T) {
	for _, c := range []struct {
		name string
		got  windows.GUID
		want string
	}{
		{"CLSID_MMDeviceEnumerator", clsidMMDeviceEnumerator, "{BCDE0395-E52F-467C-8E3D-C4579291692E}"},
		{"IID_IMMDeviceEnumerator", iidIMMDeviceEnumerator, "{A95664D2-9614-4F35-A746-DE8DB63617E6}"},
		{"IID_IAudioSessionManager2", iidIAudioSessionManager2, "{77AA99A0-1BD6-484F-8BC7-2C654C9A9B6F}"},
		{"IID_IAudioSessionControl2", iidIAudioSessionControl2, "{BFB7FF88-7239-4FC9-8FA2-07C950BE9C6D}"},
	} {
		if s := c.got.String(); s != c.want {
			t.Errorf("%s = %s, 期望 %s", c.name, s, c.want)
		}
	}
}
