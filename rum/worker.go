package rum

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxSingleEventBytes  = 8 * 1024  // 单条事件序列化上限：8 KiB
	maxBatchEvents       = 50        // 单批事件条数上限
	maxBatchBodyBytes    = 48 * 1024 // 单次 /batch/ 请求体上限：48 KiB
	normalQueueCap       = 256       // 普通事件通道容量
	maxHeartbeatDays     = 33        // lifecycleBuf 最多保留不同日历日心跳数
	maxOtherLifecycle    = 8         // 非常规名称 lifecycle 事件防御上限
	maxFlagsRespBytes    = 1 << 20   // /flags 响应读取上限：1 MiB
	flagsFetchTimeout    = 3 * time.Second
	flagsRefreshInterval = 6 * time.Hour
	defaultBatchInterval = 30 * time.Second
	defaultMinSendGap    = 20 * time.Second
	defaultHTTPTimeout   = 5 * time.Second
	defaultRetryAfter429 = 65 * time.Second
	minRetryAfter429     = 20 * time.Second
	maxRetryAfter429     = 300 * time.Second
	minRetryAfterSecs    = int64(minRetryAfter429 / time.Second)
	maxRetryAfterSecs    = int64(maxRetryAfter429 / time.Second)
	maxFlagsGraceOnFlush = 250 * time.Millisecond
)

var backoffSchedule = [...]time.Duration{
	20 * time.Second,
	40 * time.Second,
	120 * time.Second,
	300 * time.Second,
}

type queuedEvent struct {
	cat       Category
	event     string
	uuid      string
	ts        time.Time
	localDate string
	props     Props
	cfgGen    uint64
	retries   int
}

type flushReq struct {
	deadline time.Time
	done     chan struct{}
	shutdown bool
}

type sendOutcome int

const (
	sendOK sendOutcome = iota
	sendRejected
	sendRateLimited
	sendTransientErr
)

// lifecycleSnap 为不可变快照，发布到 lifecycleBuffer.state 后绝不原地修改其字段、map、slice 或元素指针。
type lifecycleSnap struct {
	start      *queuedEvent
	heartbeats map[string]*queuedEvent
	exit       *queuedEvent
	other      []*queuedEvent
}

// lifecycleBuffer 以无锁不可变快照状态机存放核心生命周期保底事件，
// Track 热路径与后台 worker 通过原子快照与 CAS 并发访问，零互斥锁阻塞。
type lifecycleBuffer struct {
	state atomic.Pointer[lifecycleSnap]
}

func (b *lifecycleBuffer) put(ev queuedEvent) {
	cp := ev
	if cp.event == "app_heartbeat" {
		if d := strings.TrimSpace(cp.localDate); d != "" {
			cp.localDate = d
		} else {
			cp.localDate = cp.ts.In(time.Local).Format("2006-01-02")
		}
	}

	for {
		old := b.state.Load()
		var next lifecycleSnap
		if old != nil {
			next = *old
		}

		switch cp.event {
		case "app_start":
			if old != nil && old.start != nil {
				return
			}
			next.start = &cp
			if b.state.CompareAndSwap(old, &next) {
				return
			}

		case "app_exit":
			next.exit = &cp
			if b.state.CompareAndSwap(old, &next) {
				return
			}

		case "app_heartbeat":
			d := cp.localDate
			if old != nil && old.heartbeats[d] != nil {
				return
			}
			evicted := false
			var nextMap map[string]*queuedEvent
			if old != nil && len(old.heartbeats) >= maxHeartbeatDays {
				oldestDate := ""
				var oldestTS time.Time
				for k, existing := range old.heartbeats {
					if oldestDate == "" || k < oldestDate || (k == oldestDate && existing.ts.Before(oldestTS)) {
						oldestDate = k
						oldestTS = existing.ts
					}
				}
				if oldestDate != "" {
					if d < oldestDate {
						AddDropped("lifecycle_evicted", 1)
						return
					}
					nextMap = make(map[string]*queuedEvent, len(old.heartbeats))
					for k, v := range old.heartbeats {
						if k != oldestDate {
							nextMap[k] = v
						}
					}
					nextMap[d] = &cp
					evicted = true
				}
			}
			if nextMap == nil {
				oldLen := 0
				if old != nil {
					oldLen = len(old.heartbeats)
				}
				nextMap = make(map[string]*queuedEvent, oldLen+1)
				if old != nil {
					for k, v := range old.heartbeats {
						nextMap[k] = v
					}
				}
				nextMap[d] = &cp
			}
			next.heartbeats = nextMap
			if b.state.CompareAndSwap(old, &next) {
				if evicted {
					AddDropped("lifecycle_evicted", 1)
				}
				return
			}

		default:
			var oldOther []*queuedEvent
			if old != nil {
				oldOther = old.other
			}
			evicted := false
			var nextOther []*queuedEvent
			if len(oldOther) >= maxOtherLifecycle {
				nextOther = make([]*queuedEvent, len(oldOther))
				copy(nextOther, oldOther[1:])
				nextOther[len(nextOther)-1] = &cp
				evicted = true
			} else {
				nextOther = make([]*queuedEvent, len(oldOther)+1)
				copy(nextOther, oldOther)
				nextOther[len(oldOther)] = &cp
			}
			next.other = nextOther
			if b.state.CompareAndSwap(old, &next) {
				if evicted {
					AddDropped("lifecycle_evicted", 1)
				}
				return
			}
		}
	}
}

