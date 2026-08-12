// Package clientid 生成本机的匿名客户端标识，并把它连同客户端环境打进出网请求头。
//
// # 它解决的问题
//
// 版本检查请求（main.checkAndUpdate）此前不带任何身份，UA 是 Go 默认的
// "Go-http-client/1.1"。网关侧只能按源 IP 计数，而主播基本都在 NAT / 动态 IP 后面：
// 同一台机器换了 IP 算成多台，同一出口的多台机器算成一台，DAU/MAU 两头都不准。
// 本包给出一个稳定、匿名、跨重装不变的标识，让网关能去重。
//
// # 为什么派生自 MachineGuid，而不是随机 UUID 落盘
//
// 随机 UUID 要在主播机器上新增一处持久化状态。本项目的第一约束是「绝不炸主播的机器」
// （AGENTS.md §0.1）——能不写就不写。MachineGuid 是 Windows 装机时生成的 128 位 GUID：
// 只读、普通用户可读、不需要管理员权限，换目录 / 重新下载 / 重装本程序都不变，
// 正好是我们要的那条身份。
//
// 代价要说清楚：粒度是「一台 Windows 装机」，不是「一个人」。
//
//	重装系统      → 算成一台新机器
//	同机多用户     → 算成一台
//	克隆的虚拟机   → 共用同一个 GUID，算成一台
//
// 对「今天有多少台播控机开过」这个问题，这个粒度是对的。要「多少个人」得有账号体系，
// 我们没有，也不打算为统计造一个。
//
// # 为什么必须哈希，且必须带域前缀
//
// MachineGuid 是真实的机器标识，原样发出去等于给每台机器发了张可跨服务关联的身份证。
// 我们要的只是「能不能去重」，不需要「是哪台」。
//
// SHA256(域前缀 + GUID) 取前 128 位同时满足两头：GUID 本身是随机 GUID，穷举不可行，
// 拿到哈希反推不出原值。域前缀让这个标识**只在本用途下有意义**——换个用途就换前缀，
// 两边算出来的哈希对不上，跨用途关联在机制上就不成立。
package clientid

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"golang.org/x/sys/windows/registry"

	"Metabox-Nexus-PlayerCap/logger"
)

var log = logger.New("ClientID")

// idDomain 是哈希的域前缀，见包注释「为什么必须哈希」。
//
// **改它 = 全体客户端换一批新 ID。** 网关侧会看到一次性的「老用户全部流失 + 等量新增」，
// 而且跨越这次改动的留存曲线永久断档。真要改，先想清楚报表怎么衔接。
const idDomain = "metabox-nexus-playercap/client-id/v1|"

// MachineGuid 的位置。只申请 QUERY_VALUE：本包**只读注册表，不写任何一个键**。
//
// 不需要 WOW64_64KEY —— 出货二进制是 windows/amd64（AGENTS.md §13.1），64 位进程看到的
// 本来就是 64 位视图。哪天真出了 32 位构建，这里要补上，否则会读到 Wow6432Node 下的另一个值。
const (
	machineGUIDPath = `SOFTWARE\Microsoft\Cryptography`
	machineGUIDName = "MachineGuid"
)

// readMachineGUID 做成变量是为了可注入——同 telemetry/sysinfo.go 的 rtlGetVersion 手法。
// 派生逻辑必须能在不依赖本机注册表的前提下被测，否则测试只是在复述本机的那一个值。
var readMachineGUID = readMachineGUIDFromRegistry

var (
	idOnce   sync.Once
	idCached string
)

// ID 返回本机的匿名客户端标识（32 位小写十六进制），读不到时返回空串。
//
// **读不到不编一个。** 退回随机值会让同一台机器每次启动都算成新设备，把 DAU 直接灌成
// 「启动次数」——那比缺一个样本坏得多，且错得看不出来（曲线只会变好看）。调用方拿到空串
// 就省掉那个请求头，网关侧看到的是「这条请求没有标识」，是个诚实的缺口。
//
// 结果进程内缓存：注册表值在进程生命周期内不会变，而心跳路径会按天反复取它。
func ID() string {
	idOnce.Do(func() {
		guid, err := readMachineGUID()
		if err != nil {
			log.Warn("读取 MachineGuid 失败: %v，本次运行不带客户端标识（不影响版本检查与自动更新）", err)
			return
		}
		idCached = derive(guid)
		if idCached == "" {
			log.Warn("MachineGuid 为空，本次运行不带客户端标识（不影响版本检查与自动更新）")
		}
	})
	return idCached
}

// derive 把 MachineGuid 派生成对外标识。纯函数——注册表进不来，因此可测。
//
// 先 trim 再转小写：注册表里的 GUID 带花括号与大小写差异（实测同一台机器上
// RegEdit 显示的与 API 读出的大小写可能不同）。不归一的话，同一台机器在不同读取路径下
// 会算出两个 ID，去重当场失效。
func derive(guid string) string {
	g := strings.ToLower(strings.TrimSpace(guid))
	if g == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(idDomain + g))
	// 取前 16 字节 = 128 位。够去重（碰撞概率在我们的量级上可以忽略），
	// 且请求头短一半——它会出现在网关的每一条访问日志里。
	return hex.EncodeToString(sum[:16])
}

func readMachineGUIDFromRegistry() (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, machineGUIDPath, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer key.Close()

	value, _, err := key.GetStringValue(machineGUIDName)
	if err != nil {
		return "", err
	}
	return value, nil
}
