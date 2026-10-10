package config

// 本文件只测两个实例同时启动时对同一个 config.yml 的各种交错（设计 §3.6 末的时间线）：无论怎样交错，
// 用户的原文件 U 都至少留有一份字节完整的副本。
//
// 配置处理早于端口绑定，端口冲突挡不住第二个实例走到这里。保证来自三条性质，而不是锁：改名的目标是
// 各自独占预留的名字；config.yml 只经不存在才放置出现；一次操作只删自己的临时文件、自己预留的空文件，
// 以及（只改版本键且自己放置成功之后）自己那份与加载时读到的内容逐字节相同的备份。
//
// 两个实例各在自己的 goroutine 里跑真实的流程，文件操作在每一步之后停下，由用例按步骤交替放行
// （instance），不靠真实并发与睡眠。记法同设计：实例 A、B，临时文件 tA、tB，备份 bA、bB。

import (
	"testing"
)

// 时间线一：A 只改版本键，B 见没有 config.yml 而按模板生成。
//
// 变异自证（隔离的 worktree，实测）：
//   - 临时文件改回固定的名字 →「A 先放置」红（B 的模板盖掉 A 写好的临时文件，A 放上去的成了模板，
//     U 的改写结果丢失）。
//   - 只改版本键路径在自己的放置没成功时也删备份 →「B 先放置」红（U 没有任何副本）。
//   - 放置新文件时允许覆盖已存在的 config.yml →「A 先放置」红（B 的模板盖掉 A 的结果）、「B 先放置」红。
//   - 按模板生成时直接写入、不经不存在才放置 →「A 先放置」红。
func TestConcurrentStampAndGenerate(t *testing.T) {
	const u = "config-version: \"v4.0.0\"\naddr: \"127.0.0.1:9000\"\noffset: -123\n"
	const stampedU = "config-version: \"v4.0.1\"\naddr: \"127.0.0.1:9000\"\noffset: -123\n"

	// 两种顺序的开头相同：A 改名并核对通过、停在放置之前；B 启动，见没有 config.yml，写好模板的临时文件。
	begin := func(t *testing.T) (s *sandbox, a, b *instance, cfgA, cfgB *Config) {
		quietLog(t)
		s = newSandbox(t, u)
		a, cfgA = s.loadIn("A", "4.0.1")
		a.runTo(stepVerified)
		s.wantFiles(configName+".*.tmp", "config.backup-v4.0.0-*.yml") // tA、bA；没有 config.yml
		if !s.hasCopyOf(u) {
			t.Fatal("bA 应是 U")
		}
		b, cfgB = s.loadIn("B", "4.0.1")
		b.runTo(stepTempWritten)
		s.wantFiles(configName+".*.tmp", configName+".*.tmp", "config.backup-v4.0.0-*.yml") // tA、tB、bA
		return
	}

	t.Run("A 先放置", func(t *testing.T) {
		s, a, b, cfgA, cfgB := begin(t)
		a.finish() // 放置 tA，删 tA、删 bA
		if s.read() != stampedU {
			t.Fatalf("A 放置后 config.yml 应是改过版本键的 U，实际 %q", s.read())
		}
		s.wantFiles(configName, configName+".*.tmp") // 只剩 tB
		b.finish()                                   // 放置 tB：已存在，不覆盖；删 tB，改为加载 config.yml
		if s.read() != stampedU {
			t.Errorf("最终 config.yml 应是改过版本键的 U，实际 %q", s.read())
		}
		s.wantFiles(configName) // 没有备份，没有临时文件
		if cfgA.FileRewrite != RewriteStamped || cfgB.ConfigFileState != FileLoaded || cfgB.FileRewrite != RewriteNone || cfgB.Offset != -123 {
			t.Errorf("A 改写=%d；B 加载结果=%d、改写=%d、offset=%d，期望 B 加载到 A 放上的那一份", cfgA.FileRewrite, cfgB.ConfigFileState, cfgB.FileRewrite, cfgB.Offset)
		}
	})

	t.Run("B 先放置", func(t *testing.T) {
		s, a, b, cfgA, cfgB := begin(t)
		b.finish() // config.yml 是模板
		if s.read() != templateContent("v4.0.1") || cfgB.ConfigFileState != FileGenerated {
			t.Fatalf("B 放置后 config.yml 应是模板，B 的加载结果=%d", cfgB.ConfigFileState)
		}
		a.finish() // A 的放置因已存在而失败：bA 不删
		if s.read() != templateContent("v4.0.1") {
			t.Errorf("A 不应覆盖 B 放上的模板")
		}
		s.wantFiles(configName, "config.backup-v4.0.0-*.yml")
		if cfgA.FileRewrite != RewriteStampPending || cfgA.FileBackup == "" || readTestFile(t, cfgA.FileBackup) != u {
			t.Errorf("A 改写=%d、备份=%q，期望未写成且 bA 仍是 U", cfgA.FileRewrite, cfgA.FileBackup)
		}
		if !s.hasCopyOf(u) {
			t.Error("U 应完整留在 bA")
		}
	})
}

