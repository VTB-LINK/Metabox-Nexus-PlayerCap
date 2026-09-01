// Package cdp 连汽水音乐主进程的 Node inspector（9229），经主进程 executeJavaScript 桥进
// rendererMain 主窗口，向字节的 transport 服务请求 sharedState.get('player') 拿全量播放态。
//
// 与 cloudmusic/cdp 的关键差异：
//   - 端口 9222(renderer DevTools) → 9229(主进程 Node inspector)；端点 /json → /json/list。
//   - 目标是主进程 Node context，**没有 DOM**。取 renderer 数据必须在主进程里
//     webContents.executeJavaScript(...) 桥过去——这是返回 Promise 的异步调用，故一律
//     awaitPromise:true。
//   - 提取 JS 用「patch MessagePort.postMessage 抓 channel.port1 → 发 method.invoke →
//     截 method.return」拿 sharedState，见 innerProbeJS。
//   - 收发骨架（gorilla WriteJSON/ReadJSON + id 过滤 + ReadDeadline）沿用 cloudmusic 的可靠写法。
package cdp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/logger"

	"Metabox-Nexus-PlayerCap/i18n"
	"github.com/gorilla/websocket"
)

var log = logger.New("Soda] [CDP")

const inspectorPort = 9229

// devToolsTarget 是 /json/list 的一个条目。
type devToolsTarget struct {
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerUrl string `json:"webSocketDebuggerUrl"`
}

// Client 是一条到主进程 inspector 的 CDP 会话。
type Client struct {
	conn   *websocket.Conn
	msgID  int
	mu     sync.Mutex
	closed bool
}

// cdpResponse 是 Runtime.evaluate 的响应壳（双层 result 嵌套）。
type cdpResponse struct {
	ID     int `json:"id"`
	Result struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	} `json:"result"`
}

// ExtractionData 是一次汽水音乐播放态快照。字段来自 sharedState 的 player 对象。
type ExtractionData struct {
	OK              bool     `json:"ok"`
	Err             string   `json:"err"`
	IsPlaying       bool     `json:"isPlaying"`
	IsLoading       bool     `json:"isLoading"`
	ProgressSeconds float32  `json:"progressSeconds"`
	DurationSeconds float32  `json:"durationSeconds"`
	MediaID         string   `json:"mediaId"` // 稳定歌曲标识（字节数字 ID），换歌检测用
	Name            string   `json:"name"`
	Artists         []string `json:"artists"`
	Album           string   `json:"album"`
	CoverURL        string   `json:"coverUrl"`  // 已拼好的 800x800 封面 URL（公网可取）
	LyricType       string   `json:"lyricType"` // "krc" / "lrc" / ""
	LyricContent    string   `json:"lyricContent"`
	// TranslationLRC 中文翻译轨（tlyric 式独立 LRC，形如 [mm:ss.xx]译文），取自
	// lyrics.translations.cn。按绝对时间戳与主歌词行对齐（见 sodamusic.applySodaTranslations）。
	// 无翻译时为空。
	TranslationLRC string `json:"translationLrc"`
	// Throttled 是**重申之后**目标 webContents 的 backgroundThrottling。正常恒为 false；
	// 若为 true 说明这一版 Electron 不吃 setBackgroundThrottling(false)，最小化久了进度源会掉到
	// 1/60Hz（见 AGENTS §7.7.2）。由 Go 侧只告警一次，不改变取数行为。
	Throttled bool `json:"throttled"`
}

// Connect 连主进程 inspector：GET /json/list → 取 node 目标 → gorilla Dial。
func Connect() (*Client, error) {
	httpClient := &http.Client{Timeout: 2 * time.Second}
	resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", inspectorPort))
	if err != nil {
		return nil, i18n.Errorf("取 /json/list 失败: %w", err)
	}
	defer resp.Body.Close()
	var targets []devToolsTarget
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, i18n.Errorf("解析 /json/list 失败: %w", err)
	}
	if len(targets) == 0 {
		return nil, i18n.Errorf("inspector 无可用目标")
	}
	// 主进程 inspector 只有一个 node 目标（electron/js2c/browser_init）。取第一个 node 型，
	// 兜底取第 0 个。
	wsURL := ""
	for _, t := range targets {
		if t.Type == "node" && t.WebSocketDebuggerUrl != "" {
			wsURL = t.WebSocketDebuggerUrl
			break
		}
	}
	if wsURL == "" {
		wsURL = targets[0].WebSocketDebuggerUrl
	}
	if wsURL == "" {
		return nil, i18n.Errorf("目标无 webSocketDebuggerUrl")
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, i18n.Errorf("WS 连接失败: %w", err)
	}
	return &Client{conn: conn, msgID: 1}, nil
}

