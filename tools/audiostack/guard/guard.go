// Package guard 是 tools/audiostack 的两道安全校验，单独成包以便测试（audiostack 本身是 //go:build ignore 的
// main，无法被测试导入）。
//
// 校验的对象都是「真正交给 manager / net.Listen 的值」：先规范化、再校验，不对原始输入做校验。
package guard

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// StandardExe 是用户实例的标准文件名。manager 启动时的孤儿清扫（audiocap/manager 的 sweepOrphans）按「进程名
// 与 filepath.Base(helper 路径) 相同（EqualFold）且命令行含 --audio-capture-helper」杀进程，与路径无关；helper
// 若取这个名字，会杀掉用户实例的 helper（直播中的波形会断）。
const StandardExe = "Metabox-Nexus-PlayerCap.exe"

// CheckHelper 返回规范化后的 helper 绝对路径，即真正交给 manager 的那个值。以下情形一律拒绝：路径规范化后（Abs，
// 会去掉末尾的 \. 与 \子目录\..）或符号链接解析后（EvalSymlinks，同时要求文件存在）的文件名是标准文件名；
// 文件名以点或空格结尾（Windows 会在文件系统层剥掉它们，名字与实际文件不一致）；不是常规文件。
func CheckHelper(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("-helper 不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("规范化 -helper 失败: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("-helper 不存在或无法解析: %w", err)
	}
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("-helper 不是常规文件")
	}
	for _, p := range []string{abs, real} {
		base := filepath.Base(p)
		if strings.EqualFold(base, StandardExe) {
			return "", fmt.Errorf("-helper 的文件名是标准文件名 %s（规范化后为 %s）：manager 启动时的孤儿清扫会杀掉用户实例的 helper，须给出非标准文件名的完整应用构建", StandardExe, p)
		}
		if strings.HasSuffix(base, ".") || strings.HasSuffix(base, " ") {
			return "", fmt.Errorf("-helper 的文件名以点或空格结尾，名字与实际文件可能不一致: %q", base)
		}
	}
	return abs, nil
}

// CheckAddr 返回只含回环字面 IP 的监听地址：host 须是回环的 IP 字面量，或 localhost（改写成 127.0.0.1，不把名字
// 交给 net.Listen 再解析：hosts 或解析器配置可能让 localhost 指向非回环地址）；端口须是 1..65535 的十进制数。
func CheckAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("-addr 须是 host:port: %w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("-addr 的端口非法: %q", port)
	}
	if strings.EqualFold(host, "localhost") {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("-addr 只允许回环地址（127.0.0.1、::1、localhost），得到 %q", host)
	}
	return net.JoinHostPort(host, port), nil
}