// 时间线三：A、B 同时迁移，加载到的都是 U。
//
// 变异自证（隔离的 worktree，实测）：
//   - 迁移路径不核对改名得到的那一份 →「A 先完成」「都已预留备份名」红（B 把 A 的迁移结果当作原文件
//     再写一遍自己的结果，留下一份内容是迁移结果、名字却写着 v3 的备份）。
//   - 核对不同也照写、不放回 → 同上两例红。
//   - 备份名改回先查后改名（同一秒同一版本用同一个名字）→「都已预留备份名」红（B 的改名盖掉 bA，
//     U 没有任何副本）。
//   - 预留备份名成功而改名失败后不删空的预留文件 →「B 的改名落在 A 的改名与放置之间」红。
func TestConcurrentMigrations(t *testing.T) {
	m := v3Migrated("v4.0.0")
	// 结束时：config.yml 是迁移结果，U 完整留在唯一的一份备份 bA 里，没有临时文件与空的预留名。
	wantEnd := func(t *testing.T, s *sandbox, cfgA, cfgB *Config) {
		t.Helper()
		if s.read() != m {
			t.Errorf("最终 config.yml 应是迁移结果，实际\n%s", s.read())
		}
		s.wantFiles(configName, "config.backup-v3-*.yml")
		if cfgA.FileRewrite != RewriteMigrated || readTestFile(t, cfgA.FileBackup) != v3Config {
			t.Errorf("A 改写=%d，bA 应是 U", cfgA.FileRewrite)
		}
		if cfgB.FileRewrite != RewriteMigratePending || cfgB.FileBackup != "" || cfgB.Offset != -200 {
			t.Errorf("B 改写=%d、备份=%q、offset=%d，期望未写成、按内存换算运行", cfgB.FileRewrite, cfgB.FileBackup, cfgB.Offset)
		}
		if !s.hasCopyOf(v3Config) {
			t.Error("U 应完整留在 bA")
		}
	}

	t.Run("A 先完成", func(t *testing.T) {
		quietLog(t)
		s := newSandbox(t, v3Config)
		a, cfgA := s.loadIn("A", "4.0.0")
		a.runTo(stepTempWritten)
		b, cfgB := s.loadIn("B", "4.0.0")
		b.runTo(stepTempWritten) // 两者加载到的都是 U
		a.finish()               // A 改名，核对通过，放置 M
		b.runTo(stepRenamed)     // B 改名：移走的是 M
		s.wantFiles(configName+".*.tmp", "config.backup-v3-*.yml", "config.backup-v3-*.yml")
		if s.exists() {
			t.Fatal("B 改名之后不应有 config.yml")
		}
		b.finish() // B 读 bB，与自己加载时的 U 不同，把 bB 放回
		wantEnd(t, s, cfgA, cfgB)
	})

	t.Run("都已预留备份名", func(t *testing.T) {
		quietLog(t)
		s := newSandbox(t, v3Config)
		a, cfgA := s.loadIn("A", "4.0.0")
		a.runTo(stepBackupReserved)
		b, cfgB := s.loadIn("B", "4.0.0")
		b.runTo(stepBackupReserved)
		if got := s.backups(); len(got) != 2 || got[0] == got[1] {
			t.Fatalf("同一秒、同一版本的两个实例应各预留一个不同的备份名，实际 %v", got)
		}
		a.finish()
		b.finish()
		wantEnd(t, s, cfgA, cfgB)
	})

	t.Run("B 的改名落在 A 的改名与放置之间", func(t *testing.T) {
		quietLog(t)
		s := newSandbox(t, v3Config)
		a, cfgA := s.loadIn("A", "4.0.0")
		a.runTo(stepTempWritten)
		b, cfgB := s.loadIn("B", "4.0.0")
		b.runTo(stepTempWritten)
		a.runTo(stepRenamed) // config.yml 不在
		b.finish()           // B 的改名失败，删掉空的 bB 与 tB
		s.wantFiles(configName+".*.tmp", "config.backup-v3-*.yml")
		a.finish()
		wantEnd(t, s, cfgA, cfgB)
	})
}

