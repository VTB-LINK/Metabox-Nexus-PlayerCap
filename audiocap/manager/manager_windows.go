//go:build windows

package manager

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/audiocap/procscan"
	"Metabox-Nexus-PlayerCap/logger"
)

var managerLog = logger.New("AudioCap] [Manager") // 渲染为 [AudioCap] [Manager]

// QPC：空闲帧 TSMillis 必须与 helper 真帧同为系统级 QPC（开机起毫秒、系统级计数器 manager 与
// helper 读到的互相可比），否则 active→idle 切换时时间基倒退，违反 §4 / frame.Frame 的 TSMillis 契约。
// x/sys/windows 未封装 QueryPerformance*，照本仓库 helper/wasapi 的 NewLazySystemDLL 范式取 kernel32。
var (
	qpcKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	procQPC     = qpcKernel32.NewProc("QueryPerformanceCounter")
	procQPFreq  = qpcKernel32.NewProc("QueryPerformanceFrequency")
)

// readQPC 读一次 QueryPerformanceCounter（系统级计数器，与 helper.readQPC 同源）。
func readQPC() int64 {
	var c int64
	procQPC.Call(uintptr(unsafe.Pointer(&c)))
	runtime.KeepAlive(&c)
	return c
}

const (
	// reconcileInterval 是门控重算的秒级保守节拍（不亚秒）。切歌的即时性靠 Wake 唤醒，不靠缩短它。
	reconcileInterval = 1500 * time.Millisecond

	// helperFlag 是 helper 子命令标志（与 main.go 入口、孤儿识别同一真源）。
	helperFlag = "--audio-capture-helper"

	// 退避与看门狗常量（backoffBase / backoffCap / quickFailWindow / maxQuickFails / hangThreshold）
	// 在 gating.go，与纯判定同处。

	// idleKeepaliveInterval 是 idleKeepalive 推空闲帧的节拍，对齐静音期 keepalive 的 ~100ms：有订阅而
	// 无捕获时，root 与 per-player 订阅者照样每拍收到一帧，据 N=0 即知当前无捕获（无需订阅 /ws），走带器
	// 也按真实时间排空。推送条件见 idleKeepalive。
	idleKeepaliveInterval = 100 * time.Millisecond
)

// helperProc 是一个在跑的 helper 子进程控制柄。
type helperProc struct {
	cmd *exec.Cmd
	// stdin 必须被 Manager 持有引用：exec 的 StdinPipe 返回的 *os.File 带 finalizer，一旦无引用被 GC
	// 回收就会提前 Close 写端 → helper 误收 stdin EOF 而早退（实测踩过的坑）。存进本结构体即保活。
	stdin  io.WriteCloser
	job    windows.Handle // KILL_ON_JOB_CLOSE 的 job；0=未建成或未赋入（拆除退回 Process.Kill，主进程异常退出时仅剩 stdin EOF 防孤儿）
	exited chan struct{}  // 解复用 goroutine 在 stdout 收尾 + cmd.Wait 后 close
	start  time.Time      // spawn 时刻，用于快速失败判定
	// watchdogKill 为真表示本进程由看门狗判假死而杀：超时各路已逐播放器记账，reapHelper 不再计 helper
	// 快速失败、不退避。仅 Run 访问。
	watchdogKill bool
}

