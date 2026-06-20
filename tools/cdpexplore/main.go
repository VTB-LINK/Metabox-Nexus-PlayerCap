// Command cdpexplore is a throwaway CDP probe for the NetEase lyric-effect canvas.
// Usage:
//   go run ./tools/cdpexplore eval <jsfile>          - run JS from file, print result
//   go run ./tools/cdpexplore evals "<expr>"         - run inline JS expr
//   go run ./tools/cdpexplore screencast <n> [ms]    - capture n frames, report sizes
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type page struct {
	ID                   string `json:"id"`
	URL                  string `json:"url"`
	WebSocketDebuggerUrl string `json:"webSocketDebuggerUrl"`
}

func wsURL() string {
	resp, err := http.Get("http://127.0.0.1:9222/json")
	must(err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var pages []page
	must(json.Unmarshal(b, &pages))
	for _, p := range pages {
		if p.URL == "orpheus://orpheus/pub/app.html" {
			return p.WebSocketDebuggerUrl
		}
	}
	if len(pages) > 0 {
		return pages[0].WebSocketDebuggerUrl
	}
	panic("no page")
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func safeDiv(a, b int) int {
	if b == 0 {
		return 0
	}
	return a / b
}

type conn struct {
	c  *websocket.Conn
	id int
}

func dial() *conn {
	c, _, err := websocket.DefaultDialer.Dial(wsURL(), nil)
	must(err)
	return &conn{c: c, id: 1}
}

// send issues a method, waits for the matching id reply, returns raw result json.
func (cc *conn) send(method string, params map[string]any) json.RawMessage {
	id := cc.id
	cc.id++
	req := map[string]any{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	must(cc.c.WriteJSON(req))
	cc.c.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		must(cc.c.ReadJSON(&m))
		if m.ID == id {
			if m.Error != nil {
				return m.Error
			}
			return m.Result
		}
	}
}

func (cc *conn) eval(expr string) string {
	res := cc.send("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	var r struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	json.Unmarshal(res, &r)
	if r.ExceptionDetails != nil {
		return "EXC: " + string(r.ExceptionDetails)
	}
	if s, ok := r.Result.Value.(string); ok {
		return s
	}
	b, _ := json.Marshal(r.Result.Value)
	return string(b)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("need cmd")
		return
	}
	cc := dial()
	defer cc.c.Close()
	cc.send("Runtime.enable", nil)

	switch os.Args[1] {
	case "jiggle":
		// 通过 CDP Input.dispatchMouseEvent 注入真实鼠标移动，触发网易云的「鼠标唤出菜单」逻辑
		n := 20
		if len(os.Args) > 2 {
			n, _ = strconv.Atoi(os.Args[2])
		}
		for i := 0; i < n; i++ {
			x := 200 + (i%10)*60
			y := 150 + (i%7)*70
			cc.send("Input.dispatchMouseEvent", map[string]any{
				"type": "mouseMoved", "x": x, "y": y,
			})
			time.Sleep(60 * time.Millisecond)
		}
		fmt.Printf("dispatched %d mouse moves\n", n)
		return
	case "dblclickat":
		// 通过 CDP Input 发送一次真实(trusted)双击
		x, y := 500, 380
		if len(os.Args) > 3 {
			x, _ = strconv.Atoi(os.Args[2])
			y, _ = strconv.Atoi(os.Args[3])
		}
		press := func(clickCount int) {
			cc.send("Input.dispatchMouseEvent", map[string]any{
				"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": clickCount,
			})
			cc.send("Input.dispatchMouseEvent", map[string]any{
				"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": clickCount,
			})
		}
		press(1)
		time.Sleep(40 * time.Millisecond)
		press(2)
		fmt.Printf("dispatched trusted double-click at (%d,%d)\n", x, y)
		return
	case "effectclient":
		// 连接我们自己的 /cloudmusicv3/effect-ws，统计收到的二进制帧（验证全链路）
		secs := 8
		if len(os.Args) > 2 {
			secs, _ = strconv.Atoi(os.Args[2])
		}
		url := "ws://localhost:8765/cloudmusicv3/effect-ws"
		if len(os.Args) > 3 && os.Args[3] != "" {
			url += "?" + os.Args[3]
		}
		ec, _, err := websocket.DefaultDialer.Dial(url, nil)
		must(err)
		defer ec.Close()
		start := time.Now()
		var frames, bytesTotal int
		go func() { time.Sleep(time.Duration(secs) * time.Second); ec.Close() }()
		for {
			mt, data, err := ec.ReadMessage()
			if err != nil {
				break
			}
			if mt == websocket.BinaryMessage {
				frames++
				bytesTotal += len(data)
				if frames%15 == 0 {
					fmt.Printf("[%5dms] frame#%d bytes=%d\n", time.Since(start).Milliseconds(), frames, len(data))
					os.WriteFile("tools/cdpexplore/effectframe.jpg", data, 0644)
				}
			} else {
				fmt.Printf("[%5dms] text msg: %.120s\n", time.Since(start).Milliseconds(), string(data))
			}
		}
		fmt.Printf("== effect frames=%d avgBytes=%d over %ds ==\n", frames, safeDiv(bytesTotal, frames), secs)
		return
	case "pollshot":
		// Poll Page.captureScreenshot at ~25fps; reports stalls (slow captures) and
		// whether content changes. captureScreenshot forces an on-demand composite,
		// which may keep producing frames even when the window is hidden/minimized.
		secs := 20
		if len(os.Args) > 2 {
			secs, _ = strconv.Atoi(os.Args[2])
		}
		cc.send("Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true})
		cc.send("Page.setWebLifecycleState", map[string]any{"state": "active"})
		cc.send("Page.enable", nil)
		start := time.Now()
		var prev uint32
		var n, frozen, slow int
		deadline := time.Now().Add(time.Duration(secs) * time.Second)
		for time.Now().Before(deadline) {
			t0 := time.Now()
			res := cc.send("Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 50})
			lat := time.Since(t0)
			var r struct {
				Data string `json:"data"`
			}
			json.Unmarshal(res, &r)
			if r.Data == "" {
				fmt.Printf("[%5dms] capture FAILED lat=%dms raw=%.80s\n", time.Since(start).Milliseconds(), lat.Milliseconds(), string(res))
				continue
			}
			n++
			h := crc32.ChecksumIEEE([]byte(r.Data))
			tag := "changed"
			if h == prev {
				tag = "frozen-identical"
				frozen++
			}
			prev = h
			if lat > 400*time.Millisecond {
				slow++
				tag += " SLOW"
			}
			if n%15 == 0 || lat > 400*time.Millisecond {
				fmt.Printf("[%5dms] cap#%d lat=%dms bytes=%d %s\n", time.Since(start).Milliseconds(), n, lat.Milliseconds(), len(r.Data), tag)
			}
			if d := 40*time.Millisecond - lat; d > 0 {
				time.Sleep(d)
			}
		}
		fmt.Printf("== captures=%d frozen-identical=%d slow(>400ms)=%d over %ds ==\n", n, frozen, slow, secs)
	case "prooftest":
		// 持久连接里：注入 preserveDrawingBuffer shim → reload → 持续读回打印命中率。
		// 连接不断开 → addScriptToEvaluateOnNewDocument 存活 → reload 后 shim 生效。
		// 用法: prooftest [alpha] —— 中途请重开歌曲详情页
		forceAlpha := len(os.Args) > 2 && os.Args[2] == "alpha"
		alphaLine := ""
		if forceAlpha {
			alphaLine = "a.alpha=true;a.premultipliedAlpha=false;"
		}
		shim := `(function(){
		  if(window.__mbxGLHook)return; window.__mbxGLHook=true;
		  var orig=HTMLCanvasElement.prototype.getContext;
		  HTMLCanvasElement.prototype.getContext=function(type,attrs){
		    if(type&&/webgl/i.test(String(type))){var a=attrs||{};a.preserveDrawingBuffer=true;` + alphaLine + `return orig.call(this,type,a);}
		    return orig.call(this,type,attrs);
		  };
		})()`
		cc.send("Page.enable", nil)
		fmt.Println(">> addScript:", string(cc.send("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": shim})))
		fmt.Println(">> reload:", string(cc.send("Page.reload", map[string]any{})))
		fmt.Printf("已注入(alpha=%v)+reload。请在 30s 内重开歌曲详情页…\n", forceAlpha)
		readJS := `JSON.stringify((function(){var c=document.querySelector('#lyric-effect-canvas-id');if(!c||!c.width)return{n:-1};var oc=document.createElement('canvas');oc.width=c.width;oc.height=c.height;var x=oc.getContext('2d');x.drawImage(c,0,0);var d=x.getImageData(0,0,oc.width,oc.height).data,nz=0,aA=0;for(var i=0;i<d.length;i+=4){if(d[i]||d[i+1]||d[i+2]||d[i+3])nz++;if(d[i+3]>0&&d[i+3]<255)aA++;}return{n:+(nz/(d.length/4)*100).toFixed(1),hook:!!window.__mbxGLHook,semiAlpha:+(aA/(d.length/4)*100).toFixed(2)};})())`
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			fmt.Printf("[%2ds] %s\n", int(time.Until(deadline).Seconds()), cc.eval(readJS))
			time.Sleep(700 * time.Millisecond)
		}
		return
	case "armlayer":
		// 注入 getContext shim 强制 preserveDrawingBuffer(可选 alpha:true)，然后 reload。
		// document-start 注入 → 网易云创建 WebGL context 时被改写 → 之后可随时 drawImage/toDataURL 读回。
		// 用法: armlayer [alpha]
		forceAlpha := len(os.Args) > 2 && os.Args[2] == "alpha"
		alphaLine := ""
		if forceAlpha {
			alphaLine = "a.alpha=true;a.premultipliedAlpha=false;"
		}
		shim := `(function(){
		  if(window.__mbxGLHook)return; window.__mbxGLHook=true;
		  var orig=HTMLCanvasElement.prototype.getContext;
		  HTMLCanvasElement.prototype.getContext=function(type,attrs){
		    if(type&&/webgl/i.test(String(type))){
		      var a=attrs||{}; a.preserveDrawingBuffer=true; ` + alphaLine + `
		      return orig.call(this,type,a);
		    }
		    return orig.call(this,type,attrs);
		  };
		})()`
		cc.send("Page.enable", nil)
		fmt.Println(">> addScript:", string(cc.send("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": shim})))
		fmt.Println(">> reload:", string(cc.send("Page.reload", map[string]any{"ignoreCache": false})))
		fmt.Printf("armed (forceAlpha=%v). 现在请在网易云重新打开一首歌的详情页，然后运行 readlayer\n", forceAlpha)
		return
	case "readlayer":
		// drawImage 把 #lyric-effect-canvas-id 读到 2D canvas，统计 alpha/RGB 分布，存 PNG。
		// 用法: readlayer [out.png]
		outFile := "tools/cdpexplore/purelayer.png"
		if len(os.Args) > 2 {
			outFile = os.Args[2]
		}
		stats := cc.eval(`JSON.stringify((function(){
		  var c=document.querySelector('#lyric-effect-canvas-id');if(!c)return{found:false};
		  var oc=document.createElement('canvas');oc.width=c.width;oc.height=c.height;
		  var x=oc.getContext('2d');x.drawImage(c,0,0);
		  var d=x.getImageData(0,0,oc.width,oc.height).data,n=d.length/4,aA=0,fO=0,aRGB=0;
		  for(var i=0;i<d.length;i+=4){var a=d[i+3];if(a>0)aA++;if(a===255)fO++;if(d[i]||d[i+1]||d[i+2])aRGB++;}
		  return{found:true,w:oc.width,h:oc.height,anyAlphaPct:+(aA/n*100).toFixed(1),fullOpaquePct:+(fO/n*100).toFixed(1),anyRGBPct:+(aRGB/n*100).toFixed(1)};
		})())`)
		fmt.Println("stats:", stats)
		// 取 PNG bytes（drawImage 后 toDataURL 含 alpha）
		durl := cc.eval(`(function(){var c=document.querySelector('#lyric-effect-canvas-id');if(!c)return'';var oc=document.createElement('canvas');oc.width=c.width;oc.height=c.height;oc.getContext('2d').drawImage(c,0,0);return oc.toDataURL('image/png');})()`)
		const pfx = "data:image/png;base64,"
		if len(durl) > len(pfx) && durl[:len(pfx)] == pfx {
			raw, err := base64.StdEncoding.DecodeString(durl[len(pfx):])
			must(err)
			must(os.WriteFile(outFile, raw, 0644))
			fmt.Printf("saved %s (%d bytes)\n", outFile, len(raw))
		} else {
			fmt.Println("no dataURL, got:", durl[:min(80, len(durl))])
		}
		return
	case "eval":
		b, err := os.ReadFile(os.Args[2])
		must(err)
		fmt.Println(cc.eval(string(b)))
	case "evals":
		fmt.Println(cc.eval(os.Args[2]))
	case "screencast":
		n := 5
		if len(os.Args) > 2 {
			n, _ = strconv.Atoi(os.Args[2])
		}
		cc.send("Page.enable", nil)
		cc.send("Page.startScreencast", map[string]any{
			"format": "jpeg", "quality": 60, "maxWidth": 600, "maxHeight": 400, "everyNthFrame": 1,
		})
		got := 0
		start := time.Now()
		cc.c.SetReadDeadline(time.Now().Add(20 * time.Second))
		for got < n {
			var m struct {
				Method string `json:"method"`
				Params struct {
					Data      string `json:"data"`
					SessionID int    `json:"sessionId"`
				} `json:"params"`
			}
			if err := cc.c.ReadJSON(&m); err != nil {
				fmt.Println("read err:", err)
				break
			}
			if m.Method == "Page.screencastFrame" {
				got++
				fmt.Printf("frame %d  t=%dms  bytes=%d\n", got, time.Since(start).Milliseconds(), len(m.Params.Data))
				cc.send("Page.screencastFrameAck", map[string]any{"sessionId": m.Params.SessionID})
			}
		}
		cc.send("Page.stopScreencast", nil)
	case "monitor", "monitorfocus":
		// continuous screencast for N seconds; flag frozen (identical) frames.
		// monitorfocus also enables focus emulation + active lifecycle (no relaunch).
		secs := 10
		if len(os.Args) > 2 {
			secs, _ = strconv.Atoi(os.Args[2])
		}
		if os.Args[1] == "monitorfocus" {
			fmt.Println(">> Emulation.setFocusEmulationEnabled(true):", string(cc.send("Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true})))
			fmt.Println(">> Page.setWebLifecycleState(active):", string(cc.send("Page.setWebLifecycleState", map[string]any{"state": "active"})))
		}
		cc.send("Page.enable", nil)
		cc.send("Page.startScreencast", map[string]any{
			"format": "jpeg", "quality": 50, "maxWidth": 480, "maxHeight": 320, "everyNthFrame": 1,
		})
		cc.c.SetReadDeadline(time.Time{}) // clear deadline left over from send(); reader blocks indefinitely
		start := time.Now()
		lastFrameNs := start.UnixNano()
		var frames, frozen int64
		done := make(chan struct{})
		// heartbeat goroutine: report stall start / resume without killing the conn
		go func() {
			t := time.NewTicker(400 * time.Millisecond)
			defer t.Stop()
			stalled := false
			for {
				select {
				case <-done:
					return
				case <-t.C:
					gap := time.Since(time.Unix(0, atomic.LoadInt64(&lastFrameNs)))
					if gap > time.Second && !stalled {
						stalled = true
						fmt.Printf("[%5dms] >>> STALL START (no frames) <<<\n", time.Since(start).Milliseconds())
					} else if gap <= time.Second && stalled {
						stalled = false
						fmt.Printf("[%5dms] >>> RESUMED after stall <<<\n", time.Since(start).Milliseconds())
					}
				}
			}
		}()
		// stopper: end the run by closing the conn so the blocking ReadJSON returns
		go func() { time.Sleep(time.Duration(secs) * time.Second); close(done); cc.c.Close() }()
		var prev uint32
		for {
			var m struct {
				Method string `json:"method"`
				Params struct {
					Data      string `json:"data"`
					SessionID int    `json:"sessionId"`
				} `json:"params"`
			}
			if err := cc.c.ReadJSON(&m); err != nil {
				fmt.Printf("[%5dms] reader stopped: %v\n", time.Since(start).Milliseconds(), err)
				break
			}
			if m.Method != "Page.screencastFrame" {
				continue
			}
			n := atomic.AddInt64(&frames, 1)
			atomic.StoreInt64(&lastFrameNs, time.Now().UnixNano())
			h := crc32.ChecksumIEEE([]byte(m.Params.Data))
			tag := "changed"
			if h == prev {
				tag = "frozen-identical"
				atomic.AddInt64(&frozen, 1)
			}
			prev = h
			if n%15 == 0 {
				fmt.Printf("[%5dms] frame#%d bytes=%d %s\n", time.Since(start).Milliseconds(), n, len(m.Params.Data), tag)
			}
			// ack is a write-only message from this (sole reader/writer) goroutine
			cc.c.WriteJSON(map[string]any{"id": cc.id, "method": "Page.screencastFrameAck", "params": map[string]any{"sessionId": m.Params.SessionID}})
			cc.id++
		}
		fmt.Printf("== frames=%d frozen-identical=%d over %ds ==\n", atomic.LoadInt64(&frames), atomic.LoadInt64(&frozen), secs)
	}
}
