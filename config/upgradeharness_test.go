package config

// 本文件只放 config.yml 版本处理各用例的共用夹具，本身不含测试：临时目录里的一份 config.yml（sandbox）、
// 可注入失败与中止的文件操作、以及让两个实例按指定步骤交错的调度（instance）。

import (
	"errors"
	"io"
	stdlog "log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// realPlayers 是出货构建里注册的六个播放器。本包的测试二进制不 import 播放器包，注册表里没有它们。
var realPlayers = []string{"wesing", "cloudmusicv3", "qqmusic", "kugou", "sodamusic", "applemusic"}

// useRealPlayers 把 realPlayers 登记进注册表（已登记的不重复登记）。注册表只增不减，同包其它测试不依赖
// 它为空。
func useRealPlayers() {
	for _, name := range realPlayers {
		if !slices.Contains(RegisteredPlayers(), name) {
			RegisterPlayer(name)
		}
	}
}

// fixedNow 是夹具里的时钟：备份名里的时间恒为 20261009-153045。
var fixedNow = time.Date(2026, 10, 9, 15, 30, 45, 0, time.Local)

// testOps 返回真实的文件操作加固定的时钟；各用例在它上面替换个别操作。它的 remove 另记下每一次以
// config.yml 的路径发起的删除（noteRemove），由 newSandbox 在用例结束时断言没有。
func testOps() *fileOps {
	ops := *osFileOps
	ops.now = func() time.Time { return fixedNow }
	ops.remove = func(path string) error {
		noteRemove(path)
		return os.Remove(path)
	}
	return &ops
}

// removedConfigs 记下经夹具的 remove 删除过的各个 config.yml 的路径。
var removedConfigs sync.Map

func noteRemove(path string) {
	if filepath.Base(path) == configName {
		removedConfigs.Store(path, true)
	}
}

var errInjected = errors.New("injected failure")

// failNth 返回让第 n 次文件操作（各种操作合计，从 1 数起）失败的文件操作，以及走到过的操作次数。
func failNth(n int) (*fileOps, *int) {
	ops, count := testOps(), new(int)
	hit := func() bool { *count++; return *count == n }
	createTemp, rename, link, move, readFile, remove := ops.createTemp, ops.rename, ops.link, ops.moveNoReplace, ops.readFile, ops.remove
	ops.createTemp = func(dir, pattern string) (*os.File, error) {
		if hit() {
			return nil, errInjected
		}
		return createTemp(dir, pattern)
	}
	ops.rename = func(oldpath, newpath string) error {
		if hit() {
			return errInjected
		}
		return rename(oldpath, newpath)
	}
	ops.link = func(oldname, newname string) error {
		if hit() {
			return errInjected
		}
		return link(oldname, newname)
	}
	ops.moveNoReplace = func(src, dst string) error {
		if hit() {
			return errInjected
		}
		return move(src, dst)
	}
	ops.readFile = func(path string) ([]byte, error) {
		if hit() {
			return nil, errInjected
		}
		return readFile(path)
	}
	ops.remove = func(path string) error {
		if hit() {
			noteRemove(path)
			return errInjected
		}
		return remove(path)
	}
	return ops, count
}

// sandbox 是一个只放着 config.yml（及其备份、临时文件）的临时目录，exe 路径与当前目录都指向它。
type sandbox struct {
	t    *testing.T
	dir  string
	path string
}

// newSandbox 建目录；content 非空串时写成 config.yml。
//
// 用例结束时断言 config.yml 没有被按路径删除过：替换它的任何路径、任何失败下，一次操作只清理自己持有的
// 名字（临时文件、预留的备份名），config.yml 这个公共的名字只经改名离开原位。凡用 newSandbox 与 testOps
// 的用例都带着这条断言。
func newSandbox(t *testing.T, content string) *sandbox {
	t.Helper()
	useRealPlayers()
	dir := t.TempDir()
	s := &sandbox{t: t, dir: dir, path: filepath.Join(dir, configName)}
	t.Cleanup(func() {
		if _, hit := removedConfigs.Load(s.path); hit {
			t.Errorf("config.yml 被按路径删除过（%s）：别的程序其间放上的内容会随之丢失", s.path)
		}
	})
	withPaths(t, filepath.Join(dir, "app.exe"), dir, nil)
	if content != "" {
		s.write(content)
	}
	return s
}

func (s *sandbox) write(content string) { s.t.Helper(); writeTestFile(s.t, s.path, content) }
func (s *sandbox) read() string         { s.t.Helper(); return readTestFile(s.t, s.path) }

func (s *sandbox) exists() bool {
	_, err := os.Lstat(s.path)
	return err == nil
}

// names 返回目录里文件名符合 glob 的各项（已排序）。
func (s *sandbox) names(glob string) []string {
	s.t.Helper()
	matches, err := filepath.Glob(filepath.Join(s.dir, glob))
	if err != nil {
		s.t.Fatal(err)
	}
	for i, m := range matches {
		matches[i] = filepath.Base(m)
	}
	sort.Strings(matches)
	return matches
}

func (s *sandbox) backups() []string { return s.names("config.backup-*.yml") }
func (s *sandbox) temps() []string   { return s.names(configName + ".*.tmp") }

// file 读目录里名为 name 的文件。
func (s *sandbox) file(name string) string {
	s.t.Helper()
	return readTestFile(s.t, filepath.Join(s.dir, name))
}

// wantFiles 断言目录里恰好是这些文件名（glob 写法，逐个对应一项），不多不少。
func (s *sandbox) wantFiles(globs ...string) {
	s.t.Helper()
	all := s.names("*")
	rest := slices.Clone(all)
	for _, g := range globs {
		i := slices.IndexFunc(rest, func(name string) bool { ok, _ := filepath.Match(g, name); return ok })
		if i < 0 {
			s.t.Errorf("目录里应有 %s，实际只有 %v", g, all)
			continue
		}
		rest = slices.Delete(rest, i, i+1)
	}
	if len(rest) > 0 {
		s.t.Errorf("目录里多出 %v（全部：%v）", rest, all)
	}
}

// load 以程序版本 version 经 ops 跑一次 loadFile，返回得到的 Config 与期间的日志。
func (s *sandbox) load(version string, ops *fileOps) (Config, string) {
	s.t.Helper()
	cfg := DefaultConfig()
	for _, name := range registeredPlayers {
		cfg.Players[name] = &PlayerConfig{}
	}
	logs := captureLog(s.t, func() { loadFileWithOps(ops, &cfg, version) })
	return cfg, logs
}

// crashSignal 是模拟崩溃用的 panic 值。
type crashSignal struct{ step string }

// crashAfter 返回在 step 这一步之后「崩溃」的文件操作：以 panic 中止，不做任何清理。
func crashAfter(step string) *fileOps {
	ops := testOps()
	ops.step = func(name string) {
		if name == step {
			panic(crashSignal{step})
		}
	}
	return ops
}

// crashed 跑 fn，报告它是否以 crashSignal 中止；别的 panic 照常抛出。
func crashed(fn func()) (hit bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(crashSignal); !ok {
				panic(r)
			}
			hit = true
		}
	}()
	fn()
	return false
}

