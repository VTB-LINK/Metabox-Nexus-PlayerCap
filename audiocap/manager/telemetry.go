package manager

// 音频采集接入遥测的纯逻辑（issue #120，设计见 doc/design/audio-telemetry-design.md §3.2）：helper stderr
// 的有界尾部与脱敏、stderr 读泵、退出分类、三个上报事件的构造。本文件无构建约束、不 import telemetry
// （该包只在 Windows 上构建），保持 GOOS=linux 可编译；接线与默认上报函数在 manager_windows.go。

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// tailCapBytes 与 tailMaxLines 是 stderr_tail 的上限：缓冲保留最近 8 KiB，快照取其中最近至多 40 行。
	tailCapBytes = 8 << 10
	tailMaxLines = 40

	// statusControlCExit 是进程被系统默认的控制台事件处理结束时的退出码（STATUS_CONTROL_C_EXIT）。没有
	// signal.Notify 的 helper 在 Ctrl+C / Ctrl+Break 下以它退出（manager_windows_test.go 用真子进程实测），
	// 这是随主服务一起关停，不报。
	statusControlCExit uint32 = 0xC000013A

	// telemetryReportDelay 是上报的延后时长：触发时若主服务已开始关停（BeginShutdown / Stop）则丢弃，主服务
	// 在此期间退出则自然不发。覆盖信号处理置位之前、或无从置位（关闭控制台窗口、注销、关机）就被一并结束的 helper。
	telemetryReportDelay = 3 * time.Second
)

// 上报的 key（同时是 Sentry 的 fingerprint）与 helper_gave_up 的 cause 取值。key 稳定，不含版本号与播放器名。
const (
	keyHelperExited  = "audiocap.helper_exited_abnormally"
	keyHelperGaveUp  = "audiocap.helper_gave_up"
	keyCaptureGaveUp = "audiocap.capture_gave_up"

	causeQuickExit    = "quick_exit"
	causeSpawnFailure = "spawn_failure"
)

// tailBuffer 保留 helper stderr 最近 tailCapBytes 字节，并发安全（读泵写、上报时取快照）。
type tailBuffer struct {
	mu      sync.Mutex
	buf     []byte
	midLine bool // 发生过截断且截断点落在行中：buf 开头是一行的后半截
}

// Write 追加 p，超出 tailCapBytes 的最旧字节丢弃。恒返回 len(p)、nil。
func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - tailCapBytes; over > 0 {
		t.midLine = t.buf[over-1] != '\n'
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// Snapshot 返回最近至多 tailMaxLines 行，已脱敏（redactUserDirs）。截断点落在行中时先丢掉开头的残行
// （缓冲里没有换行则整段都是残行，返回空串）。nil 接收者返回空串（没有接管 stderr 的 helper）。
func (t *tailBuffer) Snapshot() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	s, mid := string(t.buf), t.midLine
	t.mu.Unlock()
	if mid {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		} else {
			s = ""
		}
	}
	return redactUserDirs(lastLines(s, tailMaxLines))
}

// lastLines 返回 s 的最后至多 n 行；末尾的换行不算新起一行。
func lastLines(s string, n int) string {
	end := len(s)
	if strings.HasSuffix(s, "\n") {
		end--
	}
	for i, count := end-1, 0; i >= 0; i-- {
		if s[i] == '\n' {
			if count++; count == n {
				return s[i+1:]
			}
		}
	}
	return s
}

// userDirPattern 匹配「<盘符>:\Users\<用户名>」：分隔符可为反斜杠或正斜杠、可连写（%q 转义后的路径），盘符与
// Users 不分大小写；用户名取到下一个分隔符、换行或 Windows 文件名中不允许的字符为止，可含空格与非 ASCII 字符。
var userDirPattern = regexp.MustCompile(`(?i)([a-z]:[\\/]+users[\\/]+)[^\\/\r\n<>:"|?*]+`)

// redactUserDirs 把 s 中用户目录里的用户名替换成 <user>。
func redactUserDirs(s string) string {
	return userDirPattern.ReplaceAllString(s, "${1}<user>")
}

// pumpStderr 把 r（helper stderr 管道的读端）读到 EOF 或出错为止，每块数据顺序做两件事：写入 tail，再同步
// 写 out（写错误忽略，继续读）。不丢、不乱序；out 阻塞时本函数随之阻塞，管道写满后阻塞落在 helper 自己的
// stderr 写入上，与 helper 直接继承控制台时相同。
func pumpStderr(r io.Reader, tail *tailBuffer, out io.Writer) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			tail.Write(buf[:n])
			out.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// exitInfo 是 helper 的退出信息。known 为假表示 demux 没走到 cmd.Wait（panic 被本地恢复），其余字段无意义；