func (b *lifecycleBuffer) snapshot() []queuedEvent {
	snap := b.state.Load()
	if snap == nil {
		return nil
	}
	total := len(snap.heartbeats) + len(snap.other)
	if snap.start != nil {
		total++
	}
	if snap.exit != nil {
		total++
	}
	if total == 0 {
		return nil
	}

	out := make([]queuedEvent, 0, total)
	if snap.start != nil {
		out = append(out, *snap.start)
	}
	if len(snap.heartbeats) > 0 {
		dates := make([]string, 0, len(snap.heartbeats))
		for d := range snap.heartbeats {
			dates = append(dates, d)
		}
		sort.Strings(dates)
		for _, d := range dates {
			out = append(out, *snap.heartbeats[d])
		}
	}
	if snap.exit != nil {
		out = append(out, *snap.exit)
	}
	for _, ev := range snap.other {
		out = append(out, *ev)
	}
	return out
}

func (b *lifecycleBuffer) removeByUUIDs(ids map[string]struct{}) {
	if len(ids) == 0 {
		return
	}

	for {
		old := b.state.Load()
		if old == nil {
			return
		}

		changed := false
		next := *old

		if old.start != nil {
			if _, ok := ids[old.start.uuid]; ok {
				next.start = nil
				changed = true
			}
		}

		if len(old.heartbeats) > 0 {
			hbRemoved := 0
			for _, ev := range old.heartbeats {
				if _, ok := ids[ev.uuid]; ok {
					hbRemoved++
				}
			}
			if hbRemoved > 0 {
				changed = true
				rem := len(old.heartbeats) - hbRemoved
				if rem == 0 {
					next.heartbeats = nil
				} else {
					nextMap := make(map[string]*queuedEvent, rem)
					for d, ev := range old.heartbeats {
						if _, ok := ids[ev.uuid]; !ok {
							nextMap[d] = ev
						}
					}
					next.heartbeats = nextMap
				}
			}
		}

		if old.exit != nil {
			if _, ok := ids[old.exit.uuid]; ok {
				next.exit = nil
				changed = true
			}
		}

		if len(old.other) > 0 {
			otherRemoved := 0
			for _, ev := range old.other {
				if _, ok := ids[ev.uuid]; ok {
					otherRemoved++
				}
			}
			if otherRemoved > 0 {
				changed = true
				rem := len(old.other) - otherRemoved
				if rem == 0 {
					next.other = nil
				} else {
					nextOther := make([]*queuedEvent, 0, rem)
					for _, ev := range old.other {
						if _, ok := ids[ev.uuid]; !ok {
							nextOther = append(nextOther, ev)
						}
					}
					next.other = nextOther
				}
			}
		}

		if !changed {
			return
		}
		if b.state.CompareAndSwap(old, &next) {
			return
		}
	}
}

func (b *lifecycleBuffer) markRefiltered(ids map[string]struct{}, gen uint64) {
	if len(ids) == 0 {
		return
	}

	for {
		old := b.state.Load()
		if old == nil {
			return
		}

		changed := false
		next := *old

		if old.start != nil && old.start.cfgGen != gen {
			if _, ok := ids[old.start.uuid]; ok {
				cp := *old.start
				cp.cfgGen = gen
				next.start = &cp
				changed = true
			}
		}

		if len(old.heartbeats) > 0 {
			hbChanged := false
			for _, ev := range old.heartbeats {
				if ev.cfgGen != gen {
					if _, ok := ids[ev.uuid]; ok {
						hbChanged = true
						break
					}
				}
			}
			if hbChanged {
				changed = true
				nextMap := make(map[string]*queuedEvent, len(old.heartbeats))
				for d, ev := range old.heartbeats {
					if ev.cfgGen != gen {
						if _, ok := ids[ev.uuid]; ok {
							cp := *ev
							cp.cfgGen = gen
							nextMap[d] = &cp
							continue
						}
					}
					nextMap[d] = ev
				}
				next.heartbeats = nextMap
			}
		}

		if old.exit != nil && old.exit.cfgGen != gen {
			if _, ok := ids[old.exit.uuid]; ok {
				cp := *old.exit
				cp.cfgGen = gen
				next.exit = &cp
				changed = true
			}
		}

		if len(old.other) > 0 {
			otherChanged := false
			for _, ev := range old.other {
				if ev.cfgGen != gen {
					if _, ok := ids[ev.uuid]; ok {
						otherChanged = true
						break
					}
				}
			}
			if otherChanged {
				changed = true
				nextOther := make([]*queuedEvent, len(old.other))
				for i, ev := range old.other {
					if ev.cfgGen != gen {
						if _, ok := ids[ev.uuid]; ok {
							cp := *ev
							cp.cfgGen = gen
							nextOther[i] = &cp
							continue
						}
					}
					nextOther[i] = ev
				}
				next.other = nextOther
			}
		}

		if !changed {
			return
		}
		if b.state.CompareAndSwap(old, &next) {
			return
		}
	}
}