// Manager 监管唯一的音频 helper 子进程：门控计算、下发差异控制、退避重启、假死看门狗、stdout 解复用。
//
// 并发边界：helper / dispatched / dispatchedAt / captureFails / backoff / quickFails / givenUp /
// respawnAfter 全部只由 Run goroutine 单线程访问（reconcile 与看门狗都在 Run 上跑），无需锁。跨
// goroutine 共享的只有 lastFrame（解复用 goroutine 写、Run 的看门狗读写）与 lastForward（解复用
// goroutine 写、idleKeepalive 读），均由 mu 保护；demux 与 idleKeepalive 也在 mu 内向 sink 广播，使同一
// 播放器的真帧与空闲帧严格串行。
type Manager struct {
	sink         AudioSink
	status       func(name string) string       // 主服务 GetPlayerStatus：返回 "playing"/"paused"/"idle"/...
	normResolver func(player string) NormParams // 据 config 解析某家本轮归一化参数（下发 capture 时逐家查）
	frameRate    int                            // 出帧帧率（帧每秒），全局值，随每条 capture 下发
	exe          string                         // 本 exe 路径（spawn helper 与识别孤儿用）

	wakeCh   chan struct{} // 带缓冲=1：切歌唤醒立即重算门控（消切换空窗）
	stopCh   chan struct{}
	stopOnce sync.Once

	helper       *helperProc
	dispatched   map[string]uint32         // 已下发 capture 的 player→pid，做差异控制
	dispatchedAt map[string]time.Time      // 已下发 player→本次下发时刻，与 dispatched 同步增删；看门狗据此区分基线与真帧
	captureFails map[string]captureFailure // 逐播放器捕获失败记账；跨 helper 重启保留，PID 变化时清除
	backoff      time.Duration
	quickFails   int
	givenUp      bool
	respawnAfter time.Time // 退避窗口：此刻之前不重启 helper

	mu        sync.Mutex
	lastFrame map[string]time.Time // 每 player 最后收帧时刻（看门狗判假死）；下发 capture 时置为下发时刻作基线，reap / teardown 时重建
	// lastForward 是每 player 最近一次转发真帧的时刻（idleKeepalive 判无捕获）。只由 demux 写，不含下发
	// 基线，reap / teardown 时不清空。
	lastForward map[string]time.Time
}

// New 构造管理器。sink 为帧接收/门控能力（server 的 *audioHub），status 为主服务播放状态查询
// （GetPlayerStatus），normResolver 据 config 逐家解析归一化参数（下发 capture 时查），frameRate 为
// 出帧帧率（帧每秒，调用方已钳制到合法范围），exe 为本进程 exe 路径（os.Executable()，用于 spawn
// helper 与识别孤儿）。
func New(sink AudioSink, status func(name string) string, normResolver func(player string) NormParams, frameRate int, exe string) *Manager {
	return &Manager{
		sink:         sink,
		status:       status,
		normResolver: normResolver,
		frameRate:    frameRate,
		exe:          exe,
		wakeCh:       make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
		dispatched:   map[string]uint32{},
		dispatchedAt: map[string]time.Time{},
		captureFails: map[string]captureFailure{},
		lastFrame:    map[string]time.Time{},
		lastForward:  map[string]time.Time{},
	}
}

// Wake 非阻塞唤醒：向缓冲=1 的 wakeCh 投一个信号（已满则丢弃，多次唤醒合并成一次重算）。
// 由 SetActivePlayer 钩子在 r.mu 主线上调用（Task 11 接线），故必须非阻塞、绝不 panic。
func (m *Manager) Wake() {
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
}

// Stop 请求 Run 退出（幂等）。Run 收到即拆除 helper 并返回。
func (m *Manager) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
}

// Run 阻塞跑管理循环：秒级 ticker 或 Wake 或 helper 退出通知，先到即 reconcile；stopCh 关则拆
// helper 退出。起手做一次启动孤儿清扫。
//
// 起手 defer teardownHelper：panic 展开时同样拆除 helper，不让捕获残留到主进程退出；stop 路径
// 已拆除时它是空操作。不调 Stop：Run 被恢复后 idleKeepalive 仍按契约推空闲帧。此 defer
// 不 recover，panic 由调用方（main.go 的 localRecover）恢复并记日志。
func (m *Manager) Run() {
	defer m.teardownHelper()
	m.sweepOrphans()

	go m.idleKeepalive() // 无捕获时的空闲帧流（独立 goroutine，本地 recover，随 stopCh 退出）

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			m.teardownHelper()
			return
		case <-m.wakeCh:
		case <-ticker.C:
		case <-m.helperExitedCh():
			// helper 自行退出（崩溃/被杀）→ 唤醒立即进 reconcile 走 reap，不等下一 tick。
		}
		m.reconcile()
	}
}