// at 是进程真实的退出时刻（取不到时退回 demux 观察到退出的时刻），本机墙钟。
type exitInfo struct {
	known bool
	code  uint32
	at    time.Time
}

// classifyExit 判定一次退出是否上报 helper_exited_abnormally：退出信息已知、退出码不是 statusControlCExit，
// 且为自行退出——manager 从未主动终止它（killMarkedAt 为零），或它真实的退出时刻严格早于 manager 下手的时刻。
// 严格早于：Windows 上 time.Now() 与进程的 ExitTime 都按时钟中断步进（默认约 15.6ms），manager 下手与进程被杀
// 常落在同一个步进里、读数相等，那是 manager 自己杀的，不能算自行退出；代价只是漏报「恰死在下手同一步进内」的
// 自行退出，符合宁可漏报不误报。
// 退出码为 0 同样上报：helper 只在 stdin 读到 EOF 或出错时返回，manager 关 stdin 之前已记下 killMarkedAt，
// 除此之外的 0 退出同属预期之外。
func classifyExit(e exitInfo, killMarkedAt time.Time) bool {
	if !e.known || e.code == statusControlCExit {
		return false
	}
	return killMarkedAt.IsZero() || e.at.Before(killMarkedAt)
}

// TelemetryEvent 是 manager 交给遥测的一个「预期之外」事件，字段与 telemetry.ReportOnce 的参数一一对应。
// 只经下面三个构造函数产生（manager_windows.go 不就地写字面量，telemetrywiring_lint_test.go 守）。
type TelemetryEvent struct {
	Key, Message string
	Tags         map[string]string
	Extra        map[string]any
}

// exitCodeString 把退出码格式化为 0x 开头的大写十六进制。
func exitCodeString(code uint32) string {
	return fmt.Sprintf("0x%X", code)
}

// HelperExitedEvent 构造 helper 自行异常退出的事件：退出码、存活毫秒数、回收后的快速失败计数、stderr 尾部。
func HelperExitedEvent(exitCode uint32, alive time.Duration, quickFails int, stderrTail string) TelemetryEvent {
	return TelemetryEvent{
		Key:     keyHelperExited,
		Message: "音频采集 helper 自行退出",
		Tags:    map[string]string{"exit_code": exitCodeString(exitCode)},
		Extra: map[string]any{
			"alive_ms":    alive.Milliseconds(),
			"quick_fails": quickFails,
			"stderr_tail": stderrTail,
		},
	}
}

// HelperGaveUpEvent 构造 helper 累计快速失败而放弃本会话的事件。cause 为 causeQuickExit 或 causeSpawnFailure；
// hasLastExit 为真时附最近一个 helper 的退出码；spawnErr 非 nil 时附其文本（脱敏）；stderrTail 为最近一个
// helper 的 stderr 尾部。
func HelperGaveUpEvent(cause string, quickFails int, lastExitCode uint32, hasLastExit bool, spawnErr error, stderrTail string) TelemetryEvent {
	extra := map[string]any{
		"quick_fails": quickFails,
		"stderr_tail": stderrTail,
	}
	if hasLastExit {
		extra["last_exit_code"] = exitCodeString(lastExitCode)
	}
	if spawnErr != nil {
		extra["spawn_error"] = redactUserDirs(spawnErr.Error())
	}
	return TelemetryEvent{
		Key:     keyHelperGaveUp,
		Message: "音频采集 helper 累计快速失败，本会话放弃",
		Tags:    map[string]string{"cause": cause},
		Extra:   extra,
	}
}

// CaptureGaveUpEvent 构造单个播放器捕获连续失败而放弃的事件：播放器名、连续失败次数、当前 helper 的 stderr 尾部。
func CaptureGaveUpEvent(player string, failures int, stderrTail string) TelemetryEvent {
	return TelemetryEvent{
		Key:     keyCaptureGaveUp,
		Message: "播放器的音频捕获连续失败，已放弃",
		Tags:    map[string]string{"player": player},
		Extra: map[string]any{
			"failures":    failures,
			"stderr_tail": stderrTail,
		},
	}
}

// SampleTelemetryEvents 用与真实上报同一批构造函数、把可选字段都填上，返回每种事件各一个样本，供隐私提示的
// 门禁（telemetry 包的外部测试）核对采集面：构造函数新增 tag 或 extra 键，样本随之带上。
func SampleTelemetryEvents() []TelemetryEvent {
	return []TelemetryEvent{
		HelperExitedEvent(0xC0000005, 1500*time.Millisecond, 1, "sample"),
		HelperGaveUpEvent(causeSpawnFailure, maxQuickFails, 0xC0000005, true, fmt.Errorf("sample"), "sample"),
		CaptureGaveUpEvent("kugou", maxQuickFails, "sample"),
	}
}