// Extract 取一次播放态快照。经主进程桥进 rendererMain 跑 transport 提取。
func (c *Client) Extract() (*ExtractionData, error) {
	valStr, err := c.evaluate(bridgeExpr(), true)
	if err != nil {
		return nil, err
	}
	if valStr == "" || valStr == "null" {
		return nil, i18n.Errorf("提取返回空")
	}
	var data ExtractionData
	if err := json.Unmarshal([]byte(valStr), &data); err != nil {
		return nil, i18n.Errorf("提取结果解析失败: %w", err)
	}
	if !data.OK {
		return nil, i18n.Errorf("提取未成功: %s", data.Err)
	}
	return &data, nil
}

// evaluate 发一次 Runtime.evaluate，返回 result.result.value（字符串）。
// 主进程桥的表达式返回 JSON.stringify(...)，故 value 恒为字符串。
func (c *Client) evaluate(expression string, awaitPromise bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", i18n.Errorf("会话已关闭")
	}
	c.msgID++
	id := c.msgID
	req := map[string]interface{}{
		"id":     id,
		"method": "Runtime.evaluate",
		"params": map[string]interface{}{
			"expression": expression,
			// includeCommandLineAPI 必须为 true：Node 的 inspector 正是经 Command Line API 暴露
			// `require`（及 module/process 等）。缺了它，主进程桥里的 require('electron') 会
			// 报 "require is not defined"。
			"includeCommandLineAPI": true,
			"returnByValue":         true,
			"awaitPromise":          awaitPromise,
			"timeout":               8000,
		},
	}
	if err := c.conn.WriteJSON(req); err != nil {
		c.markClosed()
		return "", i18n.Errorf("写 CDP 请求失败: %w", err)
	}
	// 读到匹配 id 的回复；忽略其它帧。ReadDeadline 兜底防卡死。
	_ = c.conn.SetReadDeadline(time.Now().Add(9 * time.Second))
	for {
		var res cdpResponse
		if err := c.conn.ReadJSON(&res); err != nil {
			c.markClosed()
			return "", i18n.Errorf("读 CDP 回复失败: %w", err)
		}
		if res.ID != id {
			continue
		}
		if len(res.Result.ExceptionDetails) > 0 && string(res.Result.ExceptionDetails) != "null" {
			// 注意用 %s 而非把内容当格式串（内容里可能含 %）。
			return "", i18n.Errorf("求值异常: %s", res.Result.ExceptionDetails)
		}
		if len(res.Result.Result.Value) == 0 {
			return "", nil
		}
		// value 是 JSON 字符串（主进程桥 JSON.stringify 的产物）→ 先解成 Go string。
		var s string
		if err := json.Unmarshal(res.Result.Result.Value, &s); err != nil {
			return "", i18n.Errorf("求值返回非字符串（type=%s）: %w", res.Result.Result.Type, err)
		}
		return s, nil
	}
}

// —— 关于 bridgeExpr 里那一行 setBackgroundThrottling(false) ——
//
// **这是本包唯一会改变汽水行为的调用**，尽力而为：设不上只体现为 ExtractionData.Throttled
// 为 true，由上层告警一次，不影响取数。
//
// 为什么必须做：Chromium 对隐藏页面有 intensive wake-up throttling。真机对照实验
// （2026-08-08，主窗口全程最小化）——不调时闲置约 4.8 分钟后节流生效：
//
//	[  288.1s] progress= 58.021 dv= +1.001 dt=  1064.6ms   ← 前 4.8 分钟正常
//	[  316.2s] progress= 86.021 dv=+28.000 dt= 28027.8ms   ← 节流生效
//	[  376.2s] progress=146.022 dv=+60.000 dt= 60031.3ms
//	[  436.1s] progress=206.019 dv=+59.997 dt= 59934.0ms
//
// 也就是说主播把汽水最小化、五分钟不碰它，progressSeconds 就从 1Hz 掉到 1/60Hz。本地时钟外推
// 能让歌词在这段时间里照常推进（§7.7.1），但 seek / 暂停一律迟至下一次采样（最长 60s）才被
// 发现，且日志里一个字都没有。直播场景下这是实打实的缺陷。调用后：已被节流的状态下 300ms 内
// 恢复 1Hz，随后最小化闲置 8 分钟共 478 个采样、最大间隔 1121ms、无一超过 2s。
//
// 为什么用运行时 API 而不是照网易云加启动参数：网易云那条路是「杀掉进程 + 带 argv 重启」
// （watchdog/process.go），对汽水意味着**在直播中杀掉主播的播放器**；而且汽水有原生反调试，
// argv 里加东西是否会触发自杀未经验证（§7.7 当初正因它自杀才放弃了重启加参数）。
// 这里只是对它自己的 webContents 调一个官方 Electron API：不写内存、不碰 argv、不改注册表，
// 进程退出即失效。相对 §0.1「不改汽水任何内存/状态」，这是一处**刻意的、边界清楚的例外**。
//
// 「Electron 里加载后再设需 reload 才生效」这条传闻对本版本不成立（上面的实测就是反例）。
// 别据它把实现改成加载前设置或补一次 reload —— reload 汽水的渲染器会打断播放。