// idleKeepalive 每 idleKeepaliveInterval 推一轮空闲帧（N=0，epoch、flags、rms、peak 均为 0，TSMillis 为
// 系统 QPC 毫秒），使 /audio-ws 与 /<player>/audio-ws 的订阅者在连接期间持续有帧：有捕获时为真帧或静音帧，
// 无捕获时每拍一帧空闲帧；捕获停止到首个空闲帧之间最多约 noCaptureAfter + idleKeepaliveInterval（约 400ms）无帧：
//
//	player=""：root 有订阅且无活跃播放器，只命中 root 订阅；
//	player=<名>：该播放器 HasSubscribers 为真（per-player 订阅，或 root 订阅且它是活跃播放器），且
//	           noCaptureAfter 内没有转发过它的真帧（进程未运行、捕获启动中、退避或已放弃），判定见 idlePlayers。
//
// 不触发任何 WASAPI 捕获（贵的捕获仍只按 desiredCaptures 跑）。独立 goroutine：只调线程安全的 sink 方法、
// 在 mu 内判定并广播按播放器的空闲帧、读 stopCh，不碰 Run-only 状态；本地 recover、绝不挂 Guard（§1）。
func (m *Manager) idleKeepalive() {
	defer func() {
		if r := recover(); r != nil {
			managerLog.Warn("[idle-keepalive] panic recovered locally: %v", r)
		}
	}()
	ticker := time.NewTicker(idleKeepaliveInterval)
	defer ticker.Stop()

	// QPC 频率开机后恒定，起手采一次。空闲帧 TSMillis 与 helper 真帧同用系统 QPC 与 frame.QPCMillis，
	// 二者时间基一致。
	var qpcFreq int64
	procQPFreq.Call(uintptr(unsafe.Pointer(&qpcFreq)))
	runtime.KeepAlive(&qpcFreq)
	// 空闲帧：rms=0、flags=0、epoch=0、无 bins；TSMillis 用系统 QPC（与真帧同基），让走带器按真实时间推进。
	idle := func(player string) []byte {
		return frame.Encode(nil, frame.Frame{Player: player, TSMillis: frame.QPCMillis(readQPC(), qpcFreq)})
	}

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			if m.sink.HasRootSubscriber() && m.sink.ActivePlayer() == "" {
				m.sink.BroadcastAudioFrame("", idle(""))
			}
			// 判定与广播同在 mu 内：demux 也须先取 mu 才能写 lastForward 并转发真帧，故某播放器的空闲帧
			// 不会排在它恢复后的首个真帧之后。锁序 m.mu → hub 锁；hub 不回调 Manager，无反向锁序。
			// defer 解锁：广播 panic 时不把 mu 留在锁定态，否则 demux 永久阻塞。
			func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				for _, p := range idlePlayers(procscan.Players, m.sink.HasSubscribers, m.lastForward, time.Now(), noCaptureAfter) {
					m.sink.BroadcastAudioFrame(p, idle(p))
				}
			}()
		}
	}
}

// helperExitedCh 返回当前 helper 的 exited 通道；无 helper 时返回 nil（在 select 中永久阻塞）。
// 只由 Run goroutine 读 m.helper，安全。
func (m *Manager) helperExitedCh() <-chan struct{} {
	if m.helper == nil {
		return nil
	}
	return m.helper.exited
}

// reconcile 是一轮门控 + 监管的全部动作，全程在 Run goroutine 上。
func (m *Manager) reconcile() {
	// 1. 回收已退出的 helper（崩溃/被看门狗杀），按退出原因记账退避（看门狗所杀的不退避）。
	if m.helper != nil && isClosed(m.helper.exited) {
		m.reapHelper()
	}

	// 2. 无任何订阅 → 拆 helper、直接返回（按需节能：无人看则不扫、不抓、helper 不留）。
	if !m.sink.AnySubscribers() {
		m.teardownHelper()
		return
	}

	// 3. 进程快照 + 门控计算。
	snap, err := procscan.Snapshot()
	if err != nil {
		managerLog.Warn("Process snapshot failed: %v", err)
		return
	}
	hasRoot := m.sink.HasRootSubscriber()
	playing := map[string]bool{}
	for _, p := range procscan.Players {
		if m.status(p) == "playing" {
			playing[p] = true
		}
	}
	desired := desiredCaptures(procscan.Players, hasRoot, playing, m.sink.HasSubscribers)

	// 4. desired → want = map[player]pid：键始终是播放器名，仅在此把播放器名映射到进程名去
	//    MainPID 求主进程、Alive 复核（防 PID 复用 TOCTOU）。
	want := map[string]uint32{}
	for player := range desired {
		proc := procscan.PlayerProcess[player]
		pid, ok := procscan.MainPID(snap, proc)
		if !ok || !procscan.Alive(pid, proc) {
			continue // 进程刚退出 / PID 已复用 → 本轮不抓，下轮再看
		}
		want[player] = pid
	}
	// 单路捕获失败的播放器：退避期内或已放弃的不下发；播放器已重启（PID 变化）的清账放行。
	filterWant(want, m.captureFails, time.Now())

	// 5. 没什么可抓且无 helper：空转（不为「有订阅但没在播」白起一个 helper 进程）。
	if len(want) == 0 && m.helper == nil {
		return
	}
	// 本会话已放弃（累计快速失败）：波形降级，不再 spawn。
	if m.givenUp {
		return
	}
	// 6. 确保 helper 在跑（退避窗口内不 spawn）。
	if m.helper == nil {
		if time.Now().Before(m.respawnAfter) {
			return
		}
		if err := m.spawnHelper(); err != nil {
			managerLog.Warn("Failed to start helper: %v", err)
			m.noteSpawnFailure()
			return
		}
	}

	// 7. 下发差异控制（新增/PID 变更 → capture；不再需要 → stop）。
	m.applyControls(want)

	// 8. 假死看门狗。
	m.checkWatchdog(want)
}