// rebuildIn 起一个对 s 做强制重建的实例（cfg 是它先前加载得到的 Config），停在它的第一步之后。
func (s *sandbox) rebuildIn(name string, cfg *Config, version string) (*instance, *RebuildReport) {
	rep := &RebuildReport{}
	return startInstance(s.t, name, func(ops *fileOps) { *rep = forceRebuild(ops, cfg, version) }), rep
}

// 时间线二：A、B 同时重建。U 没有版本键、含 offset: 0x1f4，两者的 Load 都没有落盘。
//
// 变异自证（隔离的 worktree，实测）：备份名改回先查后改名 →「都已预留备份名」红（B 的改名盖掉 bA，U 没有
// 任何副本）；临时文件改回固定的名字 → 两例都红。
func TestConcurrentRebuilds(t *testing.T) {
	const u = "offset: 0x1f4\naddr: \"127.0.0.1:9000\"\n"
	template := templateContent("v4.0.0")
	begin := func(t *testing.T) (s *sandbox, a, b *instance, repA, repB *RebuildReport) {
		quietLog(t)
		s = newSandbox(t, u)
		cfgA, _ := s.load("4.0.0", testOps())
		cfgB, _ := s.load("4.0.0", testOps())
		if s.read() != u || !cfgA.ShouldRebuild("v4.0.0", "4.0.0", "") || !cfgB.ShouldRebuild("v4.0.0", "4.0.0", "") {
			t.Fatal("前置：Load 不应落盘，两个实例的判定都应成立")
		}
		a, repA = s.rebuildIn("A", &cfgA, "4.0.0")
		b, repB = s.rebuildIn("B", &cfgB, "4.0.0")
		return
	}

	t.Run("都已预留备份名", func(t *testing.T) {
		s, a, b, repA, repB := begin(t)
		a.runTo(stepBackupReserved)
		b.runTo(stepBackupReserved)
		if got := s.backups(); len(got) != 2 || got[0] == got[1] || s.file(got[0]) != "" || s.file(got[1]) != "" || s.read() != u {
			t.Fatalf("两个实例应各预留一个不同的空备份名，config.yml 仍是 U：%v", got)
		}
		a.finish() // A 改名（bA = U）、放置模板
		if s.read() != template || readTestFile(t, repA.Backup) != u {
			t.Fatalf("A 完成后 config.yml 应是模板、bA 应是 U")
		}
		b.finish() // B 改名：移走的是 A 放上的模板，落在 bB，碰不到 bA；再放置模板
		s.wantFiles(configName, "config.backup-v3-*.yml", "config.backup-v3-*.yml")
		if !repA.Done || !repB.Done || s.read() != template || readTestFile(t, repA.Backup) != u || readTestFile(t, repB.Backup) != template {
			t.Errorf("最终应是 config.yml = 模板、bA = U、bB = 模板：A=%+v B=%+v", repA, repB)
		}
	})

	t.Run("B 的改名落在 A 的改名与放置之间", func(t *testing.T) {
		s, a, b, repA, repB := begin(t)
		a.runTo(stepRenamed) // config.yml 不在
		b.finish()           // B 的改名失败，删掉空的 bB 与 tB，本次不重建
		if repB.Done || repB.Backup != "" {
			t.Errorf("B 不应重建：%+v", repB)
		}
		s.wantFiles(configName+".*.tmp", "config.backup-v3-*.yml") // 只剩 tA、bA
		a.finish()
		s.wantFiles(configName, "config.backup-v3-*.yml")
		if !repA.Done || s.read() != template || readTestFile(t, repA.Backup) != u {
			t.Errorf("A 应照常重建，U 在 bA：%+v", repA)
		}
	})
}

