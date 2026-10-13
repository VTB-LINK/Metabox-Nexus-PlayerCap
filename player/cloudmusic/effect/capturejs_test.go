package effect

// 本文件用 node 跑注入网易云页面的抓帧脚本（captureScript 的输出，即精简后实际发出的那一份），
// 在桩出来的页面环境里按虚拟时钟驱动：
// rAF 与 toBlob 回调按各自的时刻依次发生，toBlob 的回调延迟、各桩何时抛错由用例给定。找不到 node 就跳过。
// 只验证脚本自身的逻辑（issue #98）；网易云里 toBlob 何时被拖住、当场编的真实耗时、对网易云帧率的影响
// 只能真机验。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	capURLA = "ws://127.0.0.1:8765/cloudmusicv3/effect-ingest?t=aaaa"
	capURLB = "ws://127.0.0.1:8765/cloudmusicv3/effect-ingest?t=bbbb"
)

// capHarnessJS 页面环境的桩与驱动。
//   - 特效 canvas 每个 rAF 拍换一帧（src.frame 加一），快照记下拍号，发出的帧据此可比新旧。
//   - toBlob 在 opt.blobDelay(调用时刻) 毫秒后回调（Infinity 为永不回调），回调前该画布记为在途。
//   - toDataURL 同步返回 JPEG 的 data URL，内容是快照的拍号；opt.syncCost 毫秒的编码耗时计入发出时刻。
//   - WebSocket 构造即打开；地址不是 ws/wss 时同浏览器一样抛 SyntaxError，记进 badSockets。
//   - advance(ms) 按时刻先后处理定时事件（toBlob 回调、arrayBuffer 兑现）与 rAF 拍，每件事之后清空微任务。
const capHarnessJS = `
var T=0, nextRaf=0, rafQ=[], timers=[], timerSeq=0, marks={};
var sends=[], sockets=[], badSockets=[], encodes=[], violations=[], canvases=[];
function no(){return false;}
var opt={rafGap:function(t){return 1000/60;}, blobDelay:function(at){return 20;}, abDelay:function(t){return 0;}, syncCost:0,
  toBlobThrows:no, toDataURLThrows:no, drawThrows:no, nullBlob:no, abReject:no, sendThrows:no,
  srcPresent:function(t){return true;}};
globalThis.window=globalThis;
Object.defineProperty(globalThis,'performance',{configurable:true,writable:true,value:{now:function(){return T;}}});
globalThis.requestAnimationFrame=function(cb){rafQ.push(cb);return rafQ.length;};
function later(t,fn){timers.push({t:t,n:timerSeq++,fn:fn});}
function HTMLCanvasElement(){this.width=300;this.height=150;this.pending=0;this.frame=0;this.content=-1;this.drawn=null;this.id=canvases.length;canvases.push(this);}
globalThis.HTMLCanvasElement=HTMLCanvasElement;
HTMLCanvasElement.prototype.getContext=function(type,attrs){
  var cv=this;
  return {drawImage:function(img,x,y,w,h){
    if(opt.drawThrows(T))throw new Error('drawImage failed');
    if(cv.pending>0)violations.push('drawImage into canvas '+cv.id+' with a pending toBlob at '+T);
    cv.content=img.frame; cv.drawn=[w,h];
  }};
};
HTMLCanvasElement.prototype.toBlob=function(cb,type,q){
  if(opt.toBlobThrows(T))throw new Error('toBlob failed');
  var cv=this, frame=cv.content, at=T, rec={kind:'blob',at:at,frame:frame,canvas:cv.id,type:type,q:q,size:cv.drawn};
  cv.pending++;
  encodes.push(rec);
  var d=opt.blobDelay(at);
  if(d===Infinity)return;
  later(at+d,function(){
    cv.pending--; rec.doneAt=T;
    if(opt.nullBlob(T)){cb(null);return;}
    var rej=opt.abReject(T), ad=opt.abDelay(T);
    cb({arrayBuffer:function(){return new Promise(function(res,rj){
      var settle=function(){if(rej)rj(new Error('read failed'));else res({frame:frame});};
      if(ad>0)later(T+ad,settle);else settle();
    });}});
  });
};
HTMLCanvasElement.prototype.toDataURL=function(type,q){
  if(this.pending>0)violations.push('toDataURL on canvas '+this.id+' with a pending toBlob at '+T);
  encodes.push({kind:'sync',at:T,frame:this.content,canvas:this.id,type:type,q:q,size:this.drawn});
  if(opt.toDataURLThrows(T))throw new Error('toDataURL failed');
  T+=opt.syncCost;
  return 'data:image/jpeg;base64,'+Buffer.from('frame '+this.content).toString('base64');
};
var src=new HTMLCanvasElement(); src.width=1062; src.height=753;
globalThis.document={
  createElement:function(tag){return new HTMLCanvasElement();},
  querySelector:function(sel){return sel==='#lyric-effect-canvas-id'&&opt.srcPresent(T)?src:null;}
};
function WebSocket(url){
  if(!/^wss?:\/\//.test(url)){badSockets.push({at:T,url:url});throw new SyntaxError('invalid url: '+url);}
  this.url=url; this.readyState=1; this.id=sockets.length; sockets.push(this);
}
WebSocket.prototype.send=function(d){
  if(this.readyState!==1)throw new Error('send on a closed socket');
  if(opt.sendThrows(T))throw new Error('send failed');
  var p='data:image/jpeg;base64,';
  var s=typeof d==='string'
    ?{kind:'sync',prefixOK:d.indexOf(p)===0,frame:+Buffer.from(d.slice(p.length),'base64').toString().slice(6)}
    :{kind:'blob',frame:d.frame};
  s.at=T; s.ws=this.id; sends.push(s);
};
WebSocket.prototype.close=function(){this.readyState=3;};
globalThis.WebSocket=WebSocket;
async function flush(){for(var i=0;i<10;i++)await null;}
async function advance(ms){
  var end=T+ms;
  for(;;){
    var ti=-1;
    for(var i=0;i<timers.length;i++)if(ti<0||timers[i].t<timers[ti].t||(timers[i].t===timers[ti].t&&timers[i].n<timers[ti].n))ti=i;
    var tt=ti<0?Infinity:timers[ti].t, t=Math.min(tt,nextRaf);
    if(t>end)break;
    T=Math.max(T,t);
    if(tt<=nextRaf){timers.splice(ti,1)[0].fn();}
    else{nextRaf=T+opt.rafGap(T); src.frame++; var q=rafQ; rafQ=[]; for(var j=0;j<q.length;j++)q[j](T);}
    await flush();
  }
  T=end;
}
function mark(name){marks[name]=T;}
// inject 同 purelayerSession：先跑脚本，再写 __mbxCapPaused=false。
function inject(name){(0,eval)(SCRIPTS[name]); window.__mbxCapPaused=false;}
function out(extra){
  process.stdout.write(JSON.stringify({sends:sends,encodes:encodes,badSockets:badSockets,
    sockets:sockets.map(function(s){return {url:s.url,state:s.readyState};}),
    violations:violations,canvases:canvases.length,rafPending:rafQ.length,
    stats:window.__mbxCapStats||null,marks:marks,extra:extra||null}));
}
`