func (b *lifecycleBuffer) len() int {
	snap := b.state.Load()
	if snap == nil {
		return 0
	}
	n := len(snap.heartbeats) + len(snap.other)
	if snap.start != nil {
		n++
	}
	if snap.exit != nil {
		n++
	}
	return n
}

func (b *lifecycleBuffer) oldestTS() time.Time {
	snap := b.state.Load()
	if snap == nil {
		return time.Time{}
	}
	var oldest time.Time
	consider := func(ev *queuedEvent) {
		if ev == nil || ev.ts.IsZero() {
			return
		}
		if oldest.IsZero() || ev.ts.Before(oldest) {
			oldest = ev.ts
		}
	}
	consider(snap.start)
	for _, ev := range snap.heartbeats {
		consider(ev)
	}
	consider(snap.exit)
	for _, ev := range snap.other {
		consider(ev)
	}
	return oldest
}

type client struct {
	active                bool
	apiKey                string
	host                  string
	appVersion            string
	distinctID            string
	baseProps             Props
	httpClient            *http.Client
	now                   func() time.Time
	batchInterval         time.Duration
	minSendGap            time.Duration
	tickInterval          time.Duration // 仅同包单测配合可控时钟加速唤醒；生产默认为 0
	testBeforeEnqueueHook func(string)  // 仅同包单测用于注入并发同步屏障；生产恒为 nil
	testAfterBatchHook    func()        // 仅同包单测等待单批退避/重排/丢包结算完成；生产恒为 nil

	cfg        atomic.Pointer[remoteConfig]
	flagsReady atomic.Bool

	stateMu sync.Mutex // 仅串行化 Start 与 Shutdown 状态跃迁，热路径 Track 不加锁
	started atomic.Bool
	closed  atomic.Bool

	onceKeys  sync.Map
	lifecycle lifecycleBuffer // CatLifecycle 无锁不可变原子快照缓冲
	normalCh  chan queuedEvent
	ctrlCh    chan flushReq
	wakeCh    chan struct{}

	flagsCtx      context.Context
	flagsCancel   context.CancelFunc
	flagsDone     chan struct{}
	flagsDoneOnce sync.Once

	reqMu     sync.Mutex
	reqCancel context.CancelFunc

	doneCh chan struct{}

	// 以下字段仅由唯一后台协程访问，无需加锁。
	pendingNormal      []queuedEvent
	batchFirstQueuedAt time.Time
	minGapUntil        time.Time
	cooldownUntil      time.Time
	nextRetryAt        time.Time
	backoffStep        int
	hasSplitBacklog    bool
}

func (c *client) track(cat Category, event string, props Props) {
	if strings.TrimSpace(event) == "" {
		return
	}
	cfg := c.cfg.Load()
	if !cfg.allow(cat, event, cryptoRandFloat64) {
		return
	}
	if hook := c.testBeforeEnqueueHook; hook != nil {
		hook(event)
	}

	// 普通事件队列已满时立即短路丢弃，避免高并发满队场景下做无谓的分配与 UUID/脱敏计算。
	if cat != CatLifecycle && len(c.normalCh) >= cap(c.normalCh) {
		drops.incRum(1)
		return
	}

	var gen uint64
	if cfg != nil {
		gen = cfg.gen
	}
	ts := c.now()
	cleanProps := sanitizeProps(props)
	ev := queuedEvent{
		cat:    cat,
		event:  event,
		uuid:   newUUIDv4(),
		ts:     ts,
		props:  cleanProps,
		cfgGen: gen,
	}

	if cat == CatLifecycle {
		if event == "app_heartbeat" {
			if d, ok := cleanProps["local_date"].(string); ok && strings.TrimSpace(d) != "" {
				ev.localDate = strings.TrimSpace(d)
			} else {
				ev.localDate = ts.In(time.Local).Format("2006-01-02")
			}
		}
		c.lifecycle.put(ev)
		c.nudge()
		return
	}

	select {
	case c.normalCh <- ev:
		c.nudge()
	default:
		drops.incRum(1)
	}
}

func (c *client) nudge() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

func (c *client) markFlagsDone() {
	c.flagsReady.Store(true)
	c.flagsDoneOnce.Do(func() {
		close(c.flagsDone)
	})
}

