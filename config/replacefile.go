package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// 替换 config.yml 的步骤（设计 doc/design/config-version-and-offset-delay-design.md §3.6）。
//
// 不用「先复制出备份、再覆盖原文件」：比对与覆盖之间总有一段时间，其间被保存的内容会被覆盖且没有副本。
// 改用两个原语，自动迁移、只改版本键、强制重建、按模板生成四条路径都只用它们：
//
//   - 改名即备份（renameToBackup）：config.yml 只经改名离开原位，移走的就是那一刻实际在位的那一份。
//   - 不存在才放置（placeIfAbsent）：新内容只在原位空着时放进去，盖不掉在位的文件。放置是原子的（硬链接，
//     或不覆盖的改名），config.yml 这个名字上不做创建、写入或删除。
//
// 每次操作只碰自己持有的名字：临时文件与备份名都由 os.CreateTemp 独占地建出，本程序的另一个实例拿不到
// 同一个名字。一次操作只删自己建的临时文件、自己预留的空文件，以及（只改版本键且自己放置成功之后）
// 自己那份与加载时读到的内容逐字节相同的备份；不按名字模式清理任何别的文件。由此，两个实例同时启动时
// 无论怎样交错，用户的原文件都至少留有一份完整副本（config/concurrentinstances_test.go）。不加跨进程的锁。
//
// 崩溃留下的临时文件（config.yml.*.tmp）与空的预留备份名不自动清理：按名字清理会删掉别的实例正在用的
// 那一个。

// fileOps 是替换 config.yml 所用的文件操作与时钟。生产路径恒为 osFileOps；测试换成注入的实现，
// 以便逐步注入失败、在两步之间中止，或在两步之间执行另一个实例的动作。
type fileOps struct {
	createTemp func(dir, pattern string) (*os.File, error)
	rename     func(oldpath, newpath string) error
	link       func(oldname, newname string) error
	// moveNoReplace 是不覆盖的改名：dst 已存在时失败（errors.Is(err, fs.ErrExist)）且 src、dst 都不动，
	// 成功时 src 整份移到 dst。实现按平台分文件（replacefile_windows.go / replacefile_other.go）。
	moveNoReplace func(src, dst string) error
	readFile      func(path string) ([]byte, error)
	remove        func(path string) error
	now           func() time.Time
	// step 在 replaceFile 的每一步之后被调用，name 是刚完成的那一步；nil 即不调用。按模板生成
	// （generateDefaultConfig）在模板写进临时文件之后同样调用一次。
	step func(name string)
}

var osFileOps = &fileOps{
	createTemp:    os.CreateTemp,
	rename:        os.Rename,
	link:          os.Link,
	moveNoReplace: moveNoReplace,
	readFile:      os.ReadFile,
	remove:        os.Remove,
	now:           time.Now,
}

func (ops *fileOps) after(name string) {
	if ops.step != nil {
		ops.step(name)
	}
}

// replaceFile 各步的名字，传给 fileOps.step。
const (
	stepTempWritten    = "temp-written"    // 新内容已写进临时文件
	stepBackupReserved = "backup-reserved" // 备份名已预留（空文件）
	stepRenamed        = "renamed"         // config.yml 已改名为备份
	stepVerified       = "verified"        // 备份与加载时读到的内容相同
	stepPlaced         = "placed"          // 临时文件已放置为 config.yml
	stepTempRemoved    = "temp-removed"    // 临时文件已删
)

const tempPattern = configName + ".*.tmp"

// backupNameRE 认本程序留下的备份名：config.backup-<版本>-<本地时间 yyyyMMdd-HHmmss>-<随机段>.yml。
// 版本里可以有连字符（预发布后缀），时间与随机段从右边定位。
var backupNameRE = regexp.MustCompile(`^config\.backup-(.+)-(\d{8}-\d{6})-[^-.]+\.yml$`)