// applyControls 把 want 与已下发集合做 diff，向 helper 下发增量控制命令。
func (m *Manager) applyControls(want map[string]uint32) {
	for player, pid := range want {
		if old, ok := m.dispatched[player]; ok {
			if old == pid {
				continue // 已在捕获且 PID 未变
			}
			m.sendControl("stop " + player) // PID 变了（播放器重启）：先停旧
		}
		np := m.normResolver(player)
		if m.sendControl(fmt.Sprintf("capture %d %s %s %d %d %d", pid, player, bit(np.Enabled), np.Target, np.MaxGain, m.frameRate)) {
			now := time.Now()
			m.dispatched[player] = pid
			m.dispatchedAt[player] = now
			m.mu.Lock()
			m.lastFrame[player] = now // 下发即置基线，给 WASAPI 激活 + 首帧留看门狗宽限
			m.mu.Unlock()
		} else {
			delete(m.dispatched, player)
			delete(m.dispatchedAt, player)
		}
	}
	for player := range m.dispatched {
		if _, ok := want[player]; !ok {
			m.stopCapture(player)
		}
	}
}

// stopCapture 向 helper 下发 stop，并清除该 player 的逐下发状态（dispatched、dispatchedAt、
// lastFrame）。仅 Run 调用。这组状态另有几处同步点，新增逐下发状态须逐一照改：applyControls 的
// capture 成功分支（三者写入）与失败分支（删 dispatched、dispatchedAt）；spawnHelper 重建
// dispatched、dispatchedAt；reapHelper、teardownHelper 将二者置 nil 并重建 lastFrame。
func (m *Manager) stopCapture(player string) {
	m.sendControl("stop " + player)
	delete(m.dispatched, player)
	delete(m.dispatchedAt, player)
	m.mu.Lock()
	delete(m.lastFrame, player)
	m.mu.Unlock()
}

// bit 把布尔转为控制命令里的 "1"/"0" 位。
func bit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// sendControl 向 helper stdin 写一行控制命令。写失败（helper 已退出）返回 false——退出会经 exited
// 通道被下一轮 reconcile 的 reap 处理，这里只如实回报本次是否送达。
func (m *Manager) sendControl(line string) bool {
	if m.helper == nil {
		return false
	}
	if _, err := io.WriteString(m.helper.stdin, line+"\n"); err != nil {
		managerLog.Warn("Failed to send control (helper may have exited): %v", err)
		return false
	}
	return true
}

