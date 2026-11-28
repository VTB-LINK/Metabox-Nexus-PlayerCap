package rum

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/logger"
)

// 构建期经 `-ldflags -X` 注入的 PostHog 配置；源码中必须为空串字面量初始化的包级 var，
// 由 ldflagssymbol_test.go 与 CI 流水线双向钉死。
var (
	apiKey = "" // -X 'Metabox-Nexus-PlayerCap/rum.apiKey=...'
	host   = "" // -X 'Metabox-Nexus-PlayerCap/rum.host=...'
)

var log = logger.New("RUM")

// Category 标识 RUM 事件优先级与远端开关分类。
type Category string

const (
	CatLifecycle   Category = "lifecycle"     // P0：启动、跨日心跳、退出（豁免常规采样）
	CatUpdateCfg   Category = "update_config" // P1：自动更新、配置迁移与强制重建
	CatPlayer      Category = "player_health" // P1：播放器会话、受阻门禁、Watchdog 干预、兼容退化
	CatLyricTrack  Category = "lyric_track"   // P2：换歌取词能力结算、曲中歌词变更
	CatRouteClient Category = "route_client"  // P2：优先组路由切换、下游 WS/SSE 客户端会话
	CatAudioEffect Category = "audio_effect"  // P2：音频波形会话与异常信号、网易云特效镜像性能
	CatDiagFine    Category = "diag_fine"     // P3：跳转/暂扣瞬态拦截、内存/RM 扫描耗时、HTTP 轮询汇总
)

// Props 为结构化事件属性。
type Props map[string]any

// Options 配置进程级 RUM 客户端。
type Options struct {
	IsRelease  bool
	AppVersion string
	DistinctID string           // 来自 clientid.ID()；为空时强制禁用
	BaseProps  Props            // 进程级只读基础环境属性
	HTTPClient *http.Client     // 可选；nil 时使用 &http.Client{Timeout: 5 * time.Second}
	Now        func() time.Time // 可选；nil 时使用 time.Now
}

var (
	current     atomic.Pointer[client]
	drops       dropLedger
	globalState sync.Mutex // 仅用于 Init 替换全局实例时的串行化，热路径不加锁
)

type dropLedger struct {
	rumDropped atomic.Uint64
	scopes     sync.Map // string -> *atomic.Uint64
}

func (d *dropLedger) incRum(n uint64) {
	if n > 0 {
		d.rumDropped.Add(n)
	}
}

func (d *dropLedger) addScope(scope string, n uint64) {
	if n == 0 || scope == "" {
		return
	}
	if v, ok := d.scopes.Load(scope); ok {
		v.(*atomic.Uint64).Add(n)
		return
	}
	counter := &atomic.Uint64{}
	actual, _ := d.scopes.LoadOrStore(scope, counter)
	actual.(*atomic.Uint64).Add(n)
}

func (d *dropLedger) drain() (uint64, map[string]uint64) {
	rd := d.rumDropped.Swap(0)
	var byScope map[string]uint64
	d.scopes.Range(func(k, v any) bool {
		if n := v.(*atomic.Uint64).Swap(0); n > 0 {
			if byScope == nil {
				byScope = make(map[string]uint64)
			}
			byScope[k.(string)] = n
		}
		return true
	})
	return rd, byScope
}

func (d *dropLedger) reset() {
	d.rumDropped.Store(0)
	d.scopes.Range(func(k, v any) bool {
		v.(*atomic.Uint64).Store(0)
		d.scopes.Delete(k)
		return true
	})
}

// Configured 报告构建期是否同时注入了非空 apiKey 与 host。
func Configured() bool {
	globalState.Lock()
	k, h := apiKey, host
	globalState.Unlock()
	return strings.TrimSpace(k) != "" && strings.TrimSpace(h) != ""
}

// Enabled 报告当前进程是否实际启用 RUM（满足构建注入、发布版、非空 DistinctID 及远端总开关开启）。
func Enabled() bool {
	c := current.Load()
	if c == nil || !c.active || c.closed.Load() {
		return false
	}
	cfg := c.cfg.Load()
	return cfg != nil && cfg.enabled
}

