package config

import (
	"strings"

	"golang.org/x/mod/semver"
)

// config.yml 的版本键（issue #32）。它由程序维护，不是配置项：没有命令行参数，不经 mergeYAML，不进
// /service-status 的配置三件套。设计见 doc/design/config-version-and-offset-delay-design.md §2。
const versionKey = "config-version"

const (
	// legacyFileVersion 是没有版本键的 config.yml 的版本：3.x 及更早的程序从不写版本键，一律按 v3 对待。
	legacyFileVersion = "v3.0.0"

	// latestMigrationBoundary 是最新一次配置语义变更的边界：offset 改为延迟语义（issue #54）。它也是
	// 开发构建的盖章值，见 StampVersion。边界取「首个含该变更的版本」的最低预发布 X.Y.Z-0：4.0.0 的
	// 任何预发布与正式版都不低于 4.0.0-0。写成 4.0.0 的话，由 4.0.0-beta.3 盖过版本号的文件仍低于边界。
	latestMigrationBoundary = "v4.0.0-0"
)

// normalizeVersion 把版本号写成带 v 前缀的形式（去首尾空白），空串原样返回。同 main 包的 normalizeSemver。
func normalizeVersion(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// isReleaseVersion 报告 version 是不是发布版本：合法的 semver 且不是 0.0.0。判据同 main 包的
// isReleaseVersion（根包 stampversion_test.go 逐例比对两者）：本地构建的 0.0.0 与 CI 开发构建的非 semver
// 版本号都不是。
func isReleaseVersion(version string) bool {
	v := normalizeVersion(version)
	return v != "" && v != "v0.0.0" && semver.IsValid(v)
}

// StampVersion 返回程序写进版本键的值（「盖章值」），programVersion 是程序版本（main.Version）。
//
// 发布版本：版本号的规范形式，带 v、保留预发布后缀，如 v4.0.0、v4.0.0-beta.3。
// 开发构建：latestMigrationBoundary，含义是「到这一步为止的迁移已做」。开发构建不能照写 0.0.0：它低于
// 任何迁移边界，下次启动会把已迁移的文件再迁一遍。之后由发布版本读到时，边界低于任何 4.0.0 的预发布
// 与正式版，照常改写为发布版本号且不再迁移。
func StampVersion(programVersion string) string {
	if isReleaseVersion(programVersion) {
		return normalizeVersion(programVersion)
	}
	return latestMigrationBoundary
}

// versionKeyState 是版本键在 config.yml 里的状态。
type versionKeyState int

const (
	versionKeyAbsent  versionKeyState = iota // 没有版本键
	versionKeyValid                          // 合法的版本号
	versionKeyInvalid                        // 键在，但不是合法的版本号
)

// readFileVersion 从解析结果 m 读出文件版本（semver 的规范形式）与版本键的状态。
//
// 带不带 v 都认，并容忍大写 V 与首尾空白（有无引号由 YAML 解析抹平）；x/mod/semver 接受的两段、一段
// 简写（"4.0"、"4"）按补零后的版本对待。没有版本键时返回 legacyFileVersion。
//
// 键在而不是合法版本号（写成数字、空值、任意文字、布尔、列表、映射）时返回空串与 versionKeyInvalid，
// 调用方须把它当作「已由 4.0 以上的程序处理过」：不迁移，只把该键改写为盖章值。3.x 从不写这个键，
// 键在即说明文件经过新程序或人手；此时按 v3 再迁一遍会把已取反的 offset 再取反，而且没有任何报错。
func readFileVersion(m map[string]interface{}) (string, versionKeyState) {
	raw, ok := m[versionKey]
	if !ok {
		return legacyFileVersion, versionKeyAbsent
	}
	if s, isString := raw.(string); isString {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "V") {
			s = "v" + s[1:]
		}
		if v := normalizeVersion(s); semver.IsValid(v) {
			return semver.Canonical(v), versionKeyValid
		}
	}
	return "", versionKeyInvalid
}

// upgradePlan 是 loadFile 对一份已解析的 config.yml 定下的处置：文件版本与盖章值比较的结果。
type upgradePlan struct {
	fileVersion string          // 改写之前的文件版本 S0；版本键非法时为空串
	keyState    versionKeyState // 版本键的状态
	stamp       string          // 盖章值 P
	stampKey    bool            // 版本键要改写为 P
	migrate     bool            // 执行边界高于 fileVersion 的各步迁移（可能为零步）
	newer       bool            // 文件版本高于盖章值：不写文件、不迁移
}

// planUpgrade 按文件版本 S0 与盖章值 P 的关系定处置：
//
//	S0 < P    执行边界高于 S0 的各步迁移，版本键写为 P
//	S0 = P    不动（按 semver 比较，写法不同而版本相同的不为此写文件）
//	S0 > P    不动；把版本键改低的话，用户再升回去会重迁
//	版本键非法 不迁移，版本键写为 P
func planUpgrade(m map[string]interface{}, stamp string) upgradePlan {
	version, state := readFileVersion(m)
	p := upgradePlan{fileVersion: version, keyState: state, stamp: stamp}
	switch {
	case state == versionKeyInvalid:
		p.stampKey = true
	case semver.Compare(version, stamp) < 0:
		p.stampKey, p.migrate = true, true
	case semver.Compare(version, stamp) > 0:
		p.newer = true
	}
	return p
}
