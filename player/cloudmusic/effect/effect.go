// Package effect 捕获网易云音乐「特效歌词」WebGL canvas 的画面，镜像给前端供 OBS 捕获。
//
// 默认走「纯层捕获」(purelayer)：独立开一条 CDP WebSocket（与取词的 cdp.Client 分离），
// 注入页面脚本直读 #lyric-effect-canvas-id 像素（drawImage 复制，严格只读，绝不改源 canvas——
// 改其尺寸会让网易云崩溃），编码 JPEG 经 page→Go 的 ingest WS 回传，再交 FrameSink 广播给前端。
// 抓的是 canvas backing store，永不含工具栏/顶栏/进度条（DOM 合成层）→ 主播可常开 chrome，OBS 仍得纯特效层。
// 仅在有订阅者时才连接/注入，空闲时暂停页面抓帧循环省 CPU。
//
// 兜底：环境变量 MBX_EFFECT_MODE=screencast 回退到 Page.startScreencast 截整视口（含 chrome）。
// 保活：Emulation.setFocusEmulationEnabled + Page.setWebLifecycleState(active)（best-effort）；
// 被遮挡/最小化的 100% 保活由 off-screen 泊车（park 策略）提供。
// 设计与实测细节见 doc/cloudmusic-effect-capture.md。
package effect

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/park"

	"github.com/gorilla/websocket"
)

var log = logger.New("CloudMusic] [Effect") // 渲染为 [CloudMusic] [Effect]（网易云统一前缀 + 二级）

const targetPageURL = "orpheus://orpheus/pub/app.html"

// FrameSink 接收 JPEG 帧、告知是否有人正在收看、并提供前端设置的截帧/注入参数（由 server.Server 实现）。
type FrameSink interface {
	HasEffectSubscribers() bool
	BroadcastEffectFrame(jpeg []byte)
	// EffectCaptureParams 返回 JPEG 质量(1-100)、缩放(0-1)、顶/底栏隐藏后是否保留点击能力、变更代号。
	EffectCaptureParams() (quality int, scale float64, headerClickable, footerClickable bool, gen uint64)
	// SetEffectShowing 报告网易云是否正打开歌曲详情页（特效在显示）。
	SetEffectShowing(showing bool)
	// EffectStrategy 最小化/迷你时策略："fadeout" | "park"。
	EffectStrategy() string
	// TakeManualParkCmd 取走一次性手动命令：-1 无 / 1 park / 0 unpark。
	TakeManualParkCmd() int
	// SetEffectIngestHandler 注册纯层帧回传处理器（注入脚本经 ingest WS 推来的 JPEG）。
	SetEffectIngestHandler(func([]byte))
	// EffectIngestWSURL 注入脚本用的 page→Go 帧回传地址。
	EffectIngestWSURL() string
}

// captureFPS 纯层抓帧循环的目标帧率（注入脚本里按此节流 drawImage+编码）。
const captureFPS = 30

// captureOutMaxW 编码输出最大宽度上限：仅当 canvas 实际宽超过它（窗口很大）才降采样，控制编码量保帧率；
// 正常窗口尺寸下不缩放（直接原生编码）。注意：这是对「我们自己快照画布」的降采样，绝不改网易云 canvas。
const captureOutMaxW = 1920

// glShimJS 单独的 getContext shim：强制特效 canvas 的 WebGL context 用 preserveDrawingBuffer:true。
// 在网易云冷启动阶段（取词刚连上、还没开歌）就 eval 一次注入到当前文档 → 之后开的歌 context 自带
// preserveDrawingBuffer，订阅者连上时无需 reload（避免播放中断）。幂等（__mbxGLHook 守卫），与
// captureInjectJS 里的同段互不冲突。两处须保持一致。
const glShimJS = `(function(){
  if(window.__mbxGLHook)return; window.__mbxGLHook=true;
  var orig=HTMLCanvasElement.prototype.getContext;
  HTMLCanvasElement.prototype.getContext=function(type,attrs){
    if(type&&/webgl/i.test(String(type))){var a=attrs||{};a.preserveDrawingBuffer=true;return orig.call(this,type,a);}
    return orig.call(this,type,attrs);
  };
})()`

