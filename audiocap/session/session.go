// Package session 在音频捕获 helper 内给每路捕获探测 WASAPI 音频会话信号：目标进程树在某个活跃渲染端点
// 上有没有 Active 的会话，结果经帧头 flags 的 FlagSessionKnown / FlagSessionActive 给出（见
// doc/design/audio-session-signal-design.md §4）。
//
// 本文件只放无平台依赖的逻辑（进程树成员判定、一拍结果的发布规则、per-capture 槽位、watcher 的编排与
// 诊断限频），CI 可跑测试；会话枚举的 COM 调用在 session_windows.go（//go:build windows）。
package session

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/audiocap/procscan"
)

const (
	// probeInterval 是两拍探测之间的间隔（设计 §4.4）。前端的判据窗口是 10 秒，一拍的延迟可忽略。
	probeInterval = time.Second

	// resultTTL 是一次探测结果的存活期，约三拍：超过它没有新结果，帧回到 (0,0) 未知。
	resultTTL = 3 * time.Second

	// procRefresh 是进程快照（PID → PPID）的缓存期限。会话枚举每拍约 3 ms，而取一次进程快照约 18 ms（真机实测，
	// 约 40 个进程以上的机器随进程数增长），逐拍重取会占去整拍 CPU 的八成；进程树关系变化很慢，缓存期内沿用，
	// 其间出现缓存里没有的 Active 会话 PID（新起的进程）时立即重取。
	procRefresh = 5 * time.Second

	// maxTreeDepth 是自会话 PID 沿 PPID 链上溯的步数上限，兼作成环的 PPID 链的终止条件。
	maxTreeDepth = 16

	// diagInterval 是两条「探测不完整」诊断之间的最短间隔，首次出现即报。
	diagInterval = 60 * time.Second
)

// Slot 是一次 capture 的会话信号槽位：每条 capture 命令新建一个，stop 或同名替换时摘除，不按播放器名复用。
// 旧 PID 的在途探测只会写回已摘除的旧槽位，新槽位在自己的首次有效探测之前保持未知（设计 §4.3）。
// 零值可用，目标 PID 为 0（不匹配任何会话）。读写无锁，可跨 goroutine 并发使用。
type Slot struct {
	pid uint32
	res atomic.Pointer[result]
}

// result 是一次已发布的探测结果。at 用 time.Time 而非整数纳秒，保留单调钟读数，墙钟调整不影响存活期判定。
type result struct {
	active bool
	at     time.Time
}

// Publish 写入一次探测结果：active 为目标进程树内有 Active 会话，at 为该结果的时刻。生产路径只由 Watcher
// 调用；导出供 helper 的测试构造槽位状态。
func (s *Slot) Publish(active bool, at time.Time) {
	s.res.Store(&result{active: active, at: at})
}

// Flags 返回本槽位此刻应并入帧头的会话标志：从未发布、或最近一次结果距 now 超过 resultTTL 时为 0；否则为
// FlagSessionKnown，结果为有 Active 会话时再加 FlagSessionActive。不会只返回 FlagSessionActive。
// nil 槽位返回 0。
func (s *Slot) Flags(now time.Time) uint32 {
	if s == nil {
		return 0
	}
	r := s.res.Load()
	if r == nil || now.Sub(r.at) > resultTTL {
		return 0
	}
	if r.active {
		return frame.FlagSessionKnown | frame.FlagSessionActive
	}
	return frame.FlagSessionKnown
}

// inTree 报告 pid 是否为 root 或其后代：自 pid 沿 parent（PID → PPID）上溯，至多 maxTreeDepth 步内到达 root
// 即是。PID 0（系统声音与跨进程会话）不计；PPID 链断开（某一级不在快照里）视为不在树内；成环的链由步数
// 上限终止。
//
// shortcut: Toolhelp 的 PPID 不随父进程退出而更新，父进程退出后其 PID 被目标复用时，旧子进程会被误算进树；
// 快照不含创建时间，未比对。与捕获的进程树口径同类、概率极低，要排除时改为逐级比对进程创建时间。
func inTree(parent map[uint32]uint32, root, pid uint32) bool {
	cur := pid
	for range maxTreeDepth {
		if cur == 0 {
			return false
		}
		if cur == root {
			return true
		}
		p, ok := parent[cur]
		if !ok {
			return false
		}
		cur = p
	}
	return cur != 0 && cur == root
}

// hrNoSingleProcess 是 AUDCLNT_S_NO_SINGLE_PROCESS（0x0889000D）：IAudioSessionControl2::GetProcessId 对跨进程会话
// 返回的**成功**码，此时返回的 PID 只是最初创建会话的进程，不能证明当前出声的流属于它（Microsoft 文档）。
const hrNoSingleProcess = 0x0889000D