// checkWatchdog 是帧活性看门狗：在 mu 内快照 want 中各 player 的 lastFrame，锁外经 judgeWatchdog
// 判定（与播放状态无关）。先清除已跑稳 player 的失败记账；再对持续 hangThreshold 无帧的 player
// 逐个记失败：helper 仍有其他路出真帧时只停这几路、helper 不动；否则判 helper 假死，kill 重启。
func (m *Manager) checkWatchdog(want map[string]uint32) {
	now := time.Now()
	rows := make([]frameSnap, 0, len(want))
	m.mu.Lock()
	for player := range want {
		last, ok := m.lastFrame[player]
		rows = append(rows, frameSnap{player: player, last: last, has: ok, dispatchedAt: m.dispatchedAt[player]})
	}
	m.mu.Unlock()

	for _, r := range rows {
		if _, ok := m.captureFails[r.player]; ok && captureStable(r.dispatchedAt, r.last) {
			delete(m.captureFails, r.player)
		}
	}

	stale, killHelper := judgeWatchdog(rows, now)
	for _, player := range stale {
		m.noteCaptureFailure(player, want[player], now) // 按本次下发时刻记账，须先于下方 stopCapture 删除 dispatchedAt
		if !killHelper {
			// helper 仍在出帧：只停这一路（helper 的 active 表须先 stop 才能重新 capture），helper 不动。
			m.stopCapture(player)
		}
	}
	if killHelper {
		managerLog.Warn("%d capture(s) produced no frames for %v, and no other capture has produced a frame since this dispatch; treating the helper as hung, killing and restarting it", len(stale), hangThreshold)
		m.helper.watchdogKill = true
		m.killHelperForRestart()
	}
}

// noteCaptureFailure 按本次下发时刻给 player 记一次捕获失败（pid 为其当前 PID）并打日志。仅 Run 调用。
func (m *Manager) noteCaptureFailure(player string, pid uint32, now time.Time) {
	f := recordCaptureFailure(m.captureFails[player], now, m.dispatchedAt[player], pid)
	m.captureFails[player] = f
	if f.givenUp {
		managerLog.Warn("Capture for player %q failed %d times in a row; giving up its waveform until its process restarts. Other players are unaffected", player, f.count)
		return
	}
	managerLog.Warn("Player %q produced no frames for %v; recording capture failure #%d, retrying in %v", player, hangThreshold, f.count, f.retryAt.Sub(now))
}

// spawnHelper 拉起 helper 子进程、建 Job Object 并赋入、起 stdout 解复用 goroutine。
func (m *Manager) spawnHelper() error {
	cmd := exec.Command(m.exe, helperFlag)
	cmd.Stderr = os.Stderr // helper 诊断经 stderr 继承可见（§0.5）
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	// Job Object：KILL_ON_JOB_CLOSE，父死 → 子随内核回收。best-effort——建/赋失败不致命，只记日志、
	// helper 照跑。赋入失败时 helper 不在 Job 内、关句柄杀不掉它，故关闭 job 并保持 0，令
	// killHelperForRestart / teardownHelper / reapHelper 退回 Process.Kill。
	var job windows.Handle
	if j, jerr := createKillOnCloseJob(); jerr != nil {
		managerLog.Warn("Failed to create Job Object (orphan protection falls back to stdin EOF + Process.Kill): %v", jerr)
	} else {
		var aerr error
		h, herr := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		if herr != nil {
			aerr = fmt.Errorf("取 helper 进程句柄失败: %w", herr)
		} else {
			if err := assignToJob(j, h); err != nil {
				aerr = fmt.Errorf("assignToJob 失败: %w", err)
			}
			windows.CloseHandle(h)
		}
		if aerr != nil {
			windows.CloseHandle(j)
			managerLog.Warn("Helper not assigned to the Job Object (orphan protection falls back to stdin EOF + Process.Kill): %v", aerr)
		} else {
			job = j
		}
	}

	hp := &helperProc{cmd: cmd, stdin: stdin, job: job, exited: make(chan struct{}), start: time.Now()}
	m.helper = hp
	m.dispatched = map[string]uint32{}
	m.dispatchedAt = map[string]time.Time{}

	go m.demux(hp, stdout)

	managerLog.Info("Audio capture helper started (pid=%d)", cmd.Process.Pid)
	return nil
}