// captureInjectJS 纯层捕获注入脚本（严格只读，绝不修改网易云 canvas —— 改其尺寸会导致网易云崩溃）：
//  1. WebGL getContext shim 强制 preserveDrawingBuffer:true —— 使合成后绘制缓冲不被清空，drawImage 随时
//     能读到当前帧（否则只有 ~50% 命中）。SPA 同文档 eval 一次即对「之后创建的 context」生效；仅当连上时
//     已有歌（context 已建）才 reload 一次。
//  2. 抓帧循环：rAF 节流 → drawImage(#lyric-effect-canvas-id 到画布池快照) → toBlob(jpeg) → WS 推给 Go。
//     按 canvas 当前实际尺寸抓（天然适配窗口最大化/手动调整，不假设固定尺寸）；仅当超大才降采样到 outw。
//     画布池 + 并发 toBlob 提帧率（toBlob 异步不阻塞主线程）。抓的是 canvas backing store，永不含工具栏/
//     顶栏/进度条（DOM 合成层）→ 主播可常开 chrome，OBS 仍得纯特效层。无订阅者时 __mbxCapPaused 暂停省 CPU。
//     分辨率 = 网易云窗口尺寸（用户放大窗口即更清晰，我们不强制改渲染分辨率以免崩溃/变形）。
//
// %q=ingest WS 地址；%f=初始 q(0-1)；%d=初始 fps；%d=输出最大宽。
const captureInjectJS = `(function(){
  if(!window.__mbxGLHook){
    window.__mbxGLHook=true;
    var orig=HTMLCanvasElement.prototype.getContext;
    HTMLCanvasElement.prototype.getContext=function(type,attrs){
      if(type&&/webgl/i.test(String(type))){var a=attrs||{};a.preserveDrawingBuffer=true;return orig.call(this,type,a);}
      return orig.call(this,type,attrs);
    };
  }
  if(window.__mbxCapStarted)return; window.__mbxCapStarted=true;
  var SEL='#lyric-effect-canvas-id', WS_URL=%q;
  if(!window.__mbxCapCfg)window.__mbxCapCfg={q:%f,fps:%d,outw:%d};
  var ws=null, inflight=0, lastSend=0;
  function ensureWS(){
    if(ws&&(ws.readyState===0||ws.readyState===1))return;
    try{ws=new WebSocket(WS_URL);ws.binaryType='arraybuffer';}catch(e){ws=null;}
  }
  // 编码画布池：每个在飞帧用独立 2D 画布快照，允许并发 toBlob（toBlob 异步、编码不阻塞主线程）
  var POOL=3, cans=[];
  for(var i=0;i<POOL;i++){var o=document.createElement('canvas');var cx=o.getContext('2d',{alpha:false});cx.imageSmoothingEnabled=true;cx.imageSmoothingQuality='high';cans.push({o:o,x:cx,busy:false});}
  function loop(){
    requestAnimationFrame(loop);
    if(window.__mbxCapPaused)return;
    var cfg=window.__mbxCapCfg||{}, now=performance.now();
    // 软上限：达到目标 fps 才跳过（容差 半个 60fps 帧，避免 rAF 量化导致每隔一拍被错过而腰斩帧率）
    if(cfg.fps && now-lastSend < 1000/cfg.fps - 8)return;
    if(inflight>=POOL)return;
    var c=document.querySelector(SEL);
    if(!c||!c.width||!c.height)return;
    ensureWS(); if(!ws||ws.readyState!==1)return;
    var slot=null; for(var i=0;i<POOL;i++){if(!cans[i].busy){slot=cans[i];break;}} if(!slot)return;
    // 按 canvas 实际尺寸快照；仅当超过 outw（超大窗口）才降采样，控制编码量。绝不改源 canvas。
    var ow=c.width, oh=c.height, mx=cfg.outw||0;
    if(mx>0&&ow>mx){var k=mx/ow; ow=Math.max(1,Math.round(ow*k)); oh=Math.max(1,Math.round(oh*k));}
    if(slot.o.width!==ow)slot.o.width=ow; if(slot.o.height!==oh)slot.o.height=oh;
    try{slot.x.drawImage(c,0,0,ow,oh);}catch(e){return;}
    slot.busy=true; inflight++; lastSend=now;
    (function(s){s.o.toBlob(function(b){
      s.busy=false; inflight--;
      if(b&&ws&&ws.readyState===1){b.arrayBuffer().then(function(ab){try{ws.send(ab);}catch(_){}}).catch(function(){});}
    },'image/jpeg',cfg.q||0.88);})(slot);
  }
  requestAnimationFrame(loop);
})()`

// showingExprJS 判断网易云是否正打开歌曲详情页（特效在显示）。
// 关键：退出/进入详情页时网易云的 style 属性几乎不变（始终 translateY(0px)），真正动画的是
// computed transform（ty 从 0 ramp 到 752 下滑退出，或从大值 ramp 到 0 上滑进入）。
// 故必须读 computed transform 的 ty：|ty|>3px 即视为正在滑动（退出中/未到位）→ 不显示。
// 这样退出一开始（主页尚未露出）就能检测到，前端据此冻结，不漏主页。
const showingExprJS = `(function(){
  // 特效 canvas 必须存在：黑胶/标准模式会把 #lyric-effect-canvas-id 移出 DOM（仅特效模式才有），
  // 故非特效模式此处即 false → 前端淡出，不会僵死在最后一张特效帧。
  var c=document.querySelector('#lyric-effect-canvas-id');
  if(!c||!c.width||!c.height) return false;
  var dp=document.querySelector('#vinyl-page-container');
  if(!dp) return false;
  var cs=getComputedStyle(dp);
  if(cs.display==='none') return false;
  var m=cs.transform.match(/matrix\(([^)]+)\)/);
  if(m){var ty=parseFloat(m[1].split(',')[5])||0; if(ty>3||ty<-3) return false;}
  return dp.offsetHeight>0;
})()`