// pidProblem 判一次 GetProcessId 的结果能否用来把 Active 会话归属到某个进程：nil 表示 PID 可信；调用失败，或
// 跨进程会话（系统声音会话除外：它不属于任何进程，由 system 字段排除，不算问题）返回非 nil，调用方把它记为
// sessionInfo.pidErr，本拍按不完整处理（设计 §4.2.1）。
func pidProblem(hr uint32, system bool) error {
	switch {
	case hr&0x80000000 != 0:
		return fmt.Errorf("GetProcessId hr=0x%08X", hr)
	case hr == hrNoSingleProcess && !system:
		return fmt.Errorf("GetProcessId 报告跨进程会话（AUDCLNT_S_NO_SINGLE_PROCESS），无法确定当前出声的进程")
	}
	return nil
}

// endpoint 是一拍里一个活跃渲染端点的会话枚举结果。
type endpoint struct {
	err      error         // Activate、GetSessionEnumerator 或 GetCount 失败：该端点上的会话不可知
	sessions []sessionInfo // err 为 nil 时有效
}

// sessionInfo 是一个会话的查询结果。
type sessionInfo struct {
	err    error  // GetSession 或 GetState 失败：状态不可知
	active bool   // AudioSessionStateActive
	pidErr error  // Active 会话的 QueryInterface(IAudioSessionControl2) 或 GetProcessId 失败
	pid    uint32 // 会话所属进程；0 为系统声音或跨进程会话
	system bool   // IsSystemSoundsSession 为真
}

// Enumerator 做一次全部活跃渲染端点的会话枚举。返回的 error 表示整拍无从枚举（如创建设备枚举器或
// EnumAudioEndpoints 失败）；单个端点或会话的失败记在对应的 endpoint / sessionInfo 里。
type Enumerator interface {
	enumerate() ([]endpoint, error)
}

// scan 汇总一拍的端点枚举：pids 为全部 Active、取到 PID 且非系统声音的会话的 PID，problems 为使本拍
// 不完整的各项失败，为空即枚举完整（设计 §4.2.1）。Inactive 与 Expired 的会话不计，其 PID 查询失败也不影响
// 完整性；Active 会话取不到 PID 则无从判断它是否在目标树内，本拍不完整。
func scan(eps []endpoint) (pids []uint32, problems []string) {
	for i, ep := range eps {
		if ep.err != nil {
			problems = append(problems, fmt.Sprintf("端点 #%d: %v", i, ep.err))
			continue
		}
		for j, s := range ep.sessions {
			switch {
			case s.err != nil:
				problems = append(problems, fmt.Sprintf("端点 #%d 会话 #%d: %v", i, j, s.err))
			case !s.active:
			case s.pidErr != nil:
				problems = append(problems, fmt.Sprintf("端点 #%d 会话 #%d（Active）: %v", i, j, s.pidErr))
			case !s.system:
				pids = append(pids, s.pid)
			}
		}
	}
	return pids, problems
}

// decide 按设计 §4.2.1 判一个目标：pids 中有 root 树内的 PID 即为正结果，照常发布，不论本拍是否完整；
// 否则只在枚举完整时发布负结果，不完整时不发布（publish 为 false），让槽位按存活期自然过期。
// 不能跳过失败的端点、用其余端点的空结果发布负结果：目标唯一的 Active 会话可能恰在失败的端点上。
//
// 目标退出后其 PID 被别的进程复用时，至多一拍内可能把那个进程的会话算进来（设计 R6）；捕获随目标退出被
// 主服务拆除，不另作处理。
func decide(pids []uint32, complete bool, parent map[uint32]uint32, root uint32) (active, publish bool) {
	for _, pid := range pids {
		if inTree(parent, root, pid) {
			return true, true
		}
	}
	return false, complete
}

// Watcher 是 helper 内唯一的会话探测循环：持有被监视的槽位集合，每拍对集合里的全部槽位做一次进程快照与
// 全端点会话枚举，按 decide 写回各槽位。集合为空时阻塞，不探测。用 NewWatcher 构造；Add、Remove、Stop
// 可在任意 goroutine 调用，Run 只在一个 goroutine 上调用一次。
type Watcher struct {
	snapshot func() ([]procscan.Proc, error)
	report   func(string)

	mu    sync.Mutex
	slots map[*Slot]struct{}
	wake  chan struct{} // 容量 1：Add 非阻塞唤醒在空集合上等待的 Run
	stop  chan struct{}
	once  sync.Once

	// 进程树缓存与诊断限频，只由 Run 所在 goroutine 读写。
	parent   map[uint32]uint32 // PID → PPID；nil 即尚未取过
	parentAt time.Time         // 上次取快照的时刻

	diagAt  time.Time // 上次报出的时刻
	diagged bool      // 是否报出过
	failed  int       // 自上次报出以来不完整的拍数
}