func (c *client) setActiveReq(cancel context.CancelFunc) {
	c.reqMu.Lock()
	c.reqCancel = cancel
	c.reqMu.Unlock()
}

func (c *client) cancelActiveReq() {
	c.reqMu.Lock()
	cancel := c.reqCancel
	c.reqMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *client) flushOrShutdown(timeout time.Duration, isShutdown bool) {
	var isStarted bool
	if isShutdown {
		c.stateMu.Lock()
		c.closed.Store(true)
		isStarted = c.started.Load()
		c.stateMu.Unlock()
	} else {
		isStarted = c.started.Load()
	}
	if !c.active || !isStarted {
		return
	}

	select {
	case <-c.doneCh:
		return
	default:
	}

	if timeout <= 0 {
		if isShutdown {
			c.flagsCancel()
			c.cancelActiveReq()
			select {
			case c.ctrlCh <- flushReq{deadline: time.Now(), shutdown: true}:
			default:
			}
		}
		return
	}

	deadline := time.Now().Add(timeout)

	// 首拉慢响应早退保护：最多留给在途首拉 min(250ms, timeout/3) 宽限。
	select {
	case <-c.flagsDone:
	case <-c.doneCh:
		return
	default:
		grace := timeout / 3
		if grace > maxFlagsGraceOnFlush {
			grace = maxFlagsGraceOnFlush
		}
		graceTimer := time.NewTimer(grace)
		select {
		case <-c.flagsDone:
			graceTimer.Stop()
		case <-c.doneCh:
			graceTimer.Stop()
			return
		case <-graceTimer.C:
			c.flagsCancel()
			rem := time.Until(deadline)
			if rem <= 0 {
				if isShutdown {
					c.cancelActiveReq()
				}
				return
			}
			waitFlags := time.NewTimer(rem)
			select {
			case <-c.flagsDone:
				waitFlags.Stop()
			case <-c.doneCh:
				waitFlags.Stop()
				return
			case <-waitFlags.C:
				if isShutdown {
					c.cancelActiveReq()
				}
				return
			}
		}
	}

	rem := time.Until(deadline)
	if rem <= 0 {
		if isShutdown {
			c.cancelActiveReq()
		}
		return
	}

	req := flushReq{
		deadline: deadline,
		done:     make(chan struct{}),
		shutdown: isShutdown,
	}

	sendTimer := time.NewTimer(rem)
	select {
	case c.ctrlCh <- req:
		sendTimer.Stop()
	case <-c.doneCh:
		sendTimer.Stop()
		return
	case <-sendTimer.C:
		if isShutdown {
			c.cancelActiveReq()
		}
		return
	}

	rem = time.Until(deadline)
	if rem <= 0 {
		if isShutdown {
			c.cancelActiveReq()
		}
		return
	}

	waitTimer := time.NewTimer(rem)
	defer waitTimer.Stop()

	if isShutdown {
		select {
		case <-c.doneCh:
		case <-waitTimer.C:
			c.cancelActiveReq()
		}
		return
	}

	select {
	case <-req.done:
	case <-c.doneCh:
	case <-waitTimer.C:
		c.cancelActiveReq()
	}
}