// chromeInjectJS 注入到网易云页面：双击歌曲详情页切换隐藏顶栏+底栏+边缘渐变遮罩。
// 仅在详情页打开时生效（#vinyl-page-container 可见，三种模式共用此容器），排除主页/浏览页；
// 隐藏状态加在 body 上，SPA 切换不丢（离开详情页再回来仍保持上次状态）。
// 隐藏用 opacity:0 强制覆盖网易云自身的「鼠标唤出」逻辑（晃鼠标也不出现）；
// 顶/底栏按 headerClickable/footerClickable 决定隐藏后是否保留点击（拖动/最大化/控件）：
//   - 保留点击：仅 opacity:0，元素仍可命中，Electron 的 -webkit-app-region:drag 照常生效；
//   - 不保留：附加 pointer-events:none，点击穿透到特效区（双击该区即切换）。
//
// 暴露 window.__mbxSetChromeHidden(bool) 供 off-screen 泊车强制隐藏复用。
// 通过 Page.addScriptToEvaluateOnNewDocument 注册（每次页面加载/重载自动运行），故需 document-start 安全：
// 立即挂 window 级监听，样式注入延到 DOMContentLoaded。两个 %q 替换为顶/底栏附加样式。
const chromeInjectJS = `(function(){
  if(window.__mbxCleanup)window.__mbxCleanup();
  var headerPE=%q, footerPE=%q;
  function applyStyle(){
    if(document.getElementById('mbx-chrome-style'))return;
    var st=document.createElement('style');st.id='mbx-chrome-style';
    st.textContent=
      'body.mbx-chrome-hidden .page-header{opacity:0 !important;'+headerPE+'}'+
      'body.mbx-chrome-hidden .page-footer{opacity:0 !important;'+footerPE+'}'+
      // 进度条 slider 的 .hotzone-overlay 显式 pointer-events:auto，会盖过 footer 的 none，需单独按 footer 设置禁用
      'body.mbx-chrome-hidden [class*="StyledSliderContainer"],body.mbx-chrome-hidden .hotzone-overlay{opacity:0 !important;'+footerPE+'}'+
      'body.mbx-chrome-hidden .effect-mask::before,body.mbx-chrome-hidden .effect-mask::after{opacity:0 !important}';
    (document.head||document.documentElement).appendChild(st);
  }
  if(document.readyState==='loading'){document.addEventListener('DOMContentLoaded',applyStyle);}else{applyStyle();}
  var onDbl=function(e){
    if(e.target&&e.target.closest&&(e.target.closest('.page-header')||e.target.closest('.page-footer')))return;
    var dp=document.querySelector('#vinyl-page-container');
    if(!dp||dp.offsetHeight<=0)return;
    document.body.classList.toggle('mbx-chrome-hidden');
    e.preventDefault();e.stopPropagation();
  };
  window.addEventListener('dblclick',onDbl,true);
  window.__mbxSetChromeHidden=function(v){if(document.body)document.body.classList.toggle('mbx-chrome-hidden',!!v);};
  window.__mbxCleanup=function(){window.removeEventListener('dblclick',onDbl,true);var s=document.getElementById('mbx-chrome-style');if(s)s.remove();};
})()`

// Capturer 网易云特效画面捕获器。
type Capturer struct {
	sink          FrameSink
	netEaseUp     func() bool // 取词 player 是否已连上网易云；未连上则静默待命，不独立探 9222
	mode          string      // "purelayer"(默认，注入读 canvas) | "screencast"(兜底，Page.startScreencast)
	showing       int32       // atomic：网易云是否正显示详情页特效（用于帧门控，不在详情页时停发帧）
	minimized     int32       // atomic：仅 fadeout 策略下的最小化（并入 showing 触发淡出）
	rawMinimized  int32       // atomic：真实最小化状态（任何策略下都门控帧 → 冻结最后一帧，不漏最小化动画）
	gateUntilNano int64       // atomic：在此时间前门控帧（park 过渡期冻结，盖住最小化动画 + park 后冷渲染）
	parkedAtNano  int64       // atomic：上次 park 的时刻，用于 post-park 冷却（避免 park 反激活立即触发 unpark 弹回）
}

func (c *Capturer) setShowing(v bool) {
	var iv int32
	if v {
		iv = 1
	}
	atomic.StoreInt32(&c.showing, iv)
	c.sink.SetEffectShowing(v)
}

