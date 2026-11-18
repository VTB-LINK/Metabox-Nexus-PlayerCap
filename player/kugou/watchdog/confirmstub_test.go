package watchdog

// 本文件只放 confirmWith 各用例共用的桩：一台「酷狗在运行、已打补丁、库被主进程占用」的假机器。
// 路径与 PID 取自设计 §2.5 的实测值；各用例在默认值上改一处，构造各自的反例。

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

const (
	stubMainPID  = 62016
	stubExe      = `C:\Program Files\KuGou\KGMusic\KuGou.exe`
	stubInstall  = `C:\Program Files\KuGou\KGMusic`
	stubVerDir   = `C:\Program Files\KuGou\KGMusic\20.1.22.27795`
	stubLibcef   = stubVerDir + `\libcef.dll`
	stubVersion  = "20.1.22.27795"
	stubRoaming  = `C:\Users\u\AppData\Roaming`
	stubDataDir  = `C:\Users\u\AppData\Roaming\Kugou8` // 注册表 AppDataPath 的写法
	stubDB       = stubDataDir + `\KGMusicV3.db`
	stubINI      = stubDataDir + `\KuGou.ini`
	stubLyricDir = `C:\KuGou\Lyric`
)

// pathKey 把路径规范成大小写不敏感的比较键。
func pathKey(p string) string { return strings.ToLower(filepath.Clean(p)) }

// utf16LEWithBOM 按酷狗写 KuGou.ini 的方式编码：FF FE 开头的 UTF-16LE。
func utf16LEWithBOM(s string) []byte {
	b := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

type fakeMachine struct {
	procs       []proc
	snapErr     error
	images      map[uint32]string
	imageErr    map[uint32]error
	version     map[string]string // exe 路径键 → 版本
	allPatched  bool
	canAutoFix  bool
	patchErr    error
	holders     map[string][]uint32 // 库路径键 → 占用者
	holdersErr  error
	regAppData  string
	appData     string
	files       map[string][]byte // readFile 的内容
	existing    map[string]bool   // fileExists 为真的文件
	patchCalls  []string
	holderCalls []string
	readCalls   []string
}

func newFakeMachine() *fakeMachine {
	m := &fakeMachine{
		procs: []proc{
			{PID: 4, PPID: 0, Name: "System"},
			{PID: 100, PPID: 4, Name: "explorer.exe"},
			{PID: stubMainPID, PPID: 100, Name: "KuGou.exe"},
			{PID: 7001, PPID: stubMainPID, Name: "KuGou.exe"},
			{PID: 7002, PPID: stubMainPID, Name: "KuGou.exe"},
			{PID: 7003, PPID: stubMainPID, Name: "KuGou.exe"},
			{PID: 7004, PPID: stubMainPID, Name: "KuGou.exe"},
			{PID: 8000, PPID: stubMainPID, Name: "KGService.exe"},
		},
		images: map[uint32]string{
			stubMainPID: stubExe,
			7001:        stubVerDir + `\KuGou.exe`,
			7002:        stubVerDir + `\KuGou.exe`,
			7003:        stubVerDir + `\KuGou.exe`,
			7004:        stubVerDir + `\KuGou.exe`,
		},
		imageErr:   map[uint32]error{},
		version:    map[string]string{pathKey(stubExe): stubVersion},
		allPatched: true,
		canAutoFix: true,
		holders:    map[string][]uint32{pathKey(stubDB): {5555, stubMainPID}},
		regAppData: stubDataDir,
		appData:    stubRoaming, // %APPDATA%\KuGou8 与注册表的 Kugou8 是同一目录的两种写法
		files: map[string][]byte{
			pathKey(stubINI): utf16LEWithBOM("[LyricConfigSection]\r\nLyricPath=C:\\KuGou\\Lyric\\\r\n"),
		},
		existing: map[string]bool{pathKey(stubLibcef): true, pathKey(stubDB): true, pathKey(stubINI): true},
	}
	return m
}

func (m *fakeMachine) deps() confirmDeps {
	return confirmDeps{
		snapshot: func() ([]proc, error) { return m.procs, m.snapErr },
		imagePath: func(pid uint32) (string, error) {
			if err := m.imageErr[pid]; err != nil {
				return "", err
			}
			p, ok := m.images[pid]
			if !ok {
				return "", errors.New("no such process")
			}
			return p, nil
		},
		readVersion: func(exe string) string { return m.version[pathKey(exe)] },
		checkPatch: func(path string) (bool, bool, error) {
			m.patchCalls = append(m.patchCalls, path)
			return m.allPatched, m.canAutoFix, m.patchErr
		},
		holders: func(path string) ([]uint32, error) {
			m.holderCalls = append(m.holderCalls, path)
			if m.holdersErr != nil {
				return nil, m.holdersErr
			}
			return m.holders[pathKey(path)], nil
		},
		regAppData: func() string { return m.regAppData },
		appData:    func() string { return m.appData },
		readFile: func(path string) ([]byte, error) {
			m.readCalls = append(m.readCalls, path)
			b, ok := m.files[pathKey(path)]
			if !ok {
				return nil, errors.New("file not found")
			}
			return b, nil
		},
		fileExists: func(path string) bool { return m.existing[pathKey(path)] },
	}
}

// wantKind 断言 err 属于 want 那一类、且不同时属于另一类（未就绪与不支持互斥）。
func wantKind(t *testing.T, err, want error) {
	t.Helper()
	other := ErrConfirmUnsupported
	if want == ErrConfirmUnsupported {
		other = ErrConfirmNotReady
	}
	if !errors.Is(err, want) {
		t.Fatalf("err = %v，应属于 %q", err, want)
	}
	if errors.Is(err, other) {
		t.Fatalf("err = %v 同时属于 %q 与 %q", err, want, other)
	}
}

// wantNoDBAccess 断言确认在第 5 步之前就停下：没有查询库的占用者，也没有读任何文件。
func wantNoDBAccess(t *testing.T, m *fakeMachine) {
	t.Helper()
	if len(m.holderCalls) != 0 || len(m.readCalls) != 0 {
		t.Fatalf("闸门未通过却访问了数据目录：holders=%v readFile=%v", m.holderCalls, m.readCalls)
	}
}