type capSend struct {
	Kind     string  `json:"kind"`
	Frame    int     `json:"frame"`
	At       float64 `json:"at"`
	WS       int     `json:"ws"`
	PrefixOK bool    `json:"prefixOK"`
}

type capEncode struct {
	Kind   string   `json:"kind"`
	At     float64  `json:"at"`
	Frame  int      `json:"frame"`
	Canvas int      `json:"canvas"`
	Type   string   `json:"type"`
	Q      float64  `json:"q"`
	Size   []int    `json:"size"`
	DoneAt *float64 `json:"doneAt"` // toBlob 回调的时刻；nil 为没回调
}

type capRun struct {
	Sends      []capSend   `json:"sends"`
	Encodes    []capEncode `json:"encodes"`
	BadSockets []struct {
		At  float64 `json:"at"`
		URL string  `json:"url"`
	} `json:"badSockets"`
	Sockets []struct {
		URL   string `json:"url"`
		State int    `json:"state"`
	} `json:"sockets"`
	Violations []string `json:"violations"`
	Canvases   int      `json:"canvases"`
	RafPending int      `json:"rafPending"`
	Stats      *struct {
		Blob  int `json:"blob"`
		Sync  int `json:"sync"`
		Stale int `json:"stale"`
	} `json:"stats"`
	Marks map[string]float64 `json:"marks"`
	Extra map[string]any     `json:"extra"`
}

// runCapture 在 node 里装好桩，按 scripts 备好待注入的脚本，执行 scenario（async 函数体，可返回 extra）。
func runCapture(t *testing.T, scripts map[string]string, scenario string) capRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found; skipping JS behaviour check")
	}
	sj, err := json.Marshal(scripts)
	if err != nil {
		t.Fatal(err)
	}
	prog := capHarnessJS + "var SCRIPTS=" + string(sj) + ";\n(async function(){\n" + scenario +
		"\n})().then(out).catch(function(e){process.stderr.write(String(e&&e.stack||e));process.exit(3);});\n"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-")
	cmd.Stdin = strings.NewReader(prog)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("node failed: %v\nstderr: %s", err, stderr.String())
	}
	var r capRun
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("bad node output %q: %v", stdout.String(), err)
	}
	return r
}

// v4 是本版注入的脚本（captureScript 的输出）。
func v4(url string) string { return captureScript(url, 95) }

// sendsBetween 取 [from, to) 内发出的帧。
func sendsBetween(r capRun, from, to float64) []capSend {
	var out []capSend
	for _, s := range r.Sends {
		if s.At >= from && s.At < to {
			out = append(out, s)
		}
	}
	return out
}

func encodesOf(r capRun, kind string) []capEncode {
	var out []capEncode
	for _, e := range r.Encodes {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// maxGap 相邻两次发帧的最大间隔（含第一帧距 from 的间隔）。
func maxGap(sends []capSend, from float64) float64 {
	g, prev := 0.0, from
	for _, s := range sends {
		if s.At-prev > g {
			g = s.At - prev
		}
		prev = s.At
	}
	return g
}

// checkMonotonic 发出的帧必须一帧比一帧新：旧帧排在新帧之后，前端就会倒退一帧。
func checkMonotonic(t *testing.T, sends []capSend) {
	t.Helper()
	for i := 1; i < len(sends); i++ {
		if sends[i].Frame <= sends[i-1].Frame {
			t.Fatalf("frame %d (%s, t=%.1f) sent after frame %d (%s, t=%.1f)",
				sends[i].Frame, sends[i].Kind, sends[i].At, sends[i-1].Frame, sends[i-1].Kind, sends[i-1].At)
		}
	}
}

func checkClean(t *testing.T, r capRun) {
	t.Helper()
	checkSane(t, r)
	checkJPEG(t, r, 0)
}

// checkSane 没碰在途的画布、计数在、文本帧都是 JPEG 的 data URL。
func checkSane(t *testing.T, r capRun) {
	t.Helper()
	if len(r.Violations) > 0 {
		t.Fatalf("canvas misuse: %v", r.Violations)
	}
	if r.Stats == nil {
		t.Fatal("window.__mbxCapStats missing")
	}
	for _, s := range r.Sends {
		if s.Kind == "sync" && !s.PrefixOK {
			t.Fatalf("sync frame at %.1f is not a JPEG data URL", s.At)
		}
	}
}

// checkJPEG from 之后的编码一律是 q0.95 的 JPEG（两条路径同一质量，取本会话注入的参数）。
func checkJPEG(t *testing.T, r capRun, from float64) {
	t.Helper()
	for _, e := range r.Encodes {
		if e.At >= from && (e.Type != "image/jpeg" || e.Q != 0.95) {
			t.Fatalf("%s encode at %.1f used %q q=%v, want image/jpeg q=0.95", e.Kind, e.At, e.Type, e.Q)
		}
	}
}

// TestCaptureScriptFormatsCleanly 脚本经 fmt.Sprintf 格式化，各参数须落到各自的位置。
// 脚本里多出百分号由 go vet 拦（go test 自带，实测为构建失败）；本例拦的是同类型参数对调。
//
// 变异自证：captureScript 里对调 captureStallMs 与 captureSyncGapMs，红。
func TestCaptureScriptFormatsCleanly(t *testing.T) {
	s := v4(capURLA)
	if strings.Contains(s, "%!") {
		t.Fatalf("captureInjectJS has stray verbs:\n%s", s)
	}
	for _, want := range []string{
		fmt.Sprintf("STALL=%d, SYNC_GAP=%d", captureStallMs, captureSyncGapMs),
		fmt.Sprintf("{q:0.950000,fps:%d,outw:%d}", captureFPS, captureOutMaxW),
		fmt.Sprintf("var WS_URL=%q", capURLA),
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script lacks %q", want)
		}
	}
	// 阈值须大于两个抓帧间隔，否则正常出帧的间隔就会触发它；停顿上限由下面的模拟守。
	if captureStallMs <= 2*1000/captureFPS {
		t.Fatalf("captureStallMs=%d is not above two capture intervals (%dms)", captureStallMs, 2*1000/captureFPS)
	}
}

// TestCaptureJSIdleStaysOnToBlob 流没有断时一律走 toBlob，一帧也不当场编。回调延迟取不忙时的实测范围
// （中位 21～28ms、p90 约 40ms），也取到稍忙时仍在持续回来的 79、99ms：三块画布的池在 99ms 内仍出得满 30 帧，
// 切到限频的当场编只会降帧率（#114）。rAF 慢到 20 帧时也一样。回调更慢但仍在持续回来的见
// TestCaptureJSSlowButAlive；流真断了（toBlob 路径超过 captureStallMs 没有出东西）才当场编，见
// TestCaptureJSStallSwitchesToSyncEncode。
//
// 变异自证（均实测）：判定改回 #98 初版的「最早在途的 toBlob 等过 60ms」，79ms、99ms 两例红；判定改成
// 「有在途即当场编」、阈值除以 4，空闲各例红。
func TestCaptureJSIdleStaysOnToBlob(t *testing.T) {
	cases := []struct {
		name   string
		raf    string
		delay  int
		minOut int
	}{
		{"60Hz, 20ms callbacks", "1000/60", 20, 85},
		{"60Hz, 28ms callbacks (idle median)", "1000/60", 28, 85},
		{"60Hz, 40ms callbacks (idle p90)", "1000/60", 40, 85},
		{"60Hz, callbacks 30ms under the stall threshold", "1000/60", captureStallMs - 30, 85},
		{"120Hz, 20ms callbacks", "1000/120", 20, 85},
		{"120Hz, callbacks 30ms under the stall threshold", "1000/120", captureStallMs - 30, 85},
		{"busy page at 20 rAF/s, 20ms callbacks", "50", 20, 55},
		{"60Hz, 79ms callbacks", "1000/60", 79, 80},
		{"60Hz, 99ms callbacks", "1000/60", 99, 75},
		{"120Hz, 99ms callbacks", "1000/120", 99, 75},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCapture(t, map[string]string{"a": v4(capURLA)}, fmt.Sprintf(`
opt.rafGap=function(t){return %s;};
opt.blobDelay=function(at){return %d;};
inject('a');
await advance(3000);`, tc.raf, tc.delay))
			checkClean(t, r)
			if n := len(encodesOf(r, "sync")); n != 0 {
				t.Fatalf("%d sync encodes while callbacks were never held up", n)
			}
			if len(r.Sends) < tc.minOut {
				t.Fatalf("only %d frames in 3s, want >= %d", len(r.Sends), tc.minOut)
			}
			checkMonotonic(t, r.Sends)
			if r.Stats.Blob != len(r.Sends) || r.Stats.Sync != 0 || r.Stats.Stale != 0 {
				t.Fatalf("stats %+v with %d frames sent", *r.Stats, len(r.Sends))
			}
		})
	}
}