func (c *Capturer) isShowing() bool { return atomic.LoadInt32(&c.showing) == 1 }

// New 创建捕获器。默认 purelayer 模式；MBX_EFFECT_MODE=screencast 回退到 screencast 兜底。
//
// 纯层分辨率 = 网易云窗口当前尺寸（网易云把特效渲染在 CSS 分辨率，DPR-unaware），按 canvas 实际尺寸
// 抓取（天然适配窗口最大化/手动调整）；想更清晰让用户放大窗口（不强制改渲染分辨率以免崩溃）。
// 前端可通过 quality 调节 JPEG 清晰度/带宽。
//
// netEaseUp 由调用方提供（通常是取词 player 的 IsConnected），用于门控生命周期：未连上网易云时
// 捕获器静默待命，不独立探测 9222、不刷屏。传 nil 视为「总是可用」（如 devserver 无取词管线时）。
func New(sink FrameSink, netEaseUp func() bool) *Capturer {
	mode := "purelayer"
	if os.Getenv("MBX_EFFECT_MODE") == "screencast" {
		mode = "screencast" // 兜底：环境变量强制回退到 screencast 像素源
	}
	if netEaseUp == nil {
		netEaseUp = func() bool { return true }
	}
	return &Capturer{sink: sink, netEaseUp: netEaseUp, mode: mode}
}

// ingestFrame 纯层路径的帧门控：仅在网易云正显示详情页特效、未最小化、且不在 park 过渡冻结期时
// 才广播给 effect-ws 订阅者（与 screencast 路径的门控一致），否则前端冻结在最后一张特效帧。
func (c *Capturer) ingestFrame(jpeg []byte) {
	if c.isShowing() && atomic.LoadInt32(&c.rawMinimized) == 0 && time.Now().UnixNano() >= atomic.LoadInt64(&c.gateUntilNano) {
		c.sink.BroadcastEffectFrame(jpeg)
	}
}

