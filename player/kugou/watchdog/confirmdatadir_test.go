package watchdog

// 本文件只测确认闸门第 5 步：在注册表 AppDataPath 与 %APPDATA%\KuGou8 两个候选里，
// 选出 KGMusicV3.db 正被酷狗主进程打开的那个数据目录。

import (
	"errors"
	"testing"
)

const (
	otherDataDir = `D:\KuGouData`
	otherDB      = otherDataDir + `\KGMusicV3.db`
)

func TestConfirmDataDirHeldByMain(t *testing.T) {
	m := newFakeMachine()
	// 注册表指向另一份库（旧数据），它没被主进程占用；%APPDATA%\KuGou8 的被占用。
	m.regAppData = otherDataDir
	m.existing[pathKey(otherDB)] = true
	m.holders[pathKey(otherDB)] = []uint32{5555}
	got, err := confirmWith(m.deps())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if pathKey(got.DataDir) != pathKey(stubDataDir) {
		t.Fatalf("DataDir = %q，应为 %q", got.DataDir, stubDataDir)
	}
	if len(m.holderCalls) != 2 {
		t.Fatalf("holders 调用 = %q，两个候选应各查一次", m.holderCalls)
	}

	// 反过来：注册表那份被占用，%APPDATA% 那份没有。
	m = newFakeMachine()
	m.regAppData = otherDataDir
	m.existing[pathKey(otherDB)] = true
	m.holders[pathKey(otherDB)] = []uint32{stubMainPID}
	m.holders[pathKey(stubDB)] = nil
	m.files[pathKey(otherDataDir+`\KuGou.ini`)] = m.files[pathKey(stubINI)]
	got, err = confirmWith(m.deps())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.DataDir != otherDataDir {
		t.Fatalf("DataDir = %q，应为 %q", got.DataDir, otherDataDir)
	}
}

// 注册表的 Kugou8 与 %APPDATA%\KuGou8 是同一目录的两种写法：去重为一个，只查一次。
func TestConfirmDataDirBothHeld(t *testing.T) {
	m := newFakeMachine()
	got, err := confirmWith(m.deps())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(m.holderCalls) != 1 {
		t.Fatalf("holders 调用 = %q，同一目录应只查一次", m.holderCalls)
	}
	if got.DataDir != stubDataDir {
		t.Fatalf("DataDir = %q，应为 %q", got.DataDir, stubDataDir)
	}

	// 尾部带分隔符的写法同样去重。
	m = newFakeMachine()
	m.regAppData = stubDataDir + `\`
	if _, err := confirmWith(m.deps()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(m.holderCalls) != 1 {
		t.Fatalf("holders 调用 = %q，同一目录应只查一次", m.holderCalls)
	}
}

// 两个不同目录的库都被主进程占用：分不出哪个是数据目录，按未就绪。
func TestConfirmDataDirTwoDistinctHeld(t *testing.T) {
	m := newFakeMachine()
	m.regAppData = otherDataDir
	m.existing[pathKey(otherDB)] = true
	m.holders[pathKey(otherDB)] = []uint32{stubMainPID}
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)
}

func TestConfirmDataDirNotHeld(t *testing.T) {
	m := newFakeMachine()
	m.holders[pathKey(stubDB)] = []uint32{5555, 7001} // 别的进程、子进程占用都不算
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)
	if len(m.readCalls) != 0 {
		t.Fatalf("数据目录未确认却读了文件：%q", m.readCalls)
	}

	m = newFakeMachine()
	m.holders[pathKey(stubDB)] = nil
	_, err = confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)
}

func TestConfirmRMFails(t *testing.T) {
	m := newFakeMachine()
	m.holdersErr = errors.New("RmGetList: 5")
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)

	// 两个候选里一个查询失败、另一个被主进程占用：同样未就绪，不在不完整的结果里挑。
	m = newFakeMachine()
	m.regAppData = otherDataDir
	m.existing[pathKey(otherDB)] = true
	d := m.deps()
	inner := d.holders
	d.holders = func(p string) ([]uint32, error) {
		if pathKey(p) == pathKey(otherDB) {
			return nil, errors.New("RmRegisterResources: 5")
		}
		return inner(p)
	}
	_, err = confirmWith(d)
	wantKind(t, err, ErrConfirmNotReady)
}

func TestConfirmNoCandidateDB(t *testing.T) {
	m := newFakeMachine()
	delete(m.existing, pathKey(stubDB))
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)
	if len(m.holderCalls) != 0 {
		t.Fatalf("没有含库的候选却查了占用者：%q", m.holderCalls)
	}

	// 注册表与 APPDATA 都为空：没有候选，不拼出相对路径 KuGou8。
	m = newFakeMachine()
	m.regAppData, m.appData = "", ""
	m.existing[pathKey(`KuGou8\KGMusicV3.db`)] = true
	_, err = confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)
	if len(m.holderCalls) != 0 {
		t.Fatalf("没有候选却查了占用者：%q", m.holderCalls)
	}
}

// 注册表的值不是绝对路径（未展开的环境变量、相对路径）：不当候选，免得按本程序的当前目录解析。
func TestConfirmRegistryNotAbsolute(t *testing.T) {
	for _, v := range []string{`%APPDATA%\KuGou8`, `KuGou8`, `\Users\u\KuGou8`} {
		m := newFakeMachine()
		m.regAppData = v
		m.existing[pathKey(v+`\KGMusicV3.db`)] = true
		m.holders[pathKey(v+`\KGMusicV3.db`)] = []uint32{stubMainPID}
		got, err := confirmWith(m.deps())
		if err != nil {
			t.Fatalf("%q: err = %v", v, err)
		}
		if got.DataDir != stubRoaming+`\KuGou8` {
			t.Fatalf("%q: DataDir = %q，应只用 %%APPDATA%%\\KuGou8", v, got.DataDir)
		}
		if len(m.holderCalls) != 1 {
			t.Fatalf("%q: holders 调用 = %q", v, m.holderCalls)
		}
	}
}

// 注册表指向不存在的目录（旧数据）：只用 %APPDATA%\KuGou8。
func TestConfirmStaleRegistry(t *testing.T) {
	m := newFakeMachine()
	m.regAppData = `E:\Old\Kugou8`
	got, err := confirmWith(m.deps())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(m.holderCalls) != 1 || pathKey(m.holderCalls[0]) != pathKey(stubDB) {
		t.Fatalf("holders 调用 = %q，应只查 %%APPDATA%%\\KuGou8 的库", m.holderCalls)
	}
	if pathKey(got.DataDir) != pathKey(stubDataDir) {
		t.Fatalf("DataDir = %q", got.DataDir)
	}
}