// TestCaptureJSStallSwitchesToSyncEncode 页面一忙 toBlob 回调被拖住、流真的断了（在途的都不回来，这段时间
// rAF 也变慢）：超过 captureStallMs 没有帧就改为当场编，流里不出现 250ms 以上的停顿；当场编另用一块画布、至多每
// captureSyncGapMs 一次；被拖住的旧帧晚到时丢弃；拖住解除后回到 toBlob。
//
// 变异自证（均实测）：去掉当场编分支（只剩 toBlob，回到修复前），最大间隔约 1 秒而红；当场编改用池里的
// cans[0]，画布误用红；去掉 send 里的编号比较，帧序倒退而红；去掉 SYNC_GAP 判定，间隔不足而红。
func TestCaptureJSStallSwitchesToSyncEncode(t *testing.T) {
	cases := []struct {
		name     string
		raf      string
		delay    string
		clearsAt float64
	}{
		// 1000～2000ms 间发起的 toBlob 一律拖到 2050ms 才回调（网易云实测停顿 0.25～1.1 秒）。
		{"60Hz, busy rAF during the stall", "t>=1000&&t<2000?60:1000/60", "at>=1000&&at<2000?2050-at:20", 2050},
		{"120Hz", "1000/120", "at>=1000&&at<2000?2050-at:20", 2050},
		// 忙时 rAF 间隔实测最大 53～96ms：最后一张帧出来之后，等过阈值的第一拍才当场编，间隔不超过
		// captureStallMs + rAF 间隔 + 编码耗时。不均匀间隔见 TestCaptureJSStallUnevenRaf。
		{"rAF at 96ms during the stall", "t>=1000&&t<2000?96:1000/60", "at>=1000&&at<2000?2050-at:20", 2050},
		{"rAF just under the threshold", fmt.Sprintf("t>=1000&&t<2000?%d:1000/60", captureStallMs-1), "at>=1000&&at<2000?2050-at:20", 2050},
		{"rAF at 45ms", "t>=1000&&t<2000?45:1000/60", "at>=1000&&at<2000?2050-at:20", 2050},
		// 短停顿：三块画布都被拖住约 250ms（1000～1100 间发起的三个 toBlob 拖到 1280ms），流真的断了一小段。
		{"short stall: all three in flight held for ~250ms", "1000/60", "at>=1000&&at<1100?1280-at:20", 1280},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCapture(t, map[string]string{"a": v4(capURLA)}, fmt.Sprintf(`
opt.rafGap=function(t){return %s;};
opt.blobDelay=function(at){return %s;};
inject('a');
await advance(3500);`, tc.raf, tc.delay))
			checkClean(t, r)
			checkMonotonic(t, r.Sends)
			g := maxGap(r.Sends, 100)
			if g >= 250 {
				t.Fatalf("stream stalled for %.1fms (>= 250)", g)
			}
			syncs := encodesOf(r, "sync")
			t.Logf("max gap %.1fms; %d frames sent in [1100,2000); %d sync encodes; stats %+v",
				g, len(sendsBetween(r, 1100, 2000)), len(syncs), *r.Stats)
			if len(syncs) == 0 {
				t.Fatal("no sync encode during the stall")
			}
			pool := map[int]bool{}
			for _, e := range encodesOf(r, "blob") {
				pool[e.Canvas] = true
			}
			// 停顿前最后一次回调的时刻：当场编不会早于它之后 captureStallMs。
			lastCb := 0.0
			for _, e := range encodesOf(r, "blob") {
				if e.At < 1000 && e.DoneAt != nil && *e.DoneAt > lastCb {
					lastCb = *e.DoneAt
				}
			}
			for i, e := range syncs {
				if e.At <= lastCb+captureStallMs || e.At > tc.clearsAt {
					t.Fatalf("sync encode at %.1f outside the stall (%.1f+%d, %.0f]", e.At, lastCb, captureStallMs, tc.clearsAt)
				}
				if pool[e.Canvas] {
					t.Fatalf("sync encode at %.1f used pool canvas %d", e.At, e.Canvas)
				}
				if i > 0 && e.At-syncs[i-1].At < captureSyncGapMs {
					t.Fatalf("sync encodes %.1fms apart (< %d)", e.At-syncs[i-1].At, captureSyncGapMs)
				}
			}
			// 被拖住的 toBlob（比第一张当场编的帧旧、在它之后才回调）晚到时一帧不发。
			sent := map[int]bool{}
			for _, s := range r.Sends {
				sent[s.Frame] = true
			}
			stuck := 0
			for _, e := range encodesOf(r, "blob") {
				if e.Frame < syncs[0].Frame && (e.DoneAt == nil || *e.DoneAt > syncs[0].At) {
					stuck++
					if sent[e.Frame] {
						t.Fatalf("held-up toBlob frame %d (issued %.1f) was sent after newer sync frames", e.Frame, e.At)
					}
				}
			}
			if stuck == 0 || r.Stats.Stale < stuck {
				t.Fatalf("%d held-up toBlob frames, stats %+v", stuck, *r.Stats)
			}
			after := sendsBetween(r, tc.clearsAt+50, 3500)
			if len(after) < 30 {
				t.Fatalf("only %d frames after the stall cleared", len(after))
			}
			for _, s := range after {
				if s.Kind != "blob" {
					t.Fatalf("%s frame at %.1f after the stall cleared", s.Kind, s.At)
				}
			}
			if r.Stats.Sync != len(syncs) || r.Stats.Blob+r.Stats.Sync != len(r.Sends) {
				t.Fatalf("stats %+v, %d sync encodes, %d frames sent", *r.Stats, len(syncs), len(r.Sends))
			}
			if r.Canvases != 1+3+1 {
				t.Fatalf("%d canvases (source + pool of 3 + 1 for sync encode expected)", r.Canvases)
			}
		})
	}
}