// markClosed 标记会话已断**并真的关掉底层连接**。调用方须已持有 c.mu。
//
// **别退回裸 `c.closed = true`。** 原先 evaluate 的两条 I/O 失败路径只置标志不关 conn，
// 而 Close() 又把关闭动作包在 `if !c.closed` 里 —— 两者合起来的后果是：**凡是被错误路径
// 终结的连接，就再也没有显式关闭点**。9s ReadDeadline 到期（渲染器繁忙、桥调用排队）走的
// 正是这条路，此时 TCP 其实还是健康的：runSession 因 IsClosed() 返回 → Start 调 Close() →
// 被 closed 挡掉 → socket 与**汽水主进程侧那个 inspector 会话**一起滞留，只能等 netFD 的
// 终结器在某次 GC 后收掉，时机不可预期。而汽水抖动期每约 11s 就会累加一条。
//
// 滞留压在汽水的主进程事件循环上——它是汽水全部 IPC 的那一条，比泄漏一个 fd 严重。
func (c *Client) markClosed() {
	if !c.closed {
		c.conn.Close()
		c.closed = true
	}
}

// Close 关闭会话（幂等）。
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.markClosed()
}

// IsClosed 报告会话是否已断。
func (c *Client) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// bridgeExpr 构造主进程侧表达式：找 rendererMain 主窗口 → executeJavaScript(内层探针) → JSON.stringify。
// 内层探针经 base64 内嵌（atob 解回），免去 JS 里再套一层字符串转义。atob 是 Node16+ 全局，
// Electron 主进程可用；内层探针纯 ASCII，atob 解码无损。
func bridgeExpr() string {
	b64 := base64.StdEncoding.EncodeToString([]byte(innerProbeJS))
	return "(async()=>{" +
		"const {webContents}=require('electron');" +
		"const all=webContents.getAllWebContents();" +
		"let target=null;" +
		"for(const wc of all){try{const u=wc.getURL()||'';if(u.indexOf('main.html')>=0){target=wc;break;}}catch(e){}}" +
		"if(!target){for(const wc of all){try{const u=wc.getURL()||'';if(u.indexOf('taskbar')<0&&u.indexOf('.html')>=0){target=wc;break;}}catch(e){}}}" +
		"if(!target)return JSON.stringify({err:'no-main-window'});" +
		// 每次取数都重申一次「关掉后台节流」，并把结果一并带回（见 ExtractionData.Throttled）。
		// **不是多余的**：这个开关随 webContents 生命周期存在，而我们连的是**主进程** inspector
		// ——主进程不退出，CDP 会话就不断，只在重连时设一次的话，汽水一旦重建 rendererMain
		// 窗口，新 webContents 就回到默认的节流态，而我们永远不会知道。放在这里天然自愈，
		// 且作用域恰好是我们真正读的那一个窗口（别改回遍历 getAllWebContents：那会把
		// taskbar 等我们从不读的窗口也解除节流，纯粹白烧主播的 CPU，也越过了 §7.7.2 给
		// §0.1 例外划的边界——那条例外之所以安全，靠的就是它窄）。
		"let bt=null;try{target.setBackgroundThrottling(false);bt=target.backgroundThrottling;}catch(e){}" +
		"try{const r=await target.executeJavaScript(atob(\"" + b64 + "\"),true);" +
		"if(r&&typeof r==='object')r.throttled=bt;return JSON.stringify(r);}" +
		"catch(e){return JSON.stringify({err:'exec:'+String(e&&e.message||e)});}" +
		"})()"
}