// demux 是 stdout 解复用 goroutine：增量解帧 → 记最后收帧与转发时刻 → 重编码为自包含帧交 hub 扇出。
// 本地 recover 绝不挂 telemetry.Guard（§1：音频子系统 panic 隔离，不拖垮主线）；helper 输出当
// 不可信数据（坏帧由 Decoder 按 magic 重对齐自跳过）。读到 EOF/错误后 cmd.Wait 回收、close(exited)。
func (m *Manager) demux(hp *helperProc, stdout io.Reader) {
	defer close(hp.exited)
	defer func() {
		if r := recover(); r != nil {
			managerLog.Warn("[demux] panic recovered locally: %v", r)
		}
	}()

	var dec frame.Decoder
	buf := make([]byte, 32*1024)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			dec.Push(buf[:n])
			for {
				f, ok, derr := dec.Next()
				if derr != nil {
					managerLog.Detail("Dropped bad frame: %v", derr) // Decoder 已跳过，继续
					continue
				}
				if !ok {
					break // 需要更多字节
				}
				// 解码拿 player 路由、再编码成自包含帧转发（与真 /audio-ws「一帧一消息」一致）。
				// 每帧新分配：Encode 返回的切片会被 hub 存入多个订阅通道，不能复用缓冲。
				payload := frame.Encode(nil, f)
				// 转发与记时刻同在 mu 内：idleKeepalive 也在 mu 内判定并广播空闲帧，二者对同一播放器的发送
				// 严格串行，空闲帧不会排在恢复后的真帧之后。时刻取广播返回之后：广播本身若拖延超过
				// noCaptureAfter，取广播前的时刻会让 idleKeepalive 随即补一帧空闲帧。锁序 m.mu → hub 锁，与
				// idleKeepalive 一致。defer 解锁：广播 panic 时不把 mu 留在锁定态。
				func() {
					m.mu.Lock()
					defer m.mu.Unlock()
					m.sink.BroadcastAudioFrame(f.Player, payload)
					now := time.Now()
					m.lastFrame[f.Player] = now
					m.lastForward[f.Player] = now
				}()
			}
		}
		if err != nil {
			if err != io.EOF {
				managerLog.Detail("Reading helper stdout ended: %v", err)
			}
			break
		}
	}
	hp.cmd.Wait() // EOF 后再 Wait（os/exec 契约），回收僵尸；退出码这里不需要
}

// reapHelper 回收 exited 已 close 的 helper，记账退避 / 快速失败放弃；看门狗所杀的不计快速失败、
// 不退避。仅 Run 调用。
//
// exited 关闭不等于进程已退出：demux panic 被本地恢复时同样 close(exited)，进程可能仍在跑。故回收
// 前先确保终止：关 stdin；job 非 0 时关 job 句柄（KILL_ON_JOB_CLOSE 强杀），否则 Process.Kill，
// 进程已退出时的错误忽略。stdin、cmd、cmd.Process 为 nil 时跳过对应步骤。
func (m *Manager) reapHelper() {
	hp := m.helper
	m.helper = nil
	m.dispatched = nil
	m.dispatchedAt = nil
	if hp.stdin != nil {
		hp.stdin.Close()
	}
	if hp.job != 0 {
		windows.CloseHandle(hp.job)
	} else if hp.cmd != nil && hp.cmd.Process != nil {
		hp.cmd.Process.Kill()
	}
	m.mu.Lock()
	m.lastFrame = map[string]time.Time{}
	m.mu.Unlock()

	if time.Since(hp.start) >= quickFailWindow {
		m.quickFails = 0 // 稳定跑过一段 → 重置快速失败计数
		m.backoff = 0    // 同时复位退避：跑稳过的 helper 偶发孤立崩溃从 base 重新退避，不被历史棘轮钉死（仅 Run 访问，无需锁）
	} else if !hp.watchdogKill {
		m.quickFails++
	}
	if hp.watchdogKill {
		// 看门狗所杀：只有超时各路已逐播放器记账，其重试由各自 retryAt 经 filterWant 控制；同被杀但未超时
		// 的路不记账，在本轮 reconcile 随新 helper 立即恢复。不计 helper 快速失败、不抬退避、不推迟 respawnAfter。
		managerLog.Warn("Helper terminated by the watchdog (alive for %v); no backoff, may restart immediately", time.Since(hp.start).Round(time.Millisecond))
		return
	}
	if m.quickFails >= maxQuickFails {
		m.givenUp = true
		managerLog.Warn("Helper has failed fast %d times (spawn failure or exit within %v of starting; watchdog restarts and on-demand teardowns not counted); giving up audio capture for this session (waveform degraded) until the main service restarts", m.quickFails, quickFailWindow)
		return
	}
	m.bumpBackoff()
	m.respawnAfter = time.Now().Add(m.backoff)
	managerLog.Warn("Helper exited (alive for %v); restarting in %v", time.Since(hp.start).Round(time.Millisecond), m.backoff)
}