// TestCaptureJSStallUnevenRaf rAF 间隔不均匀时的断流（captureStallMs 注释里的最坏情形）：最后一张正常帧
// 1000ms 抓、1020ms 发出；此后 rAF 按 96、79ms 交替（96ms 是忙时实测的最大间隔），1000ms 之后发起的 toBlob
// 一律拖到 1400ms；当场编按 13ms 计（1062×753 实测中位）。最大帧间隔须不超过 captureStallMs + 96 + 13
// （距最后一张帧的上界，229ms）加 5ms 的取整余量，即低于 250ms。
//
// 变异自证（均实测）：captureStallMs 取 150，约 260ms 而红；计时改成「有新帧从空池抓起就重新起算」，rAF 慢造成的
// 空档会再叠一遍，约 264ms 而红。
func TestCaptureJSStallUnevenRaf(t *testing.T) {
	r := runCapture(t, map[string]string{"a": v4(capURLA)}, `
var k=0;
opt.rafGap=function(t){return t<1000?20:[96,79][k++%2];};
opt.blobDelay=function(at){return at>1000&&at<1400?1400-at:20;};
opt.syncCost=13;
inject('a');
await advance(2000);`)
	checkClean(t, r)
	checkMonotonic(t, r.Sends)
	if len(sendsBetween(r, 1019, 1021)) != 1 || len(encodesOf(r, "sync")) == 0 {
		t.Fatalf("scenario not as designed: last good frame at 1020? %d sync encodes", len(encodesOf(r, "sync")))
	}
	g := maxGap(r.Sends, 40)
	t.Logf("max gap %.1fms; stats %+v", g, *r.Stats)
	if bound := float64(captureStallMs + 96 + 13 + 5); g > bound {
		t.Fatalf("stream stalled for %.1fms (> %.0f)", g, bound)
	}
}

// TestCaptureJSSyncRateCapped toBlob 一去不回时一直当场编，但每秒至多 1000/captureSyncGapMs 次，
// 在途的 toBlob 不超过画布池的 3 个（不无界累积）。
//
// 变异自证：去掉 SYNC_GAP 判定，每拍都编而红。
func TestCaptureJSSyncRateCapped(t *testing.T) {
	for _, raf := range []string{"1000/60", "1000/120", "1000/240"} {
		t.Run("rAF "+raf, func(t *testing.T) {
			r := runCapture(t, map[string]string{"a": v4(capURLA)}, fmt.Sprintf(`
opt.rafGap=function(t){return %s;};
opt.blobDelay=function(at){return Infinity;};
inject('a');
await advance(2000);`, raf))
			checkClean(t, r)
			checkMonotonic(t, r.Sends)
			syncs := encodesOf(r, "sync")
			if max := 2000/captureSyncGapMs + 1; len(syncs) > max || len(syncs) < max/2 {
				t.Fatalf("%d sync encodes in 2s, want between %d and %d", len(syncs), max/2, max)
			}
			for i := 1; i < len(syncs); i++ {
				if syncs[i].At-syncs[i-1].At < captureSyncGapMs {
					t.Fatalf("sync encodes %.1fms apart", syncs[i].At-syncs[i-1].At)
				}
			}
			if n := len(encodesOf(r, "blob")); n > 3 {
				t.Fatalf("%d toBlob calls pending at once, pool is 3", n)
			}
			if r.RafPending != 1 {
				t.Fatalf("%d rAF callbacks pending, want the one loop", r.RafPending)
			}
		})
	}
}

// TestCaptureJSDropsStaleFrames 编号只增不减地发：比已发帧旧的帧，不论走哪条路、在哪一步晚到，都丢弃。
//
// 变异自证：去掉 send 里的编号比较，三例均红；改为在 toBlob 回调里（而不是真正发送时）比较编号，
// 「arrayBuffer 晚到」一例红。
func TestCaptureJSDropsStaleFrames(t *testing.T) {
	cases := []struct {
		name  string
		setup string
		stale int
	}{
		// 第一帧被拖住 600ms：其间当场编的帧比它新，它回来时丢弃。
		{"held-up toBlob after sync frames", `opt.blobDelay=function(at){return at<40?600:20;};`, 1},
		// 两个 toBlob 乱序回调（都没超过阈值，不当场编）：较旧的那个在较新的发出后才回来，丢弃。
		{"toBlob callbacks out of order", fmt.Sprintf(`opt.blobDelay=function(at){return at<40?%d:20;};`, captureStallMs-5), 1},
		// 第一帧的 toBlob 已回调、arrayBuffer 却拖到 550ms 才兑现；其间第二帧被拖住，当场编的帧先发出。
		// 第一帧与被拖住的第二帧都比当场编的旧，都丢弃。
		{"arrayBuffer settles after a sync frame", `
opt.blobDelay=function(at){return at>=60&&at<80?900:20;};
opt.abDelay=function(t){return t<60?500:0;};`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCapture(t, map[string]string{"a": v4(capURLA)}, tc.setup+`
inject('a');
await advance(1500);`)
			checkClean(t, r)
			checkMonotonic(t, r.Sends)
			if r.Stats.Stale != tc.stale {
				t.Fatalf("stale=%d, want %d (stats %+v)", r.Stats.Stale, tc.stale, *r.Stats)
			}
			first := r.Encodes[0]
			for _, s := range r.Sends {
				if s.Frame == first.Frame {
					t.Fatalf("first frame %d was sent at %.1f after newer ones", first.Frame, s.At)
				}
			}
		})
	}
}

// TestCaptureJSDownscalesBothPaths 两条路径都按同一规则快照：超过 captureOutMaxW 才等比缩，不放大。
//
// 变异自证：当场编的快照不传 outw，2560×1440 与 1921×1081 两例红。
func TestCaptureJSDownscalesBothPaths(t *testing.T) {
	cases := []struct {
		w, h         int
		wantW, wantH int
	}{
		{2560, 1440, 1920, 1080},
		{1062, 753, 1062, 753},
		{1920, 1080, 1920, 1080},
		{1921, 1081, 1920, 1080},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%dx%d", tc.w, tc.h), func(t *testing.T) {
			r := runCapture(t, map[string]string{"a": v4(capURLA)}, fmt.Sprintf(`
src.width=%d; src.height=%d;
opt.blobDelay=function(at){return at>=300&&at<800?900-at:20;};
inject('a');
await advance(1200);`, tc.w, tc.h))
			checkClean(t, r)
			kinds := map[string]bool{}
			for _, e := range r.Encodes {
				kinds[e.Kind] = true
				if len(e.Size) != 2 || e.Size[0] != tc.wantW || e.Size[1] != tc.wantH {
					t.Fatalf("%s encode at %.1f snapshot %v, want [%d %d]", e.Kind, e.At, e.Size, tc.wantW, tc.wantH)
				}
			}
			if !kinds["blob"] || !kinds["sync"] {
				t.Fatalf("scenario did not exercise both paths: %v", kinds)
			}
		})
	}
}