// 时间线四：A 迁移，B 同时重建。B 加载之后用户又把 v3 的旧文件 U 存了回去，A 加载到的是它。
// 各种顺序下 U 都在某一份备份里。
func TestConcurrentMigrationAndRebuild(t *testing.T) {
	template, m := templateContent("v4.0.0"), v3Migrated("v4.0.0")
	begin := func(t *testing.T) (s *sandbox, cfgB Config) {
		quietLog(t)
		s = newSandbox(t, "offset: 0x1f4\n")
		cfgB, _ = s.load("4.0.0", testOps()) // B 的 Load 没有落盘
		s.write(v3Config)                    // 用户把旧文件存了回去
		return
	}

	t.Run("B 先改名", func(t *testing.T) {
		s, cfgB := begin(t)
		a, cfgA := s.loadIn("A", "4.0.0")
		a.runTo(stepTempWritten) // A 加载到 U
		b, repB := s.rebuildIn("B", &cfgB, "4.0.0")
		b.runTo(stepRenamed) // bB = U
		a.finish()           // A 的改名失败，按内存换算运行
		b.finish()           // B 放上模板
		s.wantFiles(configName, "config.backup-v3-*.yml")
		if cfgA.FileRewrite != RewriteMigratePending || cfgA.Offset != -200 || !repB.Done || s.read() != template || readTestFile(t, repB.Backup) != v3Config {
			t.Errorf("A 改写=%d offset=%d；B=%+v；期望 A 未写成、config.yml = 模板、bB = U", cfgA.FileRewrite, cfgA.Offset, repB)
		}
	})

	t.Run("A 先完成", func(t *testing.T) {
		s, cfgB := begin(t)
		a, cfgA := s.loadIn("A", "4.0.0")
		a.finish() // config.yml = M，bA = U
		b, repB := s.rebuildIn("B", &cfgB, "4.0.0")
		b.finish() // B 改名移走 M，放上模板：与单个实例「迁移之后紧接着重建」相同
		s.wantFiles(configName, "config.backup-v3-*.yml", "config.backup-v3-*.yml")
		if cfgA.FileRewrite != RewriteMigrated || !repB.Done || s.read() != template || readTestFile(t, cfgA.FileBackup) != v3Config || readTestFile(t, repB.Backup) != m {
			t.Errorf("期望 config.yml = 模板、bA = U、bB = 迁移结果：A 改写=%d B=%+v", cfgA.FileRewrite, repB)
		}
	})

	t.Run("B 的改名落在 A 的改名与放置之间", func(t *testing.T) {
		s, cfgB := begin(t)
		a, cfgA := s.loadIn("A", "4.0.0")
		a.runTo(stepRenamed)
		b, repB := s.rebuildIn("B", &cfgB, "4.0.0")
		b.finish() // B 的改名失败，本次不重建
		a.finish() // A 照常放上 M
		s.wantFiles(configName, "config.backup-v3-*.yml")
		if repB.Done || cfgA.FileRewrite != RewriteMigrated || s.read() != m || readTestFile(t, cfgA.FileBackup) != v3Config {
			t.Errorf("期望 B 不重建、config.yml = 迁移结果、bA = U：A 改写=%d B=%+v", cfgA.FileRewrite, repB)
		}
	})
}