// instance 是在自己的 goroutine 里跑的一个「程序实例」，它的文件操作在每一步之后停下，等测试放行。
// 两个 instance 由测试按步骤交替放行，不靠真实并发与睡眠。
type instance struct {
	t       *testing.T
	name    string
	ops     *fileOps
	reached chan string
	resume  chan struct{}
	done    chan struct{}
	paused  bool
}

// startInstance 起一个实例跑 run（它须经传入的 ops 做文件操作），停在它的第一步之后或跑完。
func startInstance(t *testing.T, name string, run func(ops *fileOps)) *instance {
	i := &instance{t: t, name: name, ops: testOps(), reached: make(chan string), resume: make(chan struct{}), done: make(chan struct{})}
	i.ops.step = func(step string) {
		i.reached <- step
		<-i.resume
	}
	go func() {
		defer close(i.done)
		run(i.ops)
	}()
	return i
}

// runTo 放行到实例完成 step 这一步为止，停在那里。实例先跑完了即失败。
func (i *instance) runTo(step string) {
	i.t.Helper()
	for {
		if i.paused {
			i.paused = false
			i.resume <- struct{}{}
		}
		select {
		case got := <-i.reached:
			i.paused = true
			if got == step {
				return
			}
		case <-i.done:
			i.t.Fatalf("实例 %s 没有走到 %s 这一步就结束了", i.name, step)
		}
	}
}

// finish 放行到实例跑完。
func (i *instance) finish() {
	i.t.Helper()
	for {
		if i.paused {
			i.paused = false
			i.resume <- struct{}{}
		}
		select {
		case <-i.reached:
			i.paused = true
		case <-i.done:
			return
		}
	}
}

// hasCopyOf 报告目录里是否有一个文件的内容恰为 content。
func (s *sandbox) hasCopyOf(content string) bool {
	for _, name := range s.names("*") {
		if s.file(name) == content {
			return true
		}
	}
	return false
}

// quietLog 在本用例期间丢弃日志输出（多个实例并行跑时不能各自 captureLog）。
func quietLog(t *testing.T) {
	old := stdlog.Writer()
	stdlog.SetOutput(io.Discard)
	t.Cleanup(func() { stdlog.SetOutput(old) })
}

// loadIn 起一个以程序版本 version 加载 s 的实例，停在它的第一步之后（或跑完）；cfg 在实例跑完后可读。
func (s *sandbox) loadIn(name, version string) (*instance, *Config) {
	cfg := DefaultConfig()
	for _, p := range registeredPlayers {
		cfg.Players[p] = &PlayerConfig{}
	}
	return startInstance(s.t, name, func(ops *fileOps) { loadFileWithOps(ops, &cfg, version) }), &cfg
}

// containsAll 报告 s 是否含有全部 subs。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