// TestCaptureJSSurvivesFailures 任一回调或桩抛错、给空结果，循环不死、不卡在当场编、不每拍重试；
// 故障过去后恢复为 toBlob 出帧。
//
// 变异自证：toBlob 抛错时不释放画布（去掉 catch 里的 s.busy=false），在途槽位被占死、转为一直当场编而红；
// lastSync 挪到 toDataURL 成功之后，toDataURL 抛错时每拍重试而红。
func TestCaptureJSSurvivesFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup string
	}{
		{"toBlob throws", `opt.toBlobThrows=function(t){return t<500;};`},
		{"toBlob yields null", `opt.nullBlob=function(t){return t<500;};`},
		{"arrayBuffer rejects", `opt.abReject=function(t){return t<500;};`},
		{"drawImage throws", `opt.drawThrows=function(t){return t<500;};`},
		{"ws.send throws", `opt.sendThrows=function(t){return t<500;};`},
		{"effect canvas absent", `opt.srcPresent=function(t){return t>=500;};`},
		{"effect canvas zero-sized", `var w0=src.width; src.width=0; later(500,function(){src.width=w0;});`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCapture(t, map[string]string{"a": v4(capURLA)}, tc.setup+`
inject('a');
await advance(1500);`)
			checkClean(t, r)
			checkMonotonic(t, r.Sends)
			if n := len(encodesOf(r, "sync")); n != 0 {
				t.Fatalf("%d sync encodes; a failed callback must not look like a stall", n)
			}
			after := sendsBetween(r, 600, 1500)
			if len(after) < 20 {
				t.Fatalf("only %d frames after the failure stopped", len(after))
			}
			if r.RafPending != 1 {
				t.Fatalf("%d rAF callbacks pending, want the one loop", r.RafPending)
			}
		})
	}

	t.Run("toDataURL throws during a permanent stall", func(t *testing.T) {
		r := runCapture(t, map[string]string{"a": v4(capURLA)}, `
opt.rafGap=function(t){return 1000/120;};
opt.blobDelay=function(at){return Infinity;};
opt.toDataURLThrows=function(t){return true;};
inject('a');
await advance(2000);`)
		checkClean(t, r)
		syncs := encodesOf(r, "sync")
		if len(syncs) == 0 || len(syncs) > 2000/captureSyncGapMs+1 {
			t.Fatalf("%d failing sync attempts in 2s", len(syncs))
		}
		for i := 1; i < len(syncs); i++ {
			if syncs[i].At-syncs[i-1].At < captureSyncGapMs {
				t.Fatalf("failing sync retried after %.1fms", syncs[i].At-syncs[i-1].At)
			}
		}
		if len(r.Sends) != 0 || r.RafPending != 1 {
			t.Fatalf("%d frames sent, %d rAF pending", len(r.Sends), r.RafPending)
		}
	})
}

// TestCaptureJSPause 服务端写 __mbxCapPaused 仍能暂停与恢复本版循环；注入时已是暂停的，等写入 false 才开抓。
//
// 变异自证：paused 初值写死为 false（不读注入时的状态），红。
func TestCaptureJSPause(t *testing.T) {
	r := runCapture(t, map[string]string{"a": v4(capURLA)}, `
window.__mbxCapPaused=true;
(0,eval)(SCRIPTS.a);
await advance(500); mark('resume');
window.__mbxCapPaused=false;
await advance(500); mark('pause');
window.__mbxCapPaused=true;
var readBack=window.__mbxCapPaused;
await advance(500); mark('resume2');
window.__mbxCapPaused=false;
await advance(500);
return {readBack:readBack};`)
	checkClean(t, r)
	m := r.Marks
	count := func(from, to float64) int { // (from, to]
		n := 0
		for _, e := range r.Encodes {
			if e.At > from && e.At <= to {
				n++
			}
		}
		return n
	}
	if n := count(-1, m["resume"]); n != 0 {
		t.Fatalf("%d encodes while injected paused", n)
	}
	if n := count(m["resume"], m["pause"]); n < 12 {
		t.Fatalf("%d encodes after resuming", n)
	}
	if n := count(m["pause"], m["resume2"]); n != 0 {
		t.Fatalf("%d encodes while paused", n)
	}
	if n := count(m["resume2"], m["resume2"]+500); n < 12 {
		t.Fatalf("%d encodes after resuming again", n)
	}
	if r.Extra["readBack"] != true {
		t.Fatalf("window.__mbxCapPaused reads %v; the takeover relies on it reading true", r.Extra["readBack"])
	}
}

// TestCaptureJSReinjectSameVersion 同一版本重复注入（进程重启换了 token）只切 ingest 地址，不另起循环。
//
// 变异自证：去掉 __mbxCapV4 版本门（第二次注入按接管旧版处理），红。
func TestCaptureJSReinjectSameVersion(t *testing.T) {
	r := runCapture(t, map[string]string{"a": v4(capURLA), "b": v4(capURLB)}, `
inject('a');
await advance(1000); mark('reinject');
inject('b');
await advance(1000);`)
	checkClean(t, r)
	checkMonotonic(t, r.Sends)
	if r.RafPending != 1 {
		t.Fatalf("%d rAF loops after re-injection", r.RafPending)
	}
	if len(r.Sockets) != 2 || r.Sockets[0].State != 3 || r.Sockets[1].URL != capURLB || r.Sockets[1].State != 1 {
		t.Fatalf("sockets %+v", r.Sockets)
	}
	at := r.Marks["reinject"]
	if n := len(sendsBetween(r, at, 2001)); n < 25 || n > 32 {
		t.Fatalf("%d frames in the second after re-injection (one loop gives ~30)", n)
	}
	for _, s := range sendsBetween(r, at, 2001) {
		if s.WS != 1 {
			t.Fatalf("frame at %.1f went to the old socket", s.At)
		}
	}
	if len(r.BadSockets) != 0 {
		t.Fatalf("bad sockets %+v", r.BadSockets)
	}
}