func (c *client) run() {
	defer func() {
		if r := recover(); r != nil {
			log.Warn("RUM worker panicked and stopped: %v", r)
		}
		c.markFlagsDone()
		close(c.doneCh)
	}()

	// 首拉远端配置（3s 超时，同时受 Flush/Shutdown 的 flagsCancel 联动取消）。
	initCtx, initCancel := context.WithTimeout(c.flagsCtx, flagsFetchTimeout)
	c.fetchFlagsSafe(initCtx)
	initCancel()
	lastFlagsAt := c.now()

	// 首拉结束后先按最新配置复筛缓冲事件，再发布 flagsDone 就绪信号。
	c.refilterBuffered()
	c.markFlagsDone()

	for {
		// 优先响应 Flush / Shutdown 控制指令，避免被高频唤醒饿死。
		select {
		case req := <-c.ctrlCh:
			if c.handleControl(req) {
				return
			}
			continue
		default:
		}

		c.drainNormalCh()

		now := c.now()
		if now.Sub(lastFlagsAt) >= flagsRefreshInterval {
			ctx, cancel := context.WithTimeout(context.Background(), flagsFetchTimeout)
			c.setActiveReq(cancel)
			updated := c.fetchFlagsSafe(ctx)
			c.setActiveReq(nil)
			cancel()
			if updated {
				c.refilterBuffered()
			}
			lastFlagsAt = c.now()
			now = lastFlagsAt
		}

		hasEvents := c.lifecycle.len() > 0 || len(c.pendingNormal) > 0
		if hasEvents {
			if c.batchFirstQueuedAt.IsZero() {
				oldest := c.oldestQueuedTS()
				if oldest.IsZero() {
					oldest = now
				}
				c.batchFirstQueuedAt = oldest
			}
		} else {
			c.batchFirstQueuedAt = time.Time{}
			c.hasSplitBacklog = false
		}

		if hasEvents && c.shouldSendNow(now) {
			ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
			c.setActiveReq(cancel)
			c.sendOneBatch(ctx, false)
			c.setActiveReq(nil)
			cancel()
			// 发送结束后立即重新检查是否有等待中的控制指令或剩余事件。
			continue
		}

		wait := c.nextWakeDelay(now, lastFlagsAt, hasEvents)
		timer := time.NewTimer(wait)
		select {
		case req := <-c.ctrlCh:
			timer.Stop()
			if c.handleControl(req) {
				return
			}
		case <-c.wakeCh:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (c *client) handleControl(req flushReq) (shouldExit bool) {
	c.drainNormalCh()
	c.refilterBuffered()

	if time.Now().Before(req.deadline) && (c.lifecycle.len() > 0 || len(c.pendingNormal) > 0) {
		ctx, cancel := context.WithDeadline(context.Background(), req.deadline)
		c.setActiveReq(cancel)
		c.sendOneBatch(ctx, true)
		c.setActiveReq(nil)
		cancel()
	}

	if req.done != nil {
		close(req.done)
	}
	return req.shutdown
}

func (c *client) drainNormalCh() {
	cfg := c.cfg.Load()
	for {
		select {
		case ev := <-c.normalCh:
			if !allowWithConfig(&ev, cfg) {
				continue
			}
			if len(c.pendingNormal) >= normalQueueCap {
				drops.incRum(1)
				continue
			}
			c.pendingNormal = append(c.pendingNormal, ev)
		default:
			return
		}
	}
}

// allowWithConfig 使用当前最新配置复核待发事件：
//  1. 命中总开关、disabled_categories 或 disabled_events 时立即拒绝；
//  2. 当事件代次落后于当前配置代次时，按最新采样率重新抽签一次（CatLifecycle 豁免）并更新事件代次，
//     确保失败重试不重复抽签且不遗漏并发入队或 6h 刷新后的规则变更。
func allowWithConfig(ev *queuedEvent, cfg *remoteConfig) bool {
	if cfg == nil {
		return true
	}
	if cfg.isDisabled(ev.cat, ev.event) {
		return false
	}
	if ev.cfgGen != cfg.gen {
		if !cfg.allowSample(ev.cat, ev.event, cryptoRandFloat64) {
			return false
		}
		ev.cfgGen = cfg.gen
	}
	return true
}

// refilterBuffered 使用当前最新远端配置复筛 lifecycleBuf、normalCh 与 pendingNormal 中已缓冲的事件。
func (c *client) refilterBuffered() {
	cfg := c.cfg.Load()
	if cfg == nil {
		return
	}

	// 1. 复筛 lifecycleBuf
	snap := c.lifecycle.snapshot()
	if len(snap) > 0 {
		dropIDs := make(map[string]struct{})
		keepIDs := make(map[string]struct{})
		for _, ev := range snap {
			oldGen := ev.cfgGen
			if !allowWithConfig(&ev, cfg) {
				dropIDs[ev.uuid] = struct{}{}
			} else if ev.cfgGen != oldGen {
				keepIDs[ev.uuid] = struct{}{}
			}
		}
		if len(dropIDs) > 0 {
			c.lifecycle.removeByUUIDs(dropIDs)
		}
		if len(keepIDs) > 0 {
			c.lifecycle.markRefiltered(keepIDs, cfg.gen)
		}
	}

	// 2. 复筛 pendingNormal 与 normalCh 中已有事件
	c.drainNormalCh()
	if len(c.pendingNormal) > 0 {
		kept := c.pendingNormal[:0]
		for _, ev := range c.pendingNormal {
			if !allowWithConfig(&ev, cfg) {
				continue
			}
			kept = append(kept, ev)
		}
		c.pendingNormal = kept
	}
}

func (c *client) oldestQueuedTS() time.Time {
	oldest := c.lifecycle.oldestTS()
	for _, ev := range c.pendingNormal {
		if !ev.ts.IsZero() && (oldest.IsZero() || ev.ts.Before(oldest)) {
			oldest = ev.ts
		}
	}
	return oldest
}

func (c *client) shouldSendNow(now time.Time) bool {
	cfg := c.cfg.Load()
	if cfg == nil || !cfg.enabled {
		return false
	}
	if !c.cooldownUntil.IsZero() && now.Before(c.cooldownUntil) {
		return false
	}
	if !c.nextRetryAt.IsZero() && now.Before(c.nextRetryAt) {
		return false
	}
	if !c.minGapUntil.IsZero() && now.Before(c.minGapUntil) {
		return false
	}

	total := c.lifecycle.len() + len(c.pendingNormal)
	if total >= maxBatchEvents {
		return true
	}
	if c.backoffStep > 0 || c.hasSplitBacklog {
		return true
	}
	if !c.batchFirstQueuedAt.IsZero() && now.Sub(c.batchFirstQueuedAt) >= c.batchInterval {
		return true
	}
	return false
}

func (c *client) nextWakeDelay(now, lastFlagsAt time.Time, hasEvents bool) time.Duration {
	delay := flagsRefreshInterval
	if rem := lastFlagsAt.Add(flagsRefreshInterval).Sub(now); rem < delay {
		delay = rem
	}

	if hasEvents {
		earliest := time.Time{}
		consider := func(t time.Time) {
			if !t.IsZero() && t.After(earliest) {
				earliest = t
			}
		}
		consider(c.cooldownUntil)
		consider(c.nextRetryAt)
		consider(c.minGapUntil)

		total := c.lifecycle.len() + len(c.pendingNormal)
		if total < maxBatchEvents && c.backoffStep == 0 && !c.hasSplitBacklog && !c.batchFirstQueuedAt.IsZero() {
			consider(c.batchFirstQueuedAt.Add(c.batchInterval))
		}
		if !earliest.IsZero() {
			if rem := earliest.Sub(now); rem < delay {
				delay = rem
			}
		}
	}

	if delay <= 0 {
		delay = 5 * time.Millisecond
	}
	if c.tickInterval > 0 && delay > c.tickInterval {
		delay = c.tickInterval
	}
	return delay
}

func (c *client) fetchFlagsSafe(ctx context.Context) (updated bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Warn("RUM worker recovered from panic: %v", r)
		}
	}()

	reqPayload, err := json.Marshal(struct {
		APIKey     string `json:"api_key"`
		DistinctID string `json:"distinct_id"`
	}{
		APIKey:     c.apiKey,
		DistinctID: c.distinctID,
	})
	if err != nil {
		return false
	}

	url := c.host + "/flags/?v=2"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqPayload))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("RUM remote config fetch failed: %v", err)
		}
		return false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFlagsRespBytes))
	if err != nil {
		log.Warn("RUM remote config fetch failed: %v", err)
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}

	prev := c.cfg.Load()
	if next, ok := parseFlagsResponse(body, prev); ok {
		var nextGen uint64 = 1
		if prev != nil {
			nextGen = prev.gen + 1
		}
		next.gen = nextGen
		c.cfg.Store(next)
		return true
	}
	return false
}