// noteSpawnFailure 处理 spawn 本身失败（exec 起不来）：同样计快速失败并退避。
func (m *Manager) noteSpawnFailure() {
	m.quickFails++
	if m.quickFails >= maxQuickFails {
		m.givenUp = true
		managerLog.Warn("Helper has failed fast %d times (spawn failure or exit within %v of starting; watchdog restarts and on-demand teardowns not counted); giving up audio capture for this session (waveform degraded) until the main service restarts", m.quickFails, quickFailWindow)
		return
	}
	m.bumpBackoff()
	m.respawnAfter = time.Now().Add(m.backoff)
}

// bumpBackoff 指数退避、封顶 backoffCap。
func (m *Manager) bumpBackoff() {
	if m.backoff == 0 {
		m.backoff = backoffBase
	} else {
		m.backoff *= 2
		if m.backoff > backoffCap {
			m.backoff = backoffCap
		}
	}
}

// killHelperForRestart 杀掉假死 helper，不阻塞 Run：关 stdin + 关 Job（KILL_ON_JOB_CLOSE 强杀）
// → 进程死 → stdout EOF → demux close(exited) → 下一轮 reconcile 的 reapHelper 回收，同轮即可重启（不退避）。
func (m *Manager) killHelperForRestart() {
	if m.helper == nil {
		return
	}
	m.helper.stdin.Close()
	if m.helper.job != 0 {
		windows.CloseHandle(m.helper.job)
		m.helper.job = 0 // 置 0，避免 reapHelper 二次关闭
	} else {
		m.helper.cmd.Process.Kill() // 无 Job（未建成或未赋入）时退回 Process.Kill
	}
}

// teardownHelper 优雅拆除 helper（无订阅 / stopCh / Run panic 展开）：关 stdin 触发其 stdin-EOF 自杀
// （Task 6 已验），关 Job 兜底强杀，等解复用 goroutine 收尾；存活满 quickFailWindow 的 helper 同样
// 清零快速失败计数与退避。无 helper 时为空操作。仅 Run 调用，可阻塞（非高频）。
func (m *Manager) teardownHelper() {
	if m.helper == nil {
		return
	}
	hp := m.helper
	m.helper = nil
	m.dispatched = nil
	m.dispatchedAt = nil

	hp.stdin.Close()
	if hp.job != 0 {
		windows.CloseHandle(hp.job)
	} else {
		hp.cmd.Process.Kill() // 无 Job（未建成或未赋入）兜底强杀，防 helper 卡死令下方 <-hp.exited 永久阻塞 Run
	}
	<-hp.exited // demux 在 stdout 收尾 + cmd.Wait 后 close
	if time.Since(hp.start) >= quickFailWindow {
		m.quickFails = 0 // 与 reapHelper 的稳定分支一致，计数不跨订阅会话累积
		m.backoff = 0
	}

	m.mu.Lock()
	m.lastFrame = map[string]time.Time{}
	m.mu.Unlock()
}

// sweepOrphans 启动孤儿清扫（best-effort）。Job Object + stdin-EOF 自杀已覆盖「父死→子亡」的
// 常规场景；本清扫只兜底「上次主服务在赋 Job 前就崩」的窄窗残留 helper。
//
// 识别：同名于本 exe（helper 是同一 exe 的 --audio-capture-helper 子命令，进程名与主服务相同，
// Toolhelp 快照只给名区分不开）且命令行含 helperFlag、且 PID≠自身。用已在依赖的 gopsutil 取
// 命令行（不为它引 WMI 等重依赖）。管理器在主服务成功绑定端口后才起（Task 11），端口独占保证此刻
// 不存在第二个主服务实例，故命令行含 helperFlag 的同名进程必是遗留 helper、清杀安全。
func (m *Manager) sweepOrphans() {
	self := os.Getpid()
	exeName := filepath.Base(m.exe)
	procs, err := process.Processes()
	if err != nil {
		return
	}
	for _, p := range procs {
		if int(p.Pid) == self {
			continue
		}
		name, err := p.Name()
		if err != nil || !strings.EqualFold(name, exeName) {
			continue
		}
		cmdline, err := p.Cmdline()
		if err != nil || !strings.Contains(cmdline, helperFlag) {
			continue
		}
		managerLog.Warn("Cleaning up leftover orphaned audio helper process pid=%d", p.Pid)
		p.Kill()
	}
}

// isClosed 非阻塞判定一个 struct{} 通道是否已关闭。
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