// backupPattern 返回交给 os.CreateTemp 的备份名模式，随机段由它填入 * 处。label 是本进程所知的该文件的
// 版本（backupLabel）：名字因此说明这份备份里的 offset 是哪种语义。
func backupPattern(label string, now time.Time) string {
	return "config.backup-" + label + "-" + now.Format("20060102-150405") + "-*.yml"
}

// backupLabel 返回文件版本在备份名与提示里的写法：没有版本键的写作 v3，版本键非法的写作 unknown，
// 其余是版本的规范形式。
func backupLabel(version string, state versionKeyState) string {
	switch state {
	case versionKeyAbsent:
		return "v3"
	case versionKeyInvalid:
		return "unknown"
	}
	return version
}

// writeTemp 把 content 完整写进 path 同目录下新建的临时文件（写入、Sync、关闭），返回它的路径。
// 失败时删掉没写完的临时文件。
func writeTemp(ops *fileOps, path string, content []byte) (string, error) {
	f, err := ops.createTemp(filepath.Dir(path), tempPattern)
	if err != nil {
		return "", err
	}
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		ops.remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// renameToBackup 把 path 改名为备份并返回备份的路径：先由 os.CreateTemp 预留一个只属于本次操作的备份名
// （得到一个空文件），再把 path 改名到它上面。同目录内改名是原子的；os.Rename 会覆盖已存在的目标，
// 这里被覆盖的只能是自己刚预留的空文件。改名失败（例如 path 已不在）时删掉这个空的预留文件。
//
// 名字不是先查后用：同一秒、同一版本的两次操作，乃至同时启动的两个实例，各自拿到不同的名字。
func renameToBackup(ops *fileOps, path, label string) (string, error) {
	f, err := ops.createTemp(filepath.Dir(path), backupPattern(label, ops.now()))
	if err != nil {
		return "", err
	}
	backup := f.Name()
	f.Close()
	ops.after(stepBackupReserved)
	if err := ops.rename(path, backup); err != nil {
		ops.remove(backup)
		return "", err
	}
	return backup, nil
}

// placeIfAbsent 把 src 放置为 dst，dst 已存在时不动它并返回 fs.ErrExist。
//
// 首选硬链接：os.Link 在目标已存在时失败且不动它，成功时出现的就是完整的文件，没有写到一半的状态。
// 它因别的原因失败（文件系统不支持硬链接，如 FAT32、exFAT）时，退回不覆盖的改名（fileOps.moveNoReplace）：
// 同样是目标已存在即失败、成功即整份到位。
//
// 本函数不在 dst 上创建、写入或删除。「带 O_EXCL 创建再写入」不是原子的：写到一半出错就得删掉没写完的
// dst，而关闭句柄与删除之间别的程序若把 dst 换成新内容，被删掉的是那一份，它没有任何副本。
//
// moved 报告 src 是否已随放置移走（经改名放置成功）。为真时 src 这个名字已经空出、不再属于本次操作，
// **调用方不得再删它**：别的实例的 os.CreateTemp 可能恰好生成了同一个名字，被删的会是它的文件。为假时
// （经硬链接放置成功，或放置失败）src 仍在，由调用方清理。
func placeIfAbsent(ops *fileOps, src, dst string) (moved bool, err error) {
	err = ops.link(src, dst)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return false, err
	}
	err = ops.moveNoReplace(src, dst)
	return err == nil, err
}

// replaceFailure 是 replaceFile 没有把新内容放置成功的原因。
type replaceFailure int

const (
	replaceDone         replaceFailure = iota // 已放置
	replaceTempFailed                         // 写临时文件失败：文件没动
	replaceRenameFailed                       // 预留备份名或改名失败：文件没动
	replaceFileChanged                        // 改名得到的那一份与加载时读到的不同，或读不出：已尽量放回
	replaceFileAppeared                       // 放置时 config.yml 已存在（别的程序新建了它）：不覆盖
	replacePlaceFailed                        // 放置因别的原因失败
)

// replaceResult 是 replaceFile 的结果。
type replaceResult struct {
	failure replaceFailure
	err     error  // 底层的错误，可能为 nil
	backup  string // 留在磁盘上的备份（改名得到的那一份）的路径；没有留下时为空串
}