type wireEvent struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	UUID       string         `json:"uuid"`
	Timestamp  string         `json:"timestamp"`
	Properties map[string]any `json:"properties"`
}

func (c *client) marshalSingleEvent(ev queuedEvent, cfg *remoteConfig) ([]byte, bool) {
	props := make(map[string]any, len(c.baseProps)+len(ev.props)+8)
	for k, v := range c.baseProps {
		props[k] = v
	}
	for k, v := range ev.props {
		props[k] = v
	}
	props["$lib"] = "playercap-rum"
	props["$lib_version"] = c.appVersion
	props["category"] = string(ev.cat)
	props["app_version"] = c.appVersion

	if ev.event == "app_start" && cfg != nil && cfg.personProfilesOnStart {
		props["$process_person_profile"] = true
		props["$set"] = copyPropsMap(c.baseProps)
		props["$set_once"] = copyPropsMap(c.baseProps)
	} else {
		props["$process_person_profile"] = false
		delete(props, "$set")
		delete(props, "$set_once")
	}

	we := wireEvent{
		Event:      ev.event,
		DistinctID: c.distinctID,
		UUID:       ev.uuid,
		Timestamp:  ev.ts.UTC().Format(time.RFC3339Nano),
		Properties: props,
	}
	raw, err := json.Marshal(we)
	if err != nil {
		return nil, false
	}
	if len(raw) > maxSingleEventBytes {
		AddDropped("oversize_dropped", 1)
		return nil, false
	}
	return raw, true
}

