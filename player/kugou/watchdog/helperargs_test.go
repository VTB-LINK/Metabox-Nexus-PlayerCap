package watchdog

// 本文件只测提权 helper 的命令行与令牌：主进程怎样拼（helperParams、newHelperToken），helper 怎样解析
// （parseHelperArgs、isHelperToken），两者经 Windows 命令行切分之后对得上，以及令牌怎样落到信号文件名上
// （signalFiles）。helper 何时退出、何时写 DLL 在 helperexit_test.go。

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const testToken = "0123456789abcdef"

// TestParseHelperArgs 断言只接受「路径 + 合法 PID + 合法令牌」恰好三项，其余一律拒绝；个数与令牌合格、
// 错在路径或 PID 时仍返回令牌（helper 据此把原因写进本次的结果文件），个数或令牌不合格时令牌为空。
//
// 变异自证（隔离副本，实测）：去掉 `uint32(pid) == selfPID` → 「PID 是 helper 自己」一例红；去掉 `pid == 0`
// → 「PID 为 0」「PID 为 00」两例红；`len(args) != 3` 改成 `len(args) < 3` → 「多一项」一例红；去掉路径的
// TrimSpace → 「路径全是空白」一例红；去掉令牌校验 → 令牌各例红；PID 出错时不返回令牌 → PID 各例与「参数顺序颠倒」红。
func TestParseHelperArgs(t *testing.T) {
	const self = 4242
	const path = `C:\Program Files\KuGou\KGMusic\20.1.22.27795\libcef.dll`

	okCases := []struct {
		name string
		args []string
		pid  uint32
	}{
		{"常规", []string{path, "1234", testToken}, 1234},
		{"前导零", []string{path, "01234", testToken}, 1234},
		{"uint32 上限", []string{path, "4294967295", testToken}, 4294967295},
		{"与自身 PID 只差 1", []string{path, "4243", testToken}, 4243},
		{"令牌全为数字", []string{path, "1234", "0000000000000000"}, 1234},
		{"令牌全为字母", []string{path, "1234", "ffffffffffffffff"}, 1234},
	}
	for _, c := range okCases {
		t.Run("接受/"+c.name, func(t *testing.T) {
			gotPath, gotPID, gotToken, err := parseHelperArgs(c.args, self)
			if err != nil || gotPath != c.args[0] || gotPID != c.pid || gotToken != c.args[2] {
				t.Fatalf("parseHelperArgs(%q) = (%q, %d, %q, %v)，期望 (%q, %d, %q, nil)", c.args, gotPath, gotPID, gotToken, err, c.args[0], c.pid, c.args[2])
			}
		})
	}

	badCases := []struct {
		name      string
		args      []string
		keepToken bool // 个数与令牌合格、错在路径或 PID：报错的同时仍返回令牌
	}{
		{"无参数", nil, false},
		{"只有路径", []string{path}, false},
		{"缺令牌（上一版格式）", []string{path, "1234"}, false},
		{"多一项", []string{path, "1234", testToken, "extra"}, false},
		{"路径为空", []string{"", "1234", testToken}, true},
		{"路径全是空白", []string{"   ", "1234", testToken}, true},
		{"PID 为空", []string{path, "", testToken}, true},
		{"PID 非数字", []string{path, "abc", testToken}, true},
		{"PID 数字后有字母", []string{path, "12a", testToken}, true},
		{"PID 带负号", []string{path, "-5", testToken}, true},
		{"PID 带正号", []string{path, "+5", testToken}, true},
		{"PID 带空白", []string{path, " 5", testToken}, true},
		{"PID 十六进制", []string{path, "0x10", testToken}, true},
		{"PID 小数", []string{path, "12.0", testToken}, true},
		{"PID 超出 uint32", []string{path, "4294967296", testToken}, true},
		{"PID 为 0", []string{path, "0", testToken}, true},
		{"PID 为 00", []string{path, "00", testToken}, true},
		{"PID 是 helper 自己", []string{path, "4242", testToken}, true},
		{"令牌为空", []string{path, "1234", ""}, false},
		{"令牌短一位", []string{path, "1234", "0123456789abcde"}, false},
		{"令牌长一位", []string{path, "1234", "0123456789abcdef0"}, false},
		{"令牌含大写", []string{path, "1234", "0123456789ABCDEF"}, false},
		{"令牌含非十六进制字母", []string{path, "1234", "0123456789abcdeg"}, false},
		{`令牌含 ..\`, []string{path, "1234", `..\..\..\Windows`}, false},
		{"令牌含 ../", []string{path, "1234", "../../../Windows"}, false},
		{"令牌含反斜杠", []string{path, "1234", `0123456\89abcdef`}, false},
		{"令牌含斜杠", []string{path, "1234", "0123456/89abcdef"}, false},
		{"令牌含冒号", []string{path, "1234", "C:23456789abcdef"}, false},
		{"令牌含空格", []string{path, "1234", "0123456 89abcdef"}, false},
		{"令牌含 NUL", []string{path, "1234", "0123456\x0089abcdef"}, false},
		{"令牌含全角数字（恰为 16 字节）", []string{path, "1234", "０１２３４0"}, false}, // 5 个全角字符各 3 字节，加 1 个半角
		{"令牌与 PID 对调", []string{path, testToken, "1234"}, false},
		{"参数顺序颠倒", []string{"1234", path, testToken}, true},
	}
	for _, c := range badCases {
		t.Run("拒绝/"+c.name, func(t *testing.T) {
			gotPath, gotPID, gotToken, err := parseHelperArgs(c.args, self)
			if err == nil {
				t.Fatalf("parseHelperArgs(%q) = (%q, %d, %q, nil)，期望报错", c.args, gotPath, gotPID, gotToken)
			}
			wantToken := ""
			if c.keepToken {
				wantToken = c.args[2]
			}
			if gotPath != "" || gotPID != 0 || gotToken != wantToken {
				t.Errorf("报错时应返回 (\"\", 0, %q)，实得 (%q, %d, %q)", wantToken, gotPath, gotPID, gotToken)
			}
		})
	}
}

// TestIsHelperTokenBoundaries 逐字节断言令牌的字符集边界：'0'..'9'、'a'..'f' 之内的接受，紧邻其外的
// （'/'、':'、'`'、'g'、'A'、'F'）拒绝。
//
// 变异自证（隔离副本，实测）：字母上界 'f' 改成 'g'、数字上界 '9' 改成 ':'，均红。
func TestIsHelperTokenBoundaries(t *testing.T) {
	for c := 0; c < 256; c++ {
		s := strings.Repeat("0", helperTokenLen-1) + string([]byte{byte(c)})
		want := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if got := isHelperToken(s); got != want {
			t.Errorf("末字节 %#02x（%q）：isHelperToken = %v，期望 %v", c, c, got, want)
		}
	}
}

// TestNewHelperToken 断言生成的令牌都合 isHelperToken 的格式，且一批里互不相同。
//
// 变异自证（隔离副本，实测）：随机字节改为固定的全零 → 「互不相同」红；字节数改为 helperTokenLen → 长度不合格，红。
func TestNewHelperToken(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		tok := newHelperToken()
		if !isHelperToken(tok) {
			t.Fatalf("newHelperToken() = %q，不合 isHelperToken 的格式", tok)
		}
		if seen[tok] {
			t.Fatalf("1000 个令牌里出现重复：%q", tok)
		}
		seen[tok] = true
	}
}

// TestSignalFilesPerToken 断言信号文件按令牌命名、都在给定目录下：不同令牌的三个文件两两不同，同一令牌的
// 三个文件互不相同。
//
// 变异自证（隔离副本，实测）：文件名不带令牌 → 红。
func TestSignalFilesPerToken(t *testing.T) {
	dir := t.TempDir()
	a1, a2, a3 := signalFiles(dir, testToken)
	b1, b2, b3 := signalFiles(dir, "fedcba9876543210")
	all := []string{a1, a2, a3, b1, b2, b3}
	seen := map[string]bool{}
	for i, f := range all {
		if filepath.Dir(f) != dir {
			t.Errorf("%s 不在 %s 下", f, dir)
		}
		tok := testToken
		if i >= 3 {
			tok = "fedcba9876543210"
		}
		if !strings.Contains(filepath.Base(f), tok) {
			t.Errorf("%s 的文件名不含令牌 %s", f, tok)
		}
		if seen[f] {
			t.Errorf("信号文件重名：%s", f)
		}
		seen[f] = true
	}
}

// TestHelperParamsRoundTrip 断言主进程拼出的参数经 Windows 的命令行切分（CommandLineToArgvW，与 helper
// 进程里 os.Args 的切分规则相同）后，helper 解析出的正是原路径、原 PID 与原令牌：路径含空格、括号、
// 非 ASCII 字符时也是。
//
// 变异自证（隔离副本，实测）：helperParams 去掉路径两侧的引号、不拼令牌，均红。
func TestHelperParamsRoundTrip(t *testing.T) {
	const self = 7
	for _, c := range []struct {
		path string
		pid  uint32
	}{
		{`C:\Program Files\KuGou\KGMusic\20.1.22.27795\libcef.dll`, 1234},
		{`C:\Program Files (x86)\KuGou\KGMusic\20.1.22.27795\libcef.dll`, 8},
		{`D:\酷狗 音乐\KGMusic\20.1.22.27795\libcef.dll`, 4294967295},
		{`C:\KuGou\libcef.dll`, 99999},
	} {
		token := newHelperToken()
		params := helperParams(c.path, c.pid, token)
		argv, err := windows.DecomposeCommandLine(`"C:\x\Metabox-Nexus-PlayerCap.exe" ` + params)
		if err != nil {
			t.Fatalf("DecomposeCommandLine(%q): %v", params, err)
		}
		if len(argv) < 2 || argv[1] != "--kugou-patch-helper" {
			t.Fatalf("参数 %q 切分为 %q，第二项应为 --kugou-patch-helper", params, argv)
		}
		gotPath, gotPID, gotToken, err := parseHelperArgs(argv[2:], self)
		if err != nil || gotPath != c.path || gotPID != c.pid || gotToken != token {
			t.Errorf("参数 %q 切分为 %q，解析得 (%q, %d, %q, %v)，期望 (%q, %d, %q, nil)", params, argv, gotPath, gotPID, gotToken, err, c.path, c.pid, token)
		}
	}
}