// Init 完成纯内存初始化（复制并脱敏 BaseProps、初始化默认策略与缓冲），绝不启动协程或网络请求。
func Init(opts Options) {
	globalState.Lock()
	defer globalState.Unlock()

	drops.reset()

	key := strings.TrimSpace(apiKey)
	baseHost := strings.TrimRight(strings.TrimSpace(host), "/")
	distinctID := strings.TrimSpace(opts.DistinctID)

	if key == "" || baseHost == "" || !opts.IsRelease || distinctID == "" {
		current.Store(&client{active: false})
		return
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	flagsCtx, flagsCancel := context.WithCancel(context.Background())

	c := &client{
		active:        true,
		apiKey:        key,
		host:          baseHost,
		appVersion:    opts.AppVersion,
		distinctID:    distinctID,
		baseProps:     sanitizeProps(opts.BaseProps),
		httpClient:    httpClient,
		now:           nowFn,
		batchInterval: defaultBatchInterval,
		minSendGap:    defaultMinSendGap,
		normalCh:      make(chan queuedEvent, normalQueueCap),
		ctrlCh:        make(chan flushReq, 4),
		wakeCh:        make(chan struct{}, 1),
		flagsCtx:      flagsCtx,
		flagsCancel:   flagsCancel,
		flagsDone:     make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	c.cfg.Store(defaultRemoteConfig())
	current.Store(c)
}

// Start 幂等启动唯一后台工作协程；通过 stateMu 与 Shutdown 串行化状态跃迁，绝不在关闭后启动协程。
func Start() {
	c := current.Load()
	if c == nil || !c.active {
		return
	}
	c.stateMu.Lock()
	if c.closed.Load() || c.started.Load() {
		c.stateMu.Unlock()
		return
	}
	c.started.Store(true)
	go c.run()
	c.stateMu.Unlock()
}

// Track 非阻塞入队一条事件；未启用、命中关闭规则、未命中采样或队列已满时立即返回。
func Track(cat Category, event string, props Props) {
	c := current.Load()
	if c == nil || !c.active || c.closed.Load() {
		return
	}
	c.track(cat, event, props)
}

// TrackOnce 按 dedupKey 在当前进程生命周期内去重，首次命中时调用 Track。
func TrackOnce(dedupKey string, cat Category, event string, props Props) {
	c := current.Load()
	if c == nil || !c.active || c.closed.Load() {
		return
	}
	key := dedupKey
	if key == "" {
		key = string(cat) + "\x00" + event
	}
	if _, loaded := c.onceKeys.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	c.track(cat, event, props)
}

// AddDropped 无锁原子累加指定 scope 的非阻塞丢包或内部诊断计数。
func AddDropped(scope string, n uint64) {
	drops.addScope(scope, n)
}

// DrainDropped 原子取出并清零本周期累计的 rumDropped 及各 scope 丢包账单。
func DrainDropped() (rumDropped uint64, byScope map[string]uint64) {
	return drops.drain()
}

// Flush 限时冲刷当前缓冲的单个尾批分片；超时立即返回。
func Flush(timeout time.Duration) {
	c := current.Load()
	if c == nil || !c.active {
		return
	}
	c.flushOrShutdown(timeout, false)
}

// Shutdown 停止接收新事件并限时冲刷尾批；超时立即取消在途请求并返回。
func Shutdown(timeout time.Duration) {
	c := current.Load()
	if c == nil || !c.active {
		return
	}
	c.flushOrShutdown(timeout, true)
}

// resetForTest 仅供同包单元测试在清理阶段还原包级状态。
func resetForTest() {
	globalState.Lock()
	defer globalState.Unlock()
	current.Store(nil)
	drops.reset()
}

// OverrideCredentialsForTest 仅供跨包集成测试临时注入本地测试端点与密钥，返回清理并还原状态的回调。
func OverrideCredentialsForTest(key, baseHost string) func() {
	globalState.Lock()
	oldKey, oldHost := apiKey, host
	apiKey, host = key, baseHost
	globalState.Unlock()
	return func() {
		Shutdown(200 * time.Millisecond)
		globalState.Lock()
		apiKey, host = oldKey, oldHost
		current.Store(nil)
		drops.reset()
		globalState.Unlock()
	}
}

func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}

func cryptoRandFloat64() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	// 取高 53 位映射至 [0.0, 1.0) 均匀浮点区间。
	v := binary.BigEndian.Uint64(b[:]) >> 11
	return float64(v) / (1 << 53)
}