// evalOnce 用一条临时 CDP 连接执行一段 JS（best-effort）。
func evalOnce(expr string) {
	wsURL, err := screencastWSURL()
	if err != nil {
		return
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.WriteJSON(map[string]any{"id": 1, "method": "Runtime.evaluate", "params": map[string]any{"expression": expr}})
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var discard json.RawMessage
	conn.ReadJSON(&discard)
}

func (c *Capturer) doPark() {
	if park.Park() {
		atomic.StoreInt64(&c.parkedAtNano, time.Now().UnixNano())
		// 冻结一小段：盖住「最小化动画 + park 过渡 + park 后特效冷渲染」这几帧，前端 hold 住最后干净帧
		atomic.StoreInt64(&c.gateUntilNano, time.Now().Add(700*time.Millisecond).UnixNano())
		// 记住泊车前的 chrome 状态再强制隐藏（屏外保证纯净）；unpark 时还原到该状态，不覆盖用户的双击选择
		evalOnce("if(window.__mbxSetChromeHidden){window.__mbxParkPrevHidden=document.body.classList.contains('mbx-chrome-hidden');window.__mbxSetChromeHidden(true);}")
	}
}

func (c *Capturer) doUnpark() {
	if park.Unpark() {
		evalOnce("if(window.__mbxSetChromeHidden)window.__mbxSetChromeHidden(!!window.__mbxParkPrevHidden)") // 还原泊车前的 chrome 状态
	}
}

// windowStateLoop（~80ms）：检测主窗最小化（并入 showing 触发淡出），并执行 park 策略：
//
//	策略=park：最小化、非前台、且有人在看 → 自动 Park 屏外保活；网易云变前台(点任务栏) → 自动 Unpark 飞回
//	手动命令：控制端点 ?park=1/0 一次性 park/unpark（任意策略下都生效）——
//	         **该端点当前是关闭的**（796cd75 注释掉了 server.go 的路由注册，理由是策略改
//	         静态 config 后，运行时动态切换无法反映到静态的 /service-status），故
//	         TakeManualParkCmd 恒返回 -1、下面那个 switch 当前不可达。代码是那个 commit
//	         有意保留的（body 明写「保留 handleEffectControl 代码」），别当死代码清掉。
//	无订阅者兜底：若已无人收看但窗口仍泊车屏外 → 自动 Unpark，避免 OBS 停后网易云卡在屏外。
//
// 始终运行（不随订阅者启停），以便任何时候都能把遗留泊车的窗口救回。
func (c *Capturer) windowStateLoop() {
	wasForeground := false
	prevMin := false
	parkAllowed := false // 本次最小化是否允许 park：刚最小化那刻前台是「另一真实 app」=按钮最小化 → 允许
	for {
		// 无人收看且窗口仍泊车 → 飞回（不让网易云卡在屏外）
		if !c.sink.HasEffectSubscribers() && park.IsParked() {
			c.doUnpark()
		}
		strategy := c.sink.EffectStrategy()
		minimized := park.IsMainMinimized()
		fg := park.IsMainForeground()

		// 刚进入最小化那一刻判定：默认 fadeout，仅当确信是按钮最小化(焦点已切到另一真实 app)才允许 park。
		// 任务栏最小化此刻焦点在 shell(explorer)/网易云 → 不允许 park → 走 fadeout(安全、不弹)。
		if minimized && !prevMin {
			parkAllowed = strategy == "park" && park.ForegroundIsOtherApp()
		}
		if !minimized {
			parkAllowed = false
		}
		prevMin = minimized

		// rawMinimized：任何最小化都门控帧（冻结最后一帧，不漏最小化动画/停帧）
		if minimized {
			atomic.StoreInt32(&c.rawMinimized, 1)
		} else {
			atomic.StoreInt32(&c.rawMinimized, 0)
		}
		// minimized（并入 showing 触发淡出）：最小化且本次不走 park（即 fadeout 路径）
		if minimized && !parkAllowed {
			atomic.StoreInt32(&c.minimized, 1)
		} else {
			atomic.StoreInt32(&c.minimized, 0)
		}

		switch c.sink.TakeManualParkCmd() {
		case 1:
			c.doPark()
		case 0:
			c.doUnpark()
		}

		// 按钮最小化 → park（仅 parkAllowed，且必须真有人在看）
		//
		// 订阅者检查不能省：park 的唯一目的就是让屏外窗口继续供帧，没人收看时 park 纯属
		// 白干，而且会被本循环顶部的「无订阅者兜底」（!HasEffectSubscribers() && IsParked() → doUnpark）
		// 在下一 tick（80ms）无条件撤销 —— 而 Unpark 会把 savedPlacement 的
		// swShowMinimized 改写成 swShowNormal（park.go），于是窗口不是回到最小化，是**弹回
		// 屏幕上**。净效果：无订阅者时主播每次点最小化，网易云都在 ~160ms 内自己蹦回来。
		//
		// 加了这个条件后，那条兜底与本行的谓词互斥，同一 tick 不可能都成立，震荡不可达。
		// 不丢任何 park 能力：parkAllowed 只在 minimized 边沿 latch，「先最小化、后接入
		// 订阅者」时它仍为 true，订阅者一接入下一 tick 即 park。
		if parkAllowed && c.sink.HasEffectSubscribers() && minimized && !fg && !park.IsParked() {
			c.doPark()
		}
		// 自动 unpark：网易云从非前台变为前台那一刻（点任务栏/Alt-Tab 切回）；post-park 冷却 1.2s 防弹回
		if park.IsParked() && fg && !wasForeground &&
			time.Now().UnixNano()-atomic.LoadInt64(&c.parkedAtNano) > int64(1200*time.Millisecond) {
			c.doUnpark()
		}
		wasForeground = fg
		time.Sleep(80 * time.Millisecond)
	}
}

// shimInjectLoop：尽早（9222 一可达，赶在网易云页面加载前）就持久注册 preserveDrawingBuffer shim
// 的 document-start 脚本。这样自动恢复的歌、以及之后开的所有歌，其 WebGL context 创建时即带
// preserveDrawingBuffer → purelayerSession 无需任何 reload（reload 在网易云启动期会把它卡死/中断播放）。
//
// 关键：**不门控在取词 player 的 IsConnected**。player 要等 watchdog 重启 + 5s 才连上，那时页面早加载完、
// 甚至已恢复了上一首歌（context 已建、来不及注入 shim）。故这里独立、尽早地直连 9222 注册 addScript，
// 恢复「生命周期合并之前」的早注入时机。安静重试（不打日志、不刷屏）；连接断（网易云重启）后自动重注。
func (c *Capturer) shimInjectLoop() {
	for {
		wsURL, err := screencastWSURL()
		if err != nil {
			time.Sleep(400 * time.Millisecond) // 网易云未起 → 安静快重试，争取赶在页面加载前注册
			continue
		}
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			time.Sleep(400 * time.Millisecond)
			continue
		}
		id := 0
		send := func(method string, params map[string]any) error {
			id++
			req := map[string]any{"id": id, "method": method}
			if params != nil {
				req["params"] = params
			}
			conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			return conn.WriteJSON(req)
		}
		// document-start 注册（addScript 跨页面加载/导航/SPA 路由存活）+ 对当前文档也 eval 一次兜底
		send("Page.enable", nil)
		send("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": glShimJS})
		send("Runtime.evaluate", map[string]any{"expression": glShimJS})
		log.Detail("已尽早注册 preserveDrawingBuffer shim（document-start，免后续 reload）")
		// 保持连接存活以维持 addScript 注册；阻塞读到连接断（网易云退出/重启）→ 重连后重注
		conn.SetReadDeadline(time.Time{})
		for {
			if _, _, e := conn.ReadMessage(); e != nil {
				break
			}
		}
		conn.Close()
		time.Sleep(300 * time.Millisecond)
	}
}

