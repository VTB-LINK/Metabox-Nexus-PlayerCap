package main

// 本文件只测 syncChineseReadme：把程序目录里随包的 README.html 复制为中文名的 使用说明.html（#215，设计 §4.10）。
//
// 沿用同包的测试辅助：dirNames（canonicalname_test.go，按名字排序）、writeFile（updatebackup_test.go）、
// assertFileEquals（updatefetch_test.go）。

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// assertOnly 断言 dir 里恰为 README.html 与 使用说明.html 两项，没有留下临时文件。
func assertOnly(t *testing.T, dir string) {
	t.Helper()
	if got, want := dirNames(t, dir), []string{readmeName, chineseReadmeName}; !reflect.DeepEqual(got, want) {
		t.Fatalf("目录条目 %v，应为 %v", got, want)
	}
}

// 变异自证（实测）：
//   - 不论 README.html 在不在都写副本（读失败时以空内容写）→「没有 README.html」建出副本，红；
//   - 内容相同也重写 →「内容相同时不重写」修改时间变了，红；
//   - 只比较长度 →「长度相同、内容不同」不覆盖，红；
//   - 临时文件写入时不截断（O_WRONLY|O_CREATE，不带 O_TRUNC）→「残留的临时文件」副本末尾带着残留内容，红；
//   - 改名失败后不删临时文件 →「副本位置是目录」目录里多出临时文件，红；
//   - 直接写副本、不经临时文件（临时文件名取副本名）→「副本位置是目录」失败后的清理把那个空目录删了、
//     「残留的临时文件」目录里仍有临时文件，红。
func TestSyncChineseReadme(t *testing.T) {
	const readme = "<html><body>README v2</body></html>"

	t.Run("没有 README.html", func(t *testing.T) {
		dir := t.TempDir()
		syncChineseReadme(dir)
		if names := dirNames(t, dir); len(names) != 0 {
			t.Fatalf("目录里应为空，实际 %v", names)
		}
	})

	t.Run("没有副本时创建", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, readmeName), readme)
		syncChineseReadme(dir)
		assertFileEquals(t, filepath.Join(dir, chineseReadmeName), []byte(readme))
		assertOnly(t, dir)
	})

	t.Run("内容不同时覆盖", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, readmeName), readme)
		writeFile(t, filepath.Join(dir, chineseReadmeName), "<html>旧版的使用说明，比新版长得多……</html>")
		syncChineseReadme(dir)
		assertFileEquals(t, filepath.Join(dir, chineseReadmeName), []byte(readme))
		assertOnly(t, dir)
	})

	t.Run("长度相同、内容不同", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, readmeName), readme)
		writeFile(t, filepath.Join(dir, chineseReadmeName), strings.ToUpper(readme))
		syncChineseReadme(dir)
		assertFileEquals(t, filepath.Join(dir, chineseReadmeName), []byte(readme))
	})

	t.Run("内容相同时不重写", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, chineseReadmeName)
		writeFile(t, filepath.Join(dir, readmeName), readme)
		writeFile(t, dst, readme)
		old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := os.Chtimes(dst, old, old); err != nil {
			t.Fatal(err)
		}
		syncChineseReadme(dir)
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(old) {
			t.Fatalf("副本被重写：修改时间 %v，应仍为 %v", fi.ModTime(), old)
		}
		assertOnly(t, dir)
	})

	t.Run("副本位置是目录", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, readmeName), readme)
		if err := os.Mkdir(filepath.Join(dir, chineseReadmeName), 0o755); err != nil {
			t.Fatal(err)
		}
		syncChineseReadme(dir)
		assertFileEquals(t, filepath.Join(dir, readmeName), []byte(readme))
		if fi, err := os.Stat(filepath.Join(dir, chineseReadmeName)); err != nil || !fi.IsDir() {
			t.Fatalf("%s 应仍是目录：%v", chineseReadmeName, err)
		}
		// 写不进去时删掉临时文件，不在程序目录里留下杂物。
		assertOnly(t, dir)
	})

	t.Run("残留的临时文件", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, readmeName), readme)
		// 上一次写到一半被打断留下的临时文件，比新内容长。
		writeFile(t, filepath.Join(dir, chineseReadmeName+".tmp"), strings.Repeat("stale ", 64))
		syncChineseReadme(dir)
		assertFileEquals(t, filepath.Join(dir, chineseReadmeName), []byte(readme))
		assertOnly(t, dir)
	})
}