func (c *client) sendOneBatch(ctx context.Context, isFlush bool) {
	cfg := c.cfg.Load()
	if cfg == nil || !cfg.enabled {
		// 若远端已关闭，清空本地待发事件防止积压。
		if snap := c.lifecycle.snapshot(); len(snap) > 0 {
			ids := make(map[string]struct{}, len(snap))
			for _, ev := range snap {
				ids[ev.uuid] = struct{}{}
			}
			c.lifecycle.removeByUUIDs(ids)
		}
		c.pendingNormal = c.pendingNormal[:0]
		return
	}

	keyJSON, err := json.Marshal(c.apiKey)
	if err != nil {
		return
	}
	prefix := `{"api_key":` + string(keyJSON) + `,"batch":[`
	suffix := `]}`
	bodyLen := len(prefix) + len(suffix)

	var (
		encoded             [][]byte
		sentLifecycle       = make(map[string]struct{})
		dropLifecycle       = make(map[string]struct{})
		refilteredLifecycle = make(map[string]struct{})
		sentNormal          []queuedEvent
		remNormal           []queuedEvent
		splitRemain         bool
	)

	// 1. 优先装入 lifecycleBuf 事件（出网前逐条用最新 cfg 复核关闭规则与代次）
	lcSnap := c.lifecycle.snapshot()
	for _, ev := range lcSnap {
		oldGen := ev.cfgGen
		if !allowWithConfig(&ev, cfg) {
			dropLifecycle[ev.uuid] = struct{}{}
			continue
		}
		if ev.cfgGen != oldGen {
			refilteredLifecycle[ev.uuid] = struct{}{}
		}
		raw, ok := c.marshalSingleEvent(ev, cfg)
		if !ok {
			dropLifecycle[ev.uuid] = struct{}{}
			continue
		}
		extra := len(raw)
		if len(encoded) > 0 {
			extra++
		}
		if len(encoded) >= maxBatchEvents || bodyLen+extra > maxBatchBodyBytes {
			if len(encoded) == 0 {
				// 极端防御：单条仍无法装入 48 KiB 整包时放弃，避免死循环。
				AddDropped("oversize_dropped", 1)
				dropLifecycle[ev.uuid] = struct{}{}
				continue
			}
			splitRemain = true
			break
		}
		encoded = append(encoded, raw)
		bodyLen += extra
		sentLifecycle[ev.uuid] = struct{}{}
	}
	if len(dropLifecycle) > 0 {
		c.lifecycle.removeByUUIDs(dropLifecycle)
	}
	if len(refilteredLifecycle) > 0 {
		c.lifecycle.markRefiltered(refilteredLifecycle, cfg.gen)
	}

	// 2. 在剩余条数与 48 KiB 预算内装入普通事件（同样逐条按最新 cfg 复核关闭与采样代次）
	if !splitRemain && len(c.pendingNormal) > 0 {
		for i, ev := range c.pendingNormal {
			if !allowWithConfig(&ev, cfg) {
				continue
			}
			raw, ok := c.marshalSingleEvent(ev, cfg)
			if !ok {
				continue
			}
			extra := len(raw)
			if len(encoded) > 0 {
				extra++
			}
			if len(encoded) >= maxBatchEvents || bodyLen+extra > maxBatchBodyBytes {
				if len(encoded) == 0 {
					AddDropped("oversize_dropped", 1)
					continue
				}
				remNormal = append(remNormal, ev)
				if i+1 < len(c.pendingNormal) {
					remNormal = append(remNormal, c.pendingNormal[i+1:]...)
				}
				splitRemain = true
				break
			}
			encoded = append(encoded, raw)
			bodyLen += extra
			sentNormal = append(sentNormal, ev)
		}
	} else if len(c.pendingNormal) > 0 {
		remNormal = append(remNormal, c.pendingNormal...)
	}

	// 更新剩余普通队列
	c.pendingNormal = remNormal

	if len(encoded) == 0 {
		return
	}

	var buf bytes.Buffer
	buf.Grow(bodyLen)
	buf.WriteString(prefix)
	for i, raw := range encoded {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(raw)
	}
	buf.WriteString(suffix)

	sentAt := c.now()
	outcome, statusCode, retryAfter := c.postBatchSafe(ctx, buf.Bytes(), sentAt)
	if !isFlush {
		c.minGapUntil = sentAt.Add(c.minSendGap)
	}

	switch outcome {
	case sendOK:
		c.lifecycle.removeByUUIDs(sentLifecycle)
		c.backoffStep = 0
		c.nextRetryAt = time.Time{}
		c.cooldownUntil = time.Time{}
		c.hasSplitBacklog = splitRemain || c.lifecycle.len() > 0 || len(c.pendingNormal) > 0
		if !c.hasSplitBacklog {
			c.batchFirstQueuedAt = time.Time{}
		}

	case sendRejected:
		// 400 / 413 客户端格式错误：移除已拒发事件防死循环。
		log.Warn("RUM batch rejected with HTTP %d; dropping batch", statusCode)
		c.lifecycle.removeByUUIDs(sentLifecycle)
		if len(sentNormal) > 0 {
			AddDropped("send_failed_events", uint64(len(sentNormal)))
		}
		c.backoffStep = 0
		c.nextRetryAt = time.Time{}
		c.hasSplitBacklog = splitRemain || c.lifecycle.len() > 0 || len(c.pendingNormal) > 0
		if !c.hasSplitBacklog {
			c.batchFirstQueuedAt = time.Time{}
		}

	case sendRateLimited:
		// 429 冷却：lifecycleBuf 保留原 uuid/ts；普通事件最多跟退 1 轮。
		c.cooldownUntil = sentAt.Add(retryAfter)
		c.requeueOrDropNormal(sentNormal)
		c.hasSplitBacklog = true

	default:
		// 网络错误 / 5xx / panic：指数退避 20s -> 40s -> 120s -> 300s，lifecycleBuf 不丢弃。
		step := c.backoffStep
		if step >= len(backoffSchedule) {
			step = len(backoffSchedule) - 1
		}
		c.nextRetryAt = sentAt.Add(backoffSchedule[step])
		if c.backoffStep < len(backoffSchedule) {
			c.backoffStep++
		}
		c.requeueOrDropNormal(sentNormal)
		c.hasSplitBacklog = true
	}
	if hook := c.testAfterBatchHook; hook != nil {
		hook()
	}
}