// innerProbeJS 在 rendererMain 主窗口里跑：patch MessagePort.postMessage 抓 channel.port1，
// 发 method.invoke 请求 sharedState.get('player')，截 method.return 归一化返回。
// 返回一个 Promise（IIFE），executeJavaScript 会 await 它。
//
// **本常量必须保持纯 ASCII，注释一律写在这里、不要写进字符串。** bridgeExpr 用 Go 的
// base64 编码它、再由页面里的 `atob` 解回，而 `atob` 只认 Latin-1：字符串里出现任何中文，
// 解出来就是乱码 → JS 解析失败 → **inspector 直接掐断这条 WS**（实测症状：日志里每 2 秒
// 一条「CDP 连接成功」不停重连，Extract 报 `wsarecv: 远程主机强迫关闭了一个现有的连接`，
// 而所有端点的 data 全空）。这条不变量被违反过一次，就是这么发现的。
//
// 关于 cleanup：它是「把挂在汽水侧的监听器摘掉」的唯一出口，**必须在任何可能抛出的语句
// 之前就位**，且每条返回路径都走它（含最外层 catch）。原先清理只挂在 setTimeout 里、而
// setTimeout 注册在 sendTransport 之后：sendTransport 一抛，监听器就永久留在汽水的
// channel.port1 上；而 Go 侧把提取失败当瞬时错误原地重试（200ms 一次）→ 每秒往汽水渲染器里
// 堆 5 个死闭包，一小时 1.8 万个，此后汽水每收一条真实 transport 消息都要把它们全跑一遍。
// 那是往别人的进程里单向堆积，与包注释「绝不动汽水进程状态」直接冲突。
// 先赋成空函数再重定义，是为了让最外层 catch 在「port 还没拿到就抛」时也能安全调用它——
// 否则 catch 里会再抛一个 "cleanup is not a function"，Promise 永不 settle，整个
// executeJavaScript 要挂到 CDP 的 8s/9s 兜底才结束。
const innerProbeJS = `(function(){return new Promise(function(resolve){
var cleanup=function(){};
try{
var proto=MessagePort.prototype;var orig=proto.postMessage;var port=null;
proto.postMessage=function(){port=this;return orig.apply(this,arguments);};
try{window.transportPort.sendTransport({__sodaCap:1});}catch(e){}
proto.postMessage=orig;
if(!port){resolve({err:'no-port'});return;}
cleanup=function(){try{port.removeEventListener('message',onMsg);}catch(e){}};
var reqId='sodaget-'+Math.random().toString(36).slice(2)+Date.now();
var done=false;
var onMsg=function(e){
var d=e.data;
if(!d||d.type!=='method.return'||d.requestId!==reqId)return;
done=true;cleanup();
var r=d.return;
if(!r||r.type!=='success'){resolve({err:'ret'});return;}
var p=r.result||{};var md=p.mediaDetail||{};var pl=md.playable||{};var ly=md.lyrics||{};var al=pl.album||{};
var cover='';var cu=pl.cover_url;
if(typeof cu==='string'){cover=cu;}
else if(cu&&cu.uri&&cu.urls&&cu.urls.length){cover=cu.urls[0]+cu.uri+'~'+(cu.template_prefix||'')+'-crop-center:800:800.jpg';}
var artists=[];var pa=pl.artists||[];
for(var i=0;i<pa.length;i++){if(pa[i]&&pa[i].name)artists.push(pa[i].name);}
resolve({
ok:true,
isPlaying:!!p.isPlaying,isLoading:!!p.isLoading,
progressSeconds:p.progressSeconds,durationSeconds:p.durationSeconds,
mediaId:(p.mediaId!=null?String(p.mediaId):(pl.id!=null?String(pl.id):'')),
name:pl.name||'',album:(al.name||''),artists:artists,coverUrl:cover,
lyricType:ly.type||'',lyricContent:ly.content||'',
translationLrc:((ly.translations&&typeof ly.translations==='object'&&ly.translations.cn)?ly.translations.cn:'')
});
};
port.addEventListener('message',onMsg);
setTimeout(function(){if(!done){cleanup();resolve({err:'timeout'});}},2500);
try{window.transportPort.sendTransport({type:'method.invoke',fromWorkerId:'rendererMain',toServiceId:'sharedState',methodName:'get',requestId:reqId,arguments:['player'],callbacks:{}});}
catch(e){cleanup();resolve({err:'send:'+String(e&&e.message||e)});return;}
}catch(e){cleanup();resolve({err:'ex:'+String(e&&e.message||e)});}
});})()`
