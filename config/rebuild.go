package config

import (
	"reflect"
	"sort"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

// 强制删除重建（设计 doc/design/config-version-and-offset-delay-design.md §5）：网关的版本检查响应里带
// config_force_update，含义是「低于此版本的 config.yml 须按模板重建」。触发后不询问用户，旧文件改名为
// 备份，写入当前版本的模板。

// ShouldRebuild 判定网关字段 forceUpdate 是否要求本进程重建 config.yml。programVersion 是程序版本，
// handled 是本进程树已经处理过的门槛（环境变量里的已重建标记，没有即空串）。
//
// 文件版本用的是 loadFile 在改写之前记下的 FileVersion，不重读文件：此时磁盘上的版本键已是本进程的
// 盖章值，重读只会得出「已是新版本」。文件本次没有正常加载（新生成、读取或解析失败）的不判。
func (c *Config) ShouldRebuild(forceUpdate, programVersion, handled string) bool {
	return c.ConfigFileState == FileLoaded && rebuildRequired(c.FileVersion, forceUpdate, programVersion, handled)
}

// rebuildRequired 是门槛判定：记字段为 F、程序版本为 V、改写之前的文件版本为 S0，
//
//	S0 < F 且 F <= V 时重建。
//
// F 缺失、为空串或不是合法版本号即不强制；V 不是发布版本、S0 未知（空串）时不判。F 高于 V 时不做：
// 本程序的模板比 F 要求的旧，等程序升到不低于 F 的版本、首次启动时照常成立。
//
// handled 非空时，F 不高于它即跳过：同一棵进程树里同一个门槛至多处理一次，与文件内容无关（防循环的
// 硬闸）。更高的门槛是合法的新要求，照常判定。
func rebuildRequired(fileVersion, forceUpdate, programVersion, handled string) bool {
	f, v := normalizeVersion(forceUpdate), normalizeVersion(programVersion)
	if !semver.IsValid(f) || !isReleaseVersion(programVersion) || !semver.IsValid(fileVersion) {
		return false
	}
	if h := normalizeVersion(handled); semver.IsValid(h) && semver.Compare(f, h) <= 0 {
		return false
	}
	return semver.Compare(fileVersion, f) < 0 && semver.Compare(f, v) <= 0
}

// KeyDiff 是被替换的旧文件与新模板取值不同的一个键。
type KeyDiff struct {
	Key string
	Old any // 旧文件里的值，已换算成当前语义，可直接照填
	New any // 模板里的值；模板里没有这个键时为 nil
	// InTemplate 报告模板里有没有这个键（没有即重建后该键不再设置，例如旧文件里取消了注释的示例键）。
	InTemplate bool
}

// RebuildReport 是 ForceRebuild 的结果。
type RebuildReport struct {
	// Done 报告模板是否已放置为 config.yml。为假时本进程以已加载的配置继续运行，不因重建而重启。
	Done bool
	// Backup 是留在磁盘上的备份（改名得到的、被替换的那一份）的路径；文件没动时为空串。
	Backup string
	// DiffKnown 报告备份解析得了；为假时 Diffs 为空，提示里不列差异。
	DiffKnown bool
	// Diffs 是旧文件里写了、而取值与模板不同（或模板里没有）的键，模板里有的按模板的键序在前。
	Diffs []KeyDiff
	// BackupLegacy 报告备份里的 offset 是不是旧写法（正值=提前），按备份内容自己的版本键判断，不看名字：
	// 名字里的版本在文件被别的程序改过时可能不准。
	BackupLegacy bool
}

// ForceRebuild 把 config.yml 换成当前版本的模板，被替换的那一份改名为备份（设计 §5.4）。不询问用户。
//
// Load 与重建之间隔着隐私提示的等待与网关请求，其间 config.yml 可能被保存过：重建不假定文件还是 Load
// 留下的样子，也不做比对，此刻在位的是哪一份，备份里就是哪一份。新内容只在原位空着时放进去。
//
// 失败（写临时文件或改名失败，或放置时 config.yml 又被别的程序建了出来）时告警并给出人工补救的办法，
// 不自动重试：文件此时已盖上本进程的版本号，下次启动门槛不再成立（设计 §5.7，已接受）。
func (c *Config) ForceRebuild(programVersion string) RebuildReport {
	return forceRebuild(osFileOps, c, programVersion)
}

func forceRebuild(ops *fileOps, c *Config, programVersion string) RebuildReport {
	path := c.ConfigFile
	template := []byte(templateContent(StampVersion(programVersion)))
	res := replaceFile(ops, path, template, nil, c.diskVersionLabel(), true)
	rep := RebuildReport{Done: res.failure == replaceDone, Backup: res.backup}
	switch {
	case rep.Done:
		log.Info("Rebuilt %s from the template of this version; the previous file is kept as %s", path, res.backup)
		if data, err := ops.readFile(res.backup); err == nil {
			rep.Diffs, rep.BackupLegacy, rep.DiffKnown = diffAgainstTemplate(data, template, registeredPlayers)
		}
	case res.failure == replaceFileAppeared:
		log.Warn("Forced rebuild of %s was abandoned: the file was created anew by another program and is not overwritten; the file it replaced is kept as %s. The rebuild is not retried automatically; to rebuild, delete %s and restart", path, res.backup, path)
	case res.backup != "":
		log.Warn("Forced rebuild of %s could not write the new file (%v); the previous file is kept as %s, and a new one is generated from the template at the next start", path, res.err, res.backup)
	default:
		log.Warn("Forced rebuild of %s failed (%v); the file is unchanged and no backup was made. The rebuild is not retried automatically; to rebuild, delete %s and restart", path, res.err, path)
	}
	return rep
}

// diskVersionLabel 返回 config.yml 此刻在磁盘上的版本的写法，用作重建留下的备份名里的版本：
// Load 写成了即盖章值，否则仍是改写之前的版本。
func (c *Config) diskVersionLabel() string {
	if c.FileRewrite == RewriteStamped || c.FileRewrite == RewriteMigrated {
		return c.FileStamp
	}
	return c.FileVersionLabel
}

// diffAgainstTemplate 比较被替换的旧文件 old 与模板 template，返回旧文件里写了而与模板不同的键。
//
// 旧文件的取值一律先换算成当前语义：按它自己的版本键做一遍值层迁移（与 Load 用的是同一个函数）。
// Load 把迁移写成了的，它已是新写法，值层不再动它；文本层认不了而没写成的（如 offset: 0x1f4），它仍是
// 旧写法，在这里换算。列出的原值因此可直接照填。legacy 报告旧文件自己是不是旧写法。
// old 解析不了时 ok 为假。
func diffAgainstTemplate(old, template []byte, players []string) (diffs []KeyDiff, legacy, ok bool) {
	var was map[string]interface{}
	if yaml.Unmarshal(old, &was) != nil {
		return nil, false, false
	}
	var steps []migrationStep
	if version, state := readFileVersion(was); state != versionKeyInvalid {
		steps = stepsAfter(version)
	}
	was, _ = migrateValues(was, steps, players)

	var now map[string]interface{}
	var doc yaml.Node
	if yaml.Unmarshal(template, &now) != nil || yaml.Unmarshal(template, &doc) != nil {
		return nil, false, false
	}
	var order []string // 模板的键序
	if len(doc.Content) > 0 {
		for i := 0; i+1 < len(doc.Content[0].Content); i += 2 {
			order = append(order, doc.Content[0].Content[i].Value)
		}
	}
	var extra []string // 旧文件里有而模板里没有的键
	for key := range was {
		if _, in := now[key]; !in {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)

	for _, key := range append(order, extra...) {
		oldValue, written := was[key]
		newValue, in := now[key]
		if key == versionKey || !written || (in && reflect.DeepEqual(oldValue, newValue)) {
			continue
		}
		diffs = append(diffs, KeyDiff{Key: key, Old: oldValue, New: newValue, InTemplate: in})
	}
	return diffs, len(steps) > 0, true
}