func (c *client) requeueOrDropNormal(batch []queuedEvent) {
	if len(batch) == 0 {
		return
	}
	var retryList []queuedEvent
	var dropped uint64
	for _, ev := range batch {
		if ev.retries < 1 {
			ev.retries++
			retryList = append(retryList, ev)
		} else {
			dropped++
		}
	}
	if dropped > 0 {
		AddDropped("send_failed_events", dropped)
	}
	if len(retryList) == 0 {
		return
	}
	// 将待重试普通事件放回队首，总长度严格有界不超过 normalQueueCap。
	combined := make([]queuedEvent, 0, len(retryList)+len(c.pendingNormal))
	combined = append(combined, retryList...)
	combined = append(combined, c.pendingNormal...)
	if len(combined) > normalQueueCap {
		overflow := len(combined) - normalQueueCap
		drops.incRum(uint64(overflow))
		combined = combined[:normalQueueCap]
	}
	c.pendingNormal = combined
}

func (c *client) postBatchSafe(ctx context.Context, payload []byte, now time.Time) (outcome sendOutcome, statusCode int, retryAfter time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Warn("RUM worker recovered from panic: %v", r)
			outcome = sendTransientErr
		}
	}()

	url := c.host + "/batch/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return sendTransientErr, 0, 0
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return sendTransientErr, 0, 0
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	statusCode = resp.StatusCode
	switch {
	case statusCode >= 200 && statusCode < 300:
		return sendOK, statusCode, 0
	case statusCode == http.StatusBadRequest || statusCode == http.StatusRequestEntityTooLarge:
		return sendRejected, statusCode, 0
	case statusCode == http.StatusTooManyRequests:
		return sendRateLimited, statusCode, parseRateLimitDelay(
			resp.Header.Get("Retry-After"),
			resp.Header.Get("RateLimit-Reset"),
			now,
		)
	default:
		return sendTransientErr, statusCode, 0
	}
}

// parseRateLimitDelay 解析 HTTP 429 冷却时长：优先解析非空 Retry-After，
// 缺失或空白时兼容解析 Kong Rate Limiting Advanced 的 RateLimit-Reset。
func parseRateLimitDelay(retryAfter, rateLimitReset string, now time.Time) time.Duration {
	if strings.TrimSpace(retryAfter) != "" {
		return parseRetryAfter(retryAfter, now)
	}
	return parseRateLimitReset(rateLimitReset)
}

// parseRateLimitReset 解析 Kong Rate Limiting Advanced 返回的 RateLimit-Reset（仅接受正整数秒数），
// 结果钳制在 [20s, 300s]；缺失、0、负数、非整数或空白时回落默认 65s。
func parseRateLimitReset(header string) time.Duration {
	v := strings.TrimSpace(header)
	if !isAllASCIIDigits(v) {
		return defaultRetryAfter429
	}
	secs, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		if isStrconvRangeErr(err) {
			return maxRetryAfter429
		}
		return defaultRetryAfter429
	}
	if secs == 0 {
		return defaultRetryAfter429
	}
	if secs < uint64(minRetryAfterSecs) {
		return minRetryAfter429
	}
	if secs > uint64(maxRetryAfterSecs) {
		return maxRetryAfter429
	}
	return time.Duration(secs) * time.Second
}

func isAllASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseRetryAfter 解析 HTTP 429 的 Retry-After 响应头（支持秒数与 HTTP-date），
// 结果钳制在 [20s, 300s]；缺失或解析失败时返回默认 65s。
// 整数秒先在 int64 秒域完成钳制再乘 time.Second，避免超大秒数乘法溢出变负。
func parseRetryAfter(header string, now time.Time) time.Duration {
	v := strings.TrimSpace(header)
	if v == "" {
		return defaultRetryAfter429
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil || isStrconvRangeErr(err) {
		if secs < minRetryAfterSecs {
			secs = minRetryAfterSecs
		} else if secs > maxRetryAfterSecs {
			secs = maxRetryAfterSecs
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return clampRetryAfter(t.Sub(now))
	}
	return defaultRetryAfter429
}

func isStrconvRangeErr(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

func clampRetryAfter(d time.Duration) time.Duration {
	if d < minRetryAfter429 {
		return minRetryAfter429
	}
	if d > maxRetryAfter429 {
		return maxRetryAfter429
	}
	return d
}