// TestCaptureJSStopsWhenSuperseded 下一版把 __mbxCapOwner 换掉：本版停循环、关 ws，在途的帧回来也不发。
// 属主在两拍之间（508ms）换掉，在途的两帧在本版下一拍（约 517ms）之前的 510ms 回调：此时 ws 还没关，
// 只能靠 send 里的属主判断拦下。
//
// 变异自证：去掉 loop 开头的属主判断，仍在抓帧而红；去掉 send 里的属主判断，在途帧照发而红。
func TestCaptureJSStopsWhenSuperseded(t *testing.T) {
	r := runCapture(t, map[string]string{"a": v4(capURLA)}, `
opt.blobDelay=function(at){return at>=460&&at<510?510-at:20;};
inject('a');
await advance(508); mark('superseded');
window.__mbxCapOwner={};
await advance(1000);`)
	checkClean(t, r)
	at := r.Marks["superseded"]
	late := false
	for _, e := range encodesOf(r, "blob") {
		late = late || (e.At < at && e.DoneAt != nil && *e.DoneAt > at)
	}
	if !late {
		t.Fatal("no toBlob came back after the owner change; the scenario proves nothing")
	}
	for _, e := range r.Encodes {
		if e.At > at {
			t.Fatalf("%s encode at %.1f after being superseded", e.Kind, e.At)
		}
	}
	if n := len(sendsBetween(r, at, 1e9)); n != 0 {
		t.Fatalf("%d frames sent after being superseded", n)
	}
	if r.RafPending != 0 || r.Sockets[0].State != 3 {
		t.Fatalf("rAF pending %d, socket state %d", r.RafPending, r.Sockets[0].State)
	}
}

// legacyCaptureV2JS 改动前（a5647e8）注入的抓帧脚本原文：页面里可能还跑着它的循环。它没有停止入口，
// 每拍只认 __mbxCapPaused。
const legacyCaptureV2JS = `(function(){
  if(!window.__mbxGLHook){
    window.__mbxGLHook=true;
    var orig=HTMLCanvasElement.prototype.getContext;
    HTMLCanvasElement.prototype.getContext=function(type,attrs){
      if(type&&/webgl/i.test(String(type))){var a=attrs||{};a.preserveDrawingBuffer=true;return orig.call(this,type,a);}
      return orig.call(this,type,attrs);
    };
  }
  var WS_URL=%q;
  // 热接管：本版脚本(__mbxCapV2)已在跑 → 只把 ingest 地址切到新 token、不新起循环（防双 loop 双发帧）。
  if(window.__mbxCapV2&&window.__mbxCapSetIngest){window.__mbxCapSetIngest(WS_URL);return;}
  // 升级过渡：页面里只有上一版脚本(有 __mbxCapStarted、无 __mbxCapV2、无切换函数) → 无法热接管，
  // 直接返回（不硬调不存在的函数以免抛错）；网易云 reload/切歌重建 canvas 即恢复。
  if(window.__mbxCapStarted)return;
  window.__mbxCapStarted=true; window.__mbxCapV2=true;
  var SEL='#lyric-effect-canvas-id';
  if(!window.__mbxCapCfg)window.__mbxCapCfg={q:%f,fps:%d,outw:%d};
  var ws=null, inflight=0, lastSend=0;
  function ensureWS(){
    if(ws&&(ws.readyState===0||ws.readyState===1))return;
    try{ws=new WebSocket(WS_URL);ws.binaryType='arraybuffer';}catch(e){ws=null;}
  }
  // 切 ingest 地址（进程重启后带新 token）：地址没变且连接健在则免抖动；否则关旧 ws、改 url、下一帧 ensureWS 重连。
  window.__mbxCapSetIngest=function(u){
    if(u===WS_URL&&ws&&(ws.readyState===0||ws.readyState===1))return;
    WS_URL=u;
    if(ws){try{ws.close();}catch(e){}ws=null;}
    ensureWS();
  };
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

// TestCaptureJSTakesOverLegacyLoop 页面里跑着上一版的循环（连接健在、还有被拖住的在途帧）时注入本版：
// 旧循环从此不再抓帧、不再重连，在途的旧帧回来也发不出去；只有本版在发帧，暂停照旧生效。
//
// 变异自证：去掉 __mbxCapPaused 访问器（本版改回直读它），旧循环每拍拿 about:blank 重建连接而红；去掉借旧
// 切换函数关 ws，旧循环被拖住的帧在接管后经旧连接发出而红；__mbxCapCfg 改回「没有才设」，沿用旧质量而红。
func TestCaptureJSTakesOverLegacyLoop(t *testing.T) {
	cases := []struct {
		name  string
		delay string
	}{
		{"legacy frames in flight at takeover", "at>=400&&at<500?1500-at:20"},
		{"legacy idle at takeover", "20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCapture(t, map[string]string{
				// 旧循环按上一个进程的质量（0.6）在抓；接管后应按本会话注入的 0.95 编码。
				"legacy": fmt.Sprintf(legacyCaptureV2JS, capURLA, 0.6, captureFPS, captureOutMaxW),
				"v4":     v4(capURLB),
			}, fmt.Sprintf(`
