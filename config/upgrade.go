package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"Metabox-Nexus-PlayerCap/i18n"
)

// RewriteState 是 Load 对 config.yml 的改写结果（版本键与自动迁移）。它与 FileState 无关：FileState 回答
// 「读到了没有」，这里回答「文件有没有被改写」。
type RewriteState int

const (
	RewriteNone           RewriteState = iota // 不需要改写：文件版本即盖章值，或 config.yml 一层没有生效
	RewriteStamped                            // 只改了版本键，不留备份
	RewriteMigrated                           // 已迁移并写入，原文件改名为备份（Config.FileBackup）
	RewriteStampPending                       // 应改版本键而没有写成
	RewriteMigratePending                     // 应迁移而没有写成：本次运行在内存里换算
	RewriteNewerFile                          // 文件版本高于程序（降级）：不写文件、不迁移
)

// upgradeFile 处理一份刚读到（字节 data）并解析成功（m）的 config.yml 的版本：记下文件版本，按它与盖章值
// 的关系执行迁移、把版本键改写为盖章值并写回文件。返回交给 mergeYAML 的解析结果。
//
// **本次运行一律按返回值运行，与文件有没有写成无关**：没写成时每次启动都在内存里重新换算一遍并告警，
// 磁盘上的文件保持原样（仍是旧版本），不存在迁了一半或迁两遍。
//
// 记进 cfg 的 FileVersion 是改写之前的版本，在动文件之前就记下：强制重建的判定只能用它。
func upgradeFile(ops *fileOps, cfg *Config, path string, data []byte, m map[string]interface{}, programVersion string) map[string]interface{} {
	plan := planUpgrade(m, StampVersion(programVersion))
	label := backupLabel(plan.fileVersion, plan.keyState)
	cfg.FileVersion, cfg.FileVersionLabel, cfg.FileStamp = plan.fileVersion, label, plan.stamp

	if plan.newer {
		cfg.FileRewrite = RewriteNewerFile
		// 开发构建读到发布版本盖过的文件是常态，不告警。
		if isReleaseVersion(programVersion) {
			log.Warn("%s was last written by a newer version (%s) than this program (%s); it is read as is and left unchanged. Settings whose meaning changed in between may not behave as written", path, label, plan.stamp)
		}
		return m
	}
	if !plan.stampKey {
		return m
	}
	if plan.keyState == versionKeyInvalid {
		log.Warn("%s in %s is not a valid version (%v); treating the file as already migrated and resetting the key to %s", versionKey, path, m[versionKey], plan.stamp)
	}

	var steps []migrationStep
	if plan.migrate {
		steps = stepsAfter(plan.fileVersion)
	}
	migrating := len(steps) > 0
	next, changed := migrateValues(m, steps, registeredPlayers)
	next[versionKey] = plan.stamp

	// pending 记下「应改而没有写成」。why 已在调用处告警过具体原因。
	pending := func() map[string]interface{} {
		cfg.FileRewrite = RewriteStampPending
		if migrating {
			cfg.FileRewrite = RewriteMigratePending
			log.Warn("%s still holds its %s content; the migration to %s is applied in memory for this run and will be retried at the next start", path, label, plan.stamp)
		}
		return next
	}

	_, hadKey := m[versionKey]
	content, err := rewriteText(data, hadKey, next, changed, steps, registeredPlayers, plan.stamp)
	if err == nil && fileReadOnly(path) {
		err = i18n.Errorf("the file is read-only")
	}
	if err != nil {
		log.Warn("%s cannot be rewritten safely (%v); file left unchanged", path, err)
		return pending()
	}

	res := replaceFile(ops, path, content, data, label, migrating)
	cfg.FileBackup = res.backup
	switch res.failure {
	case replaceDone:
		cfg.FileRewrite = RewriteStamped
		if migrating {
			cfg.FileRewrite = RewriteMigrated
			// 两处都只写文件名：备份与 config.yml 同目录，完整路径见紧随其后的「已加载」一行；两个完整
			// 路径写在一行里有两三百个字符。
			log.Info("Migrated %s from %s to %s; the original is kept as %s", filepath.Base(path), label, plan.stamp, filepath.Base(res.backup))
		}
		return next
	case replaceTempFailed, replaceRenameFailed:
		log.Warn("Cannot rewrite %s (%v); file left unchanged", path, res.err)
	case replaceFileChanged:
		if res.backup == "" {
			log.Warn("%s was modified while it was being loaded; the saved content is left in place", path)
		} else {
			log.Warn("%s was modified while it was being loaded and could not be put back; the saved content is kept as %s", path, res.backup)
		}
	case replaceFileAppeared:
		log.Warn("%s was created anew by another program while it was being rewritten; it is not overwritten, and the file it replaced is kept as %s", path, res.backup)
	case replacePlaceFailed:
		if res.backup == "" {
			log.Warn("Cannot rewrite %s (%v); file left unchanged", path, res.err)
		} else {
			log.Warn("Cannot write the new %s (%v) and the original could not be put back; it is kept as %s", path, res.err, res.backup)
		}
	}
	return pending()
}

// fileReadOnly 报告 path 是否带只读属性。只读的 config.yml 不改写：改名对只读文件照样成功（Windows 上
// 实测），不显式检查的话它会被一份不带只读属性的新文件换掉。
func fileReadOnly(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0o200 == 0
}

// generateDefaultConfig 在 path 不存在时把版本键为 stamp 的模板放置为它，返回是否由本次调用生成。
// path 必须与 Load 的读取点一致，否则「写了读不到」。
//
// 同样是不存在才放置：模板先完整写进临时文件，放置时发现 path 已出现（别的实例刚建了它）就不覆盖，
// 返回 false，由调用方改为加载那一份。
//
// 生成成功而同目录里有本程序留下的备份时告警，并把最新一份非空备份的名字记进 cfg.OrphanBackup：
// 上一次改写 config.yml 可能在「原文件已改名为备份、新文件还没放上」时被打断，用户的设置在备份里。
// 程序不自动把备份放回去：它分不清这是写入被打断，还是用户有意删掉 config.yml 以重新生成。
func generateDefaultConfig(ops *fileOps, cfg *Config, path, stamp string) bool {
	tmp, err := writeTemp(ops, path, []byte(templateContent(stamp)))
	if err == nil {
		ops.after(stepTempWritten)
		var moved bool
		if moved, err = placeIfAbsent(ops, tmp, path); !moved { // 已随改名移走的临时文件名不再删
			ops.remove(tmp)
		}
	}
	if errors.Is(err, fs.ErrExist) {
		return false
	}
	if err != nil {
		// 写失败不是致命错误（本次运行用内置默认继续），但必须可见：否则用户只会发现「我的配置怎么
		// 没生效」而看不到任何线索。
		log.Warn("Failed to generate %s: %v (using built-in default configuration for this run)", path, err)
		return false
	}
	log.Info("Auto-generated %s", path)
	if backup := latestBackup(filepath.Dir(path)); backup != "" {
		cfg.OrphanBackup = backup
		log.Warn("%s did not exist and was generated from the template, but the backup %s is in the same directory; your previous settings may be in it", path, backup)
	}
	return true
}