// Run 阻塞运行：有订阅者时连接并截帧，无订阅者时释放并轮询等待。
func (c *Capturer) Run() {
	c.sink.SetEffectIngestHandler(c.ingestFrame) // 纯层帧回传 → 门控 → 广播
	go c.shimInjectLoop()                        // 尽早 document-start 注册 shim（早于页面加载，免后续 reload）
	go c.pollShowingLoop()                       // 独立轮询「特效是否在显示」，与帧流互不干扰
	go c.windowStateLoop()                       // 最小化检测 + park 策略
	log.Info("特效捕获模式: %s", c.mode)
	loggedSessionErr := false // 本轮故障是否已记录（避免网易云重启/不可用时每 2s 刷屏）
	for {
		if !c.sink.HasEffectSubscribers() {
			loggedSessionErr = false
			time.Sleep(500 * time.Millisecond)
			continue
		}
		// 门控生命周期：取词 player 没连上网易云时静默待命（重启/未运行期间不独立探 9222、不刷屏）
		if !c.netEaseUp() {
			loggedSessionErr = false
			time.Sleep(500 * time.Millisecond)
			continue
		}
		var err error
		if c.mode == "screencast" {
			err = c.session()
		} else {
			err = c.purelayerSession()
		}
		if err != nil {
			// 网易云重启/未运行属预期（取词 player 负责拉起），仅本轮故障首次记录一次，恢复后再记
			if !loggedSessionErr {
				log.Detail("特效捕获暂停（网易云不可用，自动重连中）: %v", err)
				loggedSessionErr = true
			}
			time.Sleep(2 * time.Second)
		} else {
			loggedSessionErr = false // 正常结束（无订阅者等），下次故障可再记一次
		}
	}
}

// pollShowingLoop 用一条独立 CDP 连接，每 ~20ms 检查网易云是否正打开详情页，写回 showing。
// 退出详情页时网易云立即设置 transform=translateY(100%)，故能在主页露出前检测到；
// 配合帧门控（不在详情页停发帧），前端冻结在最后一张特效帧。
// 网易云退出（连不上/读失败）时报 showing=false，触发前端淡出。
func (c *Capturer) pollShowingLoop() {
	for {
		if !c.sink.HasEffectSubscribers() {
			c.setShowing(false)
			time.Sleep(400 * time.Millisecond)
			continue
		}
		// 取词 player 没连上网易云 → 直接报不显示，不独立探测 9222
		if !c.netEaseUp() {
			c.setShowing(false)
			time.Sleep(400 * time.Millisecond)
			continue
		}
		wsURL, err := screencastWSURL()
		if err != nil {
			c.setShowing(false) // 网易云未运行 → 淡出
			time.Sleep(1 * time.Second)
			continue
		}
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			c.setShowing(false)
			time.Sleep(1 * time.Second)
			continue
		}
		id := 0
		for c.sink.HasEffectSubscribers() {
			id++
			conn.WriteJSON(map[string]any{
				"id": id, "method": "Runtime.evaluate",
				"params": map[string]any{"expression": showingExprJS, "returnByValue": true},
			})
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			var m struct {
				ID     int `json:"id"`
				Result struct {
					Result struct {
						Value bool `json:"value"`
					} `json:"result"`
				} `json:"result"`
			}
			if err := conn.ReadJSON(&m); err != nil {
				c.setShowing(false) // 网易云退出/连接断 → 淡出
				break
			}
			if m.ID == id {
				// showing = DOM 详情页在显示 且 主窗未最小化（最小化由 windowStateLoop 写入）
				c.setShowing(m.Result.Result.Value && atomic.LoadInt32(&c.minimized) == 0)
			}
			time.Sleep(20 * time.Millisecond)
		}
		conn.Close()
	}
}