opt.blobDelay=function(at){return %s;};
inject('legacy');
await advance(500); mark('takeover');
inject('v4');
await advance(2000); mark('pause');
window.__mbxCapPaused=true;
await advance(500); mark('resume');
window.__mbxCapPaused=false;
await advance(500);`, tc.delay))
			checkSane(t, r)
			at := r.Marks["takeover"]
			checkJPEG(t, r, at)
			legacyCanvases := map[int]bool{}
			for _, e := range r.Encodes {
				if e.At < at {
					legacyCanvases[e.Canvas] = true
				}
			}
			if len(legacyCanvases) == 0 || len(sendsBetween(r, 0, at)) == 0 {
				t.Fatal("legacy loop never ran; the scenario proves nothing")
			}
			for _, e := range r.Encodes {
				if e.At >= at && legacyCanvases[e.Canvas] {
					t.Fatalf("legacy loop encoded at %.1f after the takeover", e.At)
				}
			}
			for _, s := range sendsBetween(r, at, 1e9) {
				if s.WS == 0 {
					t.Fatalf("legacy socket sent frame %d at %.1f after the takeover", s.Frame, s.At)
				}
			}
			if len(r.BadSockets) > 1 {
				t.Fatalf("legacy loop kept reconnecting: %d bad socket attempts", len(r.BadSockets))
			}
			if r.Sockets[0].URL != capURLA || r.Sockets[0].State != 3 {
				t.Fatalf("legacy socket %+v not closed", r.Sockets[0])
			}
			after := sendsBetween(r, at, r.Marks["pause"])
			if len(after) < 50 {
				t.Fatalf("only %d frames from the new loop", len(after))
			}
			checkMonotonic(t, after)
			for _, e := range r.Encodes {
				if e.At > r.Marks["pause"] && e.At < r.Marks["resume"] {
					t.Fatalf("%s encode at %.1f while paused", e.Kind, e.At)
				}
			}
			if n := len(sendsBetween(r, r.Marks["resume"], 1e9)); n < 12 {
				t.Fatalf("only %d frames after resuming", n)
			}
			if r.RafPending != 2 {
				t.Fatalf("%d rAF callbacks pending (legacy idling + new loop expected)", r.RafPending)
			}
		})
	}
}

// legacyCaptureV3JS #98 合入时（099fbfd）注入的抓帧脚本模板：页面里可能还跑着它的循环（版本门是 __mbxCapV3，停止入口是
// __mbxCapOwner）。依次为 ingest 地址、q、fps、输出最大宽、被拖住判定（ms）、当场编最小间隔（ms）。
const legacyCaptureV3JS = `(function(){
  if(!window.__mbxGLHook){
    window.__mbxGLHook=true;
    var orig=HTMLCanvasElement.prototype.getContext;
    HTMLCanvasElement.prototype.getContext=function(type,attrs){
      if(type&&/webgl/i.test(String(type))){var a=attrs||{};a.preserveDrawingBuffer=true;return orig.call(this,type,a);}
      return orig.call(this,type,attrs);
    };
  }
  var WS_URL=%q;
  // 热接管：本版脚本(__mbxCapV3)已在跑 → 只把 ingest 地址切到新 token、不新起循环（防双 loop 双发帧）。
  if(window.__mbxCapV3&&window.__mbxCapSetIngest){window.__mbxCapSetIngest(WS_URL);return;}
  // 接管更早版本的循环：它们每拍只认 __mbxCapPaused、没有停止入口。读来恒为真 → 旧循环空转；
  // 服务端照旧写入的暂停状态经 setter 落到本版。旧版的切换函数在的话，用建不起连接的地址关掉旧 ws。
  var paused=!!window.__mbxCapPaused;
  Object.defineProperty(window,'__mbxCapPaused',{configurable:true,get:function(){return true;},set:function(v){paused=!!v;}});
  if(window.__mbxCapSetIngest){try{window.__mbxCapSetIngest('about:blank');}catch(e){}}
  var me={}; window.__mbxCapOwner=me;
  // 保留 __mbxCapV2：降级回上一版时，它的脚本据此热接管本版循环，而不是另起一个被本版暂停住的循环。
  window.__mbxCapStarted=true; window.__mbxCapV2=true; window.__mbxCapV3=true;
  window.__mbxCapCfg={q:%f,fps:%d,outw:%d};
  var SEL='#lyric-effect-canvas-id', STALL=%d, SYNC_GAP=%d;
  var ws=null, lastSend=0, lastSync=-Infinity, seq=0, sentSeq=0;
  // 计数，供真机核对：经 toBlob 发出、当场编发出、晚于已发帧而丢弃的帧数
  var stats=window.__mbxCapStats={blob:0,sync:0,stale:0};
  function ensureWS(){
    if(ws&&(ws.readyState===0||ws.readyState===1))return;
    try{ws=new WebSocket(WS_URL);ws.binaryType='arraybuffer';}catch(e){ws=null;}
  }
  // 切 ingest 地址（进程重启后带新 token）：地址没变且连接健在则免抖动；否则关旧 ws、改 url、下一帧 ensureWS 重连。
  window.__mbxCapSetIngest=function(u){
    if(u===WS_URL&&ws&&(ws.readyState===0||ws.readyState===1))return;
    WS_URL=u;
    if(ws){try{ws.close();}catch(e){}ws=null;}
    ensureWS();
  };
  function send(n,data,kind){
    if(window.__mbxCapOwner!==me)return;
    if(n<=sentSeq){stats.stale++;return;}
    if(!ws||ws.readyState!==1)return;
    try{ws.send(data);}catch(e){return;}
    sentSeq=n; stats[kind]++;
  }
  function mk(){var o=document.createElement('canvas');var cx=o.getContext('2d',{alpha:false});cx.imageSmoothingEnabled=true;cx.imageSmoothingQuality='high';return {o:o,x:cx,busy:false,t:0};}
  // 编码画布池：每个在途的 toBlob 各用一块快照；当场编另用一块，不碰在途的
  var POOL=3, cans=[], syncCan=mk();
  for(var i=0;i<POOL;i++)cans.push(mk());
  // 按 canvas 实际尺寸快照；仅当超过 outw（超大窗口）才降采样，控制编码量。绝不改源 canvas。
  function snap(s,c,mx){
    var ow=c.width, oh=c.height;
    if(mx>0&&ow>mx){var k=mx/ow; ow=Math.max(1,Math.round(ow*k)); oh=Math.max(1,Math.round(oh*k));}
    if(s.o.width!==ow)s.o.width=ow; if(s.o.height!==oh)s.o.height=oh;
    s.x.drawImage(c,0,0,ow,oh);
  }
  function loop(){
    if(window.__mbxCapOwner!==me){if(ws){try{ws.close();}catch(e){}ws=null;}return;}
    requestAnimationFrame(loop);
    if(paused)return;
    var cfg=window.__mbxCapCfg||{}, now=performance.now(), q=cfg.q||0.88;
    // 软上限：达到目标 fps 才跳过（容差 半个 60fps 帧，避免 rAF 量化导致每隔一拍被错过而腰斩帧率）
    if(cfg.fps && now-lastSend < 1000/cfg.fps - 8)return;
    var c=document.querySelector(SEL);
    if(!c||!c.width||!c.height)return;
    ensureWS(); if(!ws||ws.readyState!==1)return;
    var slot=null, oldest=now;
    for(var i=0;i<POOL;i++){var s=cans[i]; if(!s.busy){if(!slot)slot=s;}else if(s.t<oldest)oldest=s.t;}
    if(now-oldest>STALL){
      if(now-lastSync<SYNC_GAP)return;
      // 先记时刻再编：编码抛错也照样限频，不会每拍重试
      lastSync=now; lastSend=now;
      var n=++seq, u;
      try{snap(syncCan,c,cfg.outw||0); u=syncCan.o.toDataURL('image/jpeg',q);}catch(e){return;}
      send(n,u,'sync');
      return;
    }
    if(!slot)return;
    try{snap(slot,c,cfg.outw||0);}catch(e){return;}
    slot.busy=true; slot.t=now; lastSend=now;
    (function(s,n){
      try{s.o.toBlob(function(b){
        s.busy=false;
        if(b)b.arrayBuffer().then(function(ab){send(n,ab,'blob');}).catch(function(){});
      },'image/jpeg',q);}catch(e){s.busy=false;}
    })(slot,++seq);
  }
  requestAnimationFrame(loop);
})()`

// TestCaptureJSTakesOverV3Loop 页面里跑着上一版（#98 合入时）的循环、还有被拖住的在途帧时注入本版：旧循环自己停掉、
// 关 ws，在途的旧帧回来也发不出去，旧循环不再占 rAF；只有本版在发帧，暂停照旧生效。
//
// 变异自证（均实测）：本版的版本门改回 __mbxCapV3（把旧循环当成同版本，只切地址不接管），旧循环继续占着 rAF 与旧
// 连接而红；去掉 __mbxCapOwner 的更换，旧循环不停而红。
func TestCaptureJSTakesOverV3Loop(t *testing.T) {
	r := runCapture(t, map[string]string{
		"v3": compactJS(fmt.Sprintf(legacyCaptureV3JS, capURLA, 0.6, captureFPS, captureOutMaxW, 60, captureSyncGapMs)),
		"v4": v4(capURLB),
	}, `