// NewWatcher 构造 watcher：snapshot 取一次进程快照（PID 与 PPID），report 投递一条诊断（须非阻塞，失败
// 即丢弃该条）。
func NewWatcher(snapshot func() ([]procscan.Proc, error), report func(string)) *Watcher {
	return &Watcher{
		snapshot: snapshot,
		report:   report,
		slots:    map[*Slot]struct{}{},
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
}

// Add 为目标 PID 新建一个槽位并登记进监视集合，返回该槽位。
func (w *Watcher) Add(pid uint32) *Slot {
	s := &Slot{pid: pid}
	w.mu.Lock()
	w.slots[s] = struct{}{}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return s
}

// Remove 把槽位摘出监视集合。在途的探测仍可能写回它，但它已不属于任何在跑的捕获。
func (w *Watcher) Remove(s *Slot) {
	w.mu.Lock()
	delete(w.slots, s)
	w.mu.Unlock()
}

// Stop 通知 Run 返回：正在等待时立即返回，正在探测时于本拍结束后返回。可重复调用。
func (w *Watcher) Stop() {
	w.once.Do(func() { close(w.stop) })
}

// Run 是探测循环，直到 Stop。enum 须在调用 Run 的这个 goroutine 上可用（Windows 下为已 LockOSThread 且已
// CoInitializeEx 的线程）。集合为空时阻塞在唤醒与停止上，不探测、不起计时器；每拍之后等 probeInterval。
func (w *Watcher) Run(enum Enumerator) {
	for {
		slots := w.pending()
		if len(slots) == 0 {
			select {
			case <-w.wake:
				continue
			case <-w.stop:
				return
			}
		}
		w.probe(enum, slots)
		t := time.NewTimer(probeInterval)
		select {
		case <-t.C:
		case <-w.stop:
			t.Stop()
			return
		}
	}
}

// pending 持锁拷出当前的槽位。探测在锁外做，且只写拷出的这些槽位：其间摘除或新登记的槽位都不受本拍影响。
func (w *Watcher) pending() []*Slot {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*Slot, 0, len(w.slots))
	for s := range w.slots {
		out = append(out, s)
	}
	return out
}

// probe 做一拍探测并写回 slots。结果的时刻取本拍开始时：枚举本身若拖延，按更早的时刻计龄，偏向过期。
// 进程快照沿用缓存（见 parents），取在会话枚举之后：重取时持有会话的进程在枚举时已存在，快照一定包含它，除非
// 它在其间退出（按断链计）。
// 枚举整体失败或快照失败时本拍失败，不写任何槽位（设计 §4.5）。
func (w *Watcher) probe(enum Enumerator, slots []*Slot) {
	at := time.Now()
	eps, err := enum.enumerate()
	if err != nil {
		w.diag(at, []string{err.Error()})
		return
	}
	pids, problems := scan(eps)
	parent, err := w.parents(at, pids)
	if err != nil {
		w.diag(at, append(problems, "进程快照失败: "+err.Error()))
		return
	}
	for _, s := range slots {
		if active, ok := decide(pids, len(problems) == 0, parent, s.pid); ok {
			s.Publish(active, at)
		}
	}
	if len(problems) > 0 {
		w.diag(at, problems)
	}
}

// parents 返回 PID → PPID 映射：沿用缓存，缓存已满 procRefresh、从未取过、或 pids 里有缓存没有的 PID（新起的
// 进程，其父链此前不可知）时重取。重取失败返回错误，本拍按失败处理，缓存保持原状。
//
// shortcut: 某个 Active 会话属于已退出的进程（不在任何快照里）时，每拍都会重取；这种会话 Windows 很快会
// 置为 Expired，不另设限频。
func (w *Watcher) parents(now time.Time, pids []uint32) (map[uint32]uint32, error) {
	if w.parent != nil && now.Sub(w.parentAt) < procRefresh {
		known := true
		for _, pid := range pids {
			if _, ok := w.parent[pid]; !ok && pid != 0 {
				known = false
				break
			}
		}
		if known {
			return w.parent, nil
		}
	}
	procs, err := w.snapshot()
	if err != nil {
		return nil, err
	}
	m := make(map[uint32]uint32, len(procs))
	for _, p := range procs {
		m[p.PID] = p.PPID
	}
	w.parent, w.parentAt = m, now
	return m, nil
}

// diag 记一拍不完整：首次即报，此后距上次报出满 diagInterval 才报，带上其间不完整的拍数与本拍的首项失败。
// report 投递失败时该条丢弃，不补报：下一条到期时仍会报出。
func (w *Watcher) diag(now time.Time, problems []string) {
	w.failed++
	if w.diagged && now.Sub(w.diagAt) < diagInterval {
		return
	}
	w.report(fmt.Sprintf("[helper] 会话探测不完整，本拍 %d 项失败，首项：%s；自上条以来共 %d 拍不完整", len(problems), problems[0], w.failed))
	w.diagAt, w.diagged, w.failed = now, true, 0
}