// replaceFile 把 path 换成 content，被换下的那一份改名为备份（名字里的版本取 label）。步骤：
//
//  1. content 写进临时文件。失败：文件没动。
//  2. 预留备份名，path 改名过去。失败：文件没动，删掉空的预留文件与临时文件。
//  3. expect 非 nil 时读备份、与 expect（加载时读到的字节）比对。不同（加载之后被保存过）或读不出：
//     把备份放回 path（同样是不存在才放置，成功后删掉备份名；path 又出现了就两份都留），不再继续。
//  4. 临时文件放置为 path。path 已存在：不覆盖，两份都在。因别的原因放不上去且 expect 非 nil：
//     把备份放回。
//  5. 删临时文件。第 3、4 步里经改名放置的，源名（备份名、临时文件名）已随改名移走，不再删
//     （placeIfAbsent 的 moved）。
//  6. keepBackup 为假时删掉备份：只在自己的放置成功之后才删，第 3 步已确认它与 expect 逐字节相同。
//
// 自动迁移传 expect 与 keepBackup=true；只改版本键传 expect 与 keepBackup=false（新文件与 expect 只差
// 版本键一行，删掉备份不丢任何内容）；强制重建传 expect=nil 与 keepBackup=true（被替换的是哪一份，
// 备份里就是哪一份，不比对）。
//
// 清理都不用 defer：进程在两步之间崩溃时不会有清理，测试以 panic 模拟崩溃，defer 会让它看到真实崩溃
// 之后不存在的状态。
func replaceFile(ops *fileOps, path string, content, expect []byte, label string, keepBackup bool) replaceResult {
	tmp, err := writeTemp(ops, path, content)
	if err != nil {
		return replaceResult{failure: replaceTempFailed, err: err}
	}
	ops.after(stepTempWritten)

	backup, err := renameToBackup(ops, path, label)
	if err != nil {
		ops.remove(tmp)
		return replaceResult{failure: replaceRenameFailed, err: err}
	}
	ops.after(stepRenamed)

	// putBack 把备份放回 path，返回仍留在磁盘上的备份路径。放回成功后删掉备份名，它已随改名移走的除外。
	putBack := func() string {
		moved, err := placeIfAbsent(ops, backup, path)
		if err != nil {
			return backup
		}
		if !moved {
			ops.remove(backup)
		}
		return ""
	}

	if expect != nil {
		got, err := ops.readFile(backup)
		if err != nil || !bytes.Equal(got, expect) {
			left := putBack()
			ops.remove(tmp)
			return replaceResult{failure: replaceFileChanged, err: err, backup: left}
		}
		ops.after(stepVerified)
	}

	moved, err := placeIfAbsent(ops, tmp, path)
	if err != nil {
		res := replaceResult{failure: replacePlaceFailed, err: err, backup: backup}
		if errors.Is(err, fs.ErrExist) {
			res.failure = replaceFileAppeared
		} else if expect != nil {
			res.backup = putBack()
		}
		ops.remove(tmp)
		return res
	}
	ops.after(stepPlaced)

	if !moved {
		ops.remove(tmp)
	}
	ops.after(stepTempRemoved)

	if !keepBackup {
		ops.remove(backup)
		backup = ""
	}
	return replaceResult{backup: backup}
}

// latestBackup 返回 dir 里最新一份非空备份的文件名，没有时返回空串。新旧按名字里的时间排，时间相同的
// 按名字排。空的备份名是预留之后没来得及改名的，不算。
func latestBackup(dir string) string {
	entries, _ := os.ReadDir(dir)
	var best, bestTime string
	for _, e := range entries {
		g := backupNameRE.FindStringSubmatch(e.Name())
		if g == nil || e.IsDir() {
			continue
		}
		if info, err := e.Info(); err != nil || info.Size() == 0 {
			continue
		}
		if g[2] > bestTime || (g[2] == bestTime && e.Name() > best) {
			best, bestTime = e.Name(), g[2]
		}
	}
	return best
}