opt.blobDelay=function(at){return at>=400&&at<500?1500-at:20;};
inject('v3');
await advance(500); mark('takeover');
inject('v4');
await advance(2000); mark('pause');
window.__mbxCapPaused=true;
await advance(500); mark('resume');
window.__mbxCapPaused=false;
await advance(500);`)
	checkSane(t, r)
	at := r.Marks["takeover"]
	checkJPEG(t, r, at)
	if len(sendsBetween(r, 0, at)) == 0 {
		t.Fatal("the V3 loop never ran; the scenario proves nothing")
	}
	for _, s := range sendsBetween(r, at, 1e9) {
		if s.WS == 0 {
			t.Fatalf("V3 socket sent frame %d at %.1f after the takeover", s.Frame, s.At)
		}
	}
	if r.Sockets[0].URL != capURLA || r.Sockets[0].State != 3 {
		t.Fatalf("V3 socket %+v not closed", r.Sockets[0])
	}
	if r.RafPending != 1 {
		t.Fatalf("%d rAF callbacks pending after the takeover, want only the new loop", r.RafPending)
	}
	after := sendsBetween(r, at, r.Marks["pause"])
	if len(after) < 50 {
		t.Fatalf("only %d frames from the new loop", len(after))
	}
	checkMonotonic(t, after)
	for _, e := range r.Encodes {
		if e.At > r.Marks["pause"] && e.At < r.Marks["resume"] {
			t.Fatalf("%s encode at %.1f while paused", e.Kind, e.At)
		}
	}
	if n := len(sendsBetween(r, r.Marks["resume"], 1e9)); n < 12 {
		t.Fatalf("only %d frames after resuming", n)
	}
}

// TestCaptureJSSlowButAlive toBlob 回调慢到 120～150ms（超过 captureStallMs）但一直在回来：三块画布的池仍按
// 回调的节奏出帧，不陷进当场编。刚开始抓帧时第一个回调还没回来，允许开头当场编一两帧；此后不再当场编，帧间隔
// 不超过 250ms（回调成簇到达，簇间空档约 D−66ms）。
//
// 变异自证（均实测）：判据改回 #98 初版的「最早在途的 toBlob 等过 60ms」，120ms、150ms 两例都滑进每秒约 8 帧的
// 当场编而红；回调到达（含过期的）不再算「还在出东西」，同样红。
func TestCaptureJSSlowButAlive(t *testing.T) {
	for _, d := range []int{120, 150} {
		t.Run(fmt.Sprintf("%dms callbacks", d), func(t *testing.T) {
			r := runCapture(t, map[string]string{"a": v4(capURLA)}, fmt.Sprintf(`
opt.blobDelay=function(at){return %d;};
inject('a');
await advance(5000);`, d))
			checkClean(t, r)
			checkMonotonic(t, r.Sends)
			syncs := encodesOf(r, "sync")
			g := maxGap(sendsBetween(r, 400, 1e9), 400)
			t.Logf("%d frames, %d sync, %d stale, max gap %.0fms", len(r.Sends), len(syncs), r.Stats.Stale, g)
			if len(syncs) > 2 {
				t.Fatalf("%d sync encodes although callbacks kept coming back", len(syncs))
			}
			if want := 5000 * 3 / d * 6 / 10; len(r.Sends) < want {
				t.Fatalf("only %d frames in 5s, want >= %d (pool of 3 at %dms)", len(r.Sends), want, d)
			}
			if g >= 250 {
				t.Fatalf("stream stalled for %.0fms", g)
			}
		})
	}
}

// TestCaptureJSOneSlotHeldUpKeepsStreaming 只有一块画布的 toBlob 被拖住约 2 秒，另两块照常出帧：流没有断，
// 不当场编；被拖住的那一帧回来时比已发的旧，丢弃。
//
// 变异自证：判据改回「最早在途的 toBlob 等过 60ms」，被拖住的那一块让整段都在当场编而红。
func TestCaptureJSOneSlotHeldUpKeepsStreaming(t *testing.T) {
	r := runCapture(t, map[string]string{"a": v4(capURLA)}, `
opt.blobDelay=function(at){return at>=990&&at<1020?2000:20;};
inject('a');
await advance(3500);`)
	checkClean(t, r)
	checkMonotonic(t, r.Sends)
	if n := len(encodesOf(r, "sync")); n != 0 {
		t.Fatalf("%d sync encodes while two slots kept delivering", n)
	}
	if n := len(sendsBetween(r, 1100, 2900)); n < 40 {
		t.Fatalf("only %d frames while one slot was held up", n)
	}
	if g := maxGap(r.Sends, 40); g > 100 {
		t.Fatalf("stream gap %.0fms with two healthy slots", g)
	}
	if r.Stats.Stale != 1 {
		t.Fatalf("stale=%d, want the one held-up frame dropped (stats %+v)", r.Stats.Stale, *r.Stats)
	}
}

// TestCaptureJSNoSyncAfterIdle 抓帧停了很久之后恢复：断流从恢复的那一拍起算，不拿停了很久的上一次出帧去比，一帧也不
// 当场编。三种停法各是 loop 里的一个重置分支，各自独立一个场景，免得一个场景的重置掩盖另一个分支的遗漏：
// 暂停 60 秒、特效画布缺席 60 秒、ingest 连接建不起来 60 秒（切换函数给了建不起连接的地址）。
// 回调取 60ms：恢复后的第二拍被 25ms 的抓帧节流挡掉，第三拍起回调才可能已回来，回调太快就看不出误触发。
//
// 变异自证（均实测）：只去掉暂停分支的 wasIdle=true，「暂停」一例红；只去掉画布缺席分支的，「画布缺席」一例红；
// 只去掉连接未就绪分支的，「ingest 建不起来」一例红；三者同时去掉，三例都红（恢复后立即当场编）。
func TestCaptureJSNoSyncAfterIdle(t *testing.T) {
	cases := []struct {
		name  string
		setup string // 注入前的 opt 设定
		idle  string // 抓帧跑过 500ms 之后停 60 秒的做法，最后一句之后抓帧恢复
	}{
		{"paused for 60s", ``, `
window.__mbxCapPaused=true;
await advance(60000);
window.__mbxCapPaused=false;`},
		{"effect canvas absent for 60s", `opt.srcPresent=function(t){return t<500||t>=60500;};`, `
await advance(60000);`},
		{"ingest unreachable for 60s", ``, fmt.Sprintf(`
window.__mbxCapSetIngest('about:blank');
await advance(60000);
window.__mbxCapSetIngest(%q);`, capURLB)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCapture(t, map[string]string{"a": v4(capURLA)}, `
opt.blobDelay=function(at){return 60;};`+tc.setup+`
inject('a');
await advance(500); mark('idle');`+tc.idle+`
mark('back');
await advance(2000);`)
			checkClean(t, r)
			checkMonotonic(t, r.Sends)
			if n := len(encodesOf(r, "sync")); n != 0 {
				t.Fatalf("%d sync encodes after the idle period", n)
			}
			if n := len(sendsBetween(r, 0, r.Marks["idle"])); n < 5 {
				t.Fatalf("only %d frames before the idle period", n)
			}
			if n := len(sendsBetween(r, r.Marks["back"], 1e9)); n < 40 {
				t.Fatalf("only %d frames after recovering", n)
			}
		})
	}
}