// RestoreChrome 在 playercap 优雅退出时调用：还原网易云被双击隐藏的菜单并清理注入。
// best-effort：网易云未运行或连接失败则静默返回。崩溃/强杀不会触发（由双击/重载兜底）。
func (c *Capturer) RestoreChrome() {
	wsURL, err := screencastWSURL()
	if err != nil {
		return
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.WriteJSON(map[string]any{
		"id":     1,
		"method": "Runtime.evaluate",
		"params": map[string]any{
			"expression": "if(window.__mbxSetChromeHidden)window.__mbxSetChromeHidden(false);if(window.__mbxCleanup)window.__mbxCleanup();",
		},
	})
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var discard json.RawMessage
	conn.ReadJSON(&discard) // 等回执，确保 eval 送达后再关闭
	log.Info("已还原网易云菜单（优雅退出）")
}

// devToolsPage CDP /json 返回的页面项
type devToolsPage struct {
	URL                  string `json:"url"`
	WebSocketDebuggerUrl string `json:"webSocketDebuggerUrl"`
}

func screencastWSURL() (string, error) {
	resp, err := http.Get("http://127.0.0.1:9222/json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var pages []devToolsPage
	if err := json.Unmarshal(body, &pages); err != nil {
		return "", err
	}
	for _, p := range pages {
		if p.URL == targetPageURL {
			return p.WebSocketDebuggerUrl, nil
		}
	}
	if len(pages) > 0 {
		return pages[0].WebSocketDebuggerUrl, nil
	}
	return "", io.EOF
}

// purelayerSession 纯层捕获：建立一条 CDP 连接，注入「preserveDrawingBuffer shim + 抓帧循环」，
// 帧由注入脚本经 ingest WS 直接推给 Go（不走本连接）。本连接只负责注入、参数热更、保活探活、
// 以及（仅当连上时已有歌、context 已建）reload 一次让 shim 生效。无订阅者/连接断时返回。
func (c *Capturer) purelayerSession() error {
	wsURL, err := screencastWSURL()
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	id := 0
	call := func(method string, params map[string]any) (json.RawMessage, error) {
		id++
		myID := id
		req := map[string]any{"id": myID, "method": method}
		if params != nil {
			req["params"] = params
		}
		if err := conn.WriteJSON(req); err != nil {
			return nil, err
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			var m struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if err := conn.ReadJSON(&m); err != nil {
				return nil, err
			}
			if m.ID == myID {
				return m.Result, nil
			}
		}
	}
	eval := func(expr string) (json.RawMessage, error) {
		return call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	}

	quality, _, headerClickable, footerClickable, startGen := c.sink.EffectCaptureParams()
	// scale 对纯层无意义（直读 canvas 原生分辨率，不缩放）；仅 quality 影响 JPEG 编码。

	// 保活
	call("Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true})
	call("Page.setWebLifecycleState", map[string]any{"state": "active"})
	call("Runtime.enable", nil)
	call("Page.enable", nil)

	// 注入双击隐藏 chrome 脚本（仍保留：抓的是 canvas 像素与 chrome 无关，但隐藏对主播本机视图仍有用）
	const noPE = ""
	const blockPE = "pointer-events:none !important;"
	headerPE, footerPE := blockPE, blockPE
	if headerClickable {
		headerPE = noPE
	}
	if footerClickable {
		footerPE = noPE
	}
	chromeSrc := fmt.Sprintf(chromeInjectJS, headerPE, footerPE)
	call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": chromeSrc})
	call("Runtime.evaluate", map[string]any{"expression": chromeSrc, "returnByValue": true})

	// 注入纯层抓帧脚本：注册到新文档（reload/导航后自动补注）+ 立即对当前文档运行一次
	capSrc := fmt.Sprintf(captureInjectJS, c.sink.EffectIngestWSURL(), float64(quality)/100.0, captureFPS, captureOutMaxW)
	call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": capSrc})
	eval(capSrc)
	eval("window.__mbxCapPaused=false;")

	// 不再在此 reload：shim 由 shimInjectLoop 在页面加载前就 document-start 注册，所有歌的 context 自带
	// preserveDrawingBuffer。此前「连上时已有歌则 reload」会在网易云启动期触发 Page.reload → 卡死/中断播放，
	// 已移除。极少数 shim 注册晚于自动恢复歌的边角情况：该歌可能略闪，切歌/切模式重建 canvas 即恢复，绝不卡死。

	log.Success("纯层捕获已启动（注入抓帧 q%d 原生分辨率 fps%d）", quality, captureFPS)

	// 维持连接：参数热更（gen 变）/ 无订阅者暂停退出 / 连接断重连
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if !c.sink.HasEffectSubscribers() {
			eval("window.__mbxCapPaused=true;") // 暂停页面内抓帧循环，省网易云渲染进程 CPU
			return nil
		}
		q, _, _, _, gen := c.sink.EffectCaptureParams()
		if gen != startGen {
			startGen = gen
			// 仅热更 q/fps，保留 rs/outw（渲染倍率/输出宽）
			if _, err := eval(fmt.Sprintf("if(window.__mbxCapCfg){window.__mbxCapCfg.q=%f;window.__mbxCapCfg.fps=%d;}", float64(q)/100.0, captureFPS)); err != nil {
				return err
			}
			continue
		}
		// 保活探活：一次轻量 eval，连接断则返回触发重连
		if _, err := eval("1"); err != nil {
			return err
		}
	}
	return nil
}

// session 建立一条 CDP 连接并截帧，直到出错、无订阅者或参数变更。
func (c *Capturer) session() error {
	wsURL, err := screencastWSURL()
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	id := 0
	send := func(method string, params map[string]any) {
		id++
		req := map[string]any{"id": id, "method": method}
		if params != nil {
			req["params"] = params
		}
		conn.WriteJSON(req)
	}
	// call 发送并等待匹配 id 的回复（仅用于截帧开始前的同步设置阶段）
	call := func(method string, params map[string]any) (json.RawMessage, error) {
		id++
		myID := id
		req := map[string]any{"id": myID, "method": method}
		if params != nil {
			req["params"] = params
		}
		if err := conn.WriteJSON(req); err != nil {
			return nil, err
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			var m struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if err := conn.ReadJSON(&m); err != nil {
				return nil, err
			}
			if m.ID == myID {
				return m.Result, nil
			}
		}
	}

	quality, scale, headerClickable, footerClickable, startGen := c.sink.EffectCaptureParams()

	// 保活
	call("Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true})
	call("Page.setWebLifecycleState", map[string]any{"state": "active"})
	call("Runtime.enable", nil)
	call("Page.enable", nil)

	// 注入双击隐藏 chrome 脚本（幂等，会话重启/页面重载自动补注）
	const noPE = ""
	const blockPE = "pointer-events:none !important;"
	headerPE, footerPE := blockPE, blockPE
	if headerClickable {
		headerPE = noPE
	}
	if footerClickable {
		footerPE = noPE
	}
	injectSrc := fmt.Sprintf(chromeInjectJS, headerPE, footerPE)
	// 注册到「每次新文档自动运行」（页面重载/导航后自动补注）
	call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": injectSrc})
	// 并对当前已加载页面立即运行一次
	call("Runtime.evaluate", map[string]any{"expression": injectSrc, "returnByValue": true})

	// 查询网易云页面原生物理分辨率（CSS × DPR），按 scale 计算截帧上限
	maxW, maxH := 1920, 1080
	if res, err := call("Runtime.evaluate", map[string]any{
		"expression":    "JSON.stringify({w:Math.round(innerWidth*devicePixelRatio),h:Math.round(innerHeight*devicePixelRatio)})",
		"returnByValue": true,
	}); err == nil {
		var r struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if json.Unmarshal(res, &r) == nil && r.Result.Value != "" {
			var d struct{ W, H int }
			if json.Unmarshal([]byte(r.Result.Value), &d) == nil && d.W > 0 && d.H > 0 {
				maxW = int(float64(d.W)*scale + 0.5)
				maxH = int(float64(d.H)*scale + 0.5)
			}
		}
	}

	send("Page.startScreencast", map[string]any{
		"format": "jpeg", "quality": quality, "maxWidth": maxW, "maxHeight": maxH, "everyNthFrame": 1,
	})
	conn.SetReadDeadline(time.Time{}) // 清除设置阶段的读超时，帧循环无限阻塞读
	log.Success("特效截帧已开始（%dx%d q%d scale%.2f）", maxW, maxH, quality, scale)

	// 读循环阻塞，无读超时（gorilla 读超时会废连接）；
	// 由 watcher 协程在「无订阅者」或「参数变更」时关闭连接来打断读循环。
	var closed int32
	stopWatcher := make(chan struct{})
	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopWatcher:
				return
			case <-t.C:
				_, _, _, _, gen := c.sink.EffectCaptureParams()
				if !c.sink.HasEffectSubscribers() || gen != startGen {
					atomic.StoreInt32(&closed, 1)
					conn.Close()
					return
				}
			}
		}
	}()
	defer close(stopWatcher)

	for {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Data      string `json:"data"`
				SessionID int    `json:"sessionId"`
			} `json:"params"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			if atomic.LoadInt32(&closed) == 1 {
				return nil // 因无订阅者主动关闭，正常
			}
			return err
		}
		if msg.Method != "Page.screencastFrame" {
			continue
		}
		// 帧门控：仅在网易云正显示详情页特效、未最小化、且不在 park 过渡冻结期时才下发帧；
		// 否则前端冻结在最后一张特效帧，不漏主页/最小化动画/park 后冷渲染。
		if c.isShowing() && atomic.LoadInt32(&c.rawMinimized) == 0 && time.Now().UnixNano() >= atomic.LoadInt64(&c.gateUntilNano) {
			jpeg, decErr := base64.StdEncoding.DecodeString(msg.Params.Data)
			if decErr == nil && len(jpeg) > 0 {
				c.sink.BroadcastEffectFrame(jpeg)
			}
		}
		// 必须 ack，否则网易云停止推送后续帧
		id++
		conn.WriteJSON(map[string]any{
			"id": id, "method": "Page.screencastFrameAck",
			"params": map[string]any{"sessionId": msg.Params.SessionID},
		})
	}
}
