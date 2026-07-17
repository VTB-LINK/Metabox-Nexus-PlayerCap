// Command wsrecord 同时连多个 WS 端点，把收到的事件带时间戳原样录成 jsonl。
// 为 issue #43（重写 API 文档）而写：文档里的 WS 示例必须来自真实流量，不是手写的。
//
// 用法：
//
//	go run ./tools/wsrecord -out <目录> /ws /qqmusic/ws
//	go run ./tools/wsrecord -out <目录> -addr 127.0.0.1:8765 /ws /kugou/ws
//
// 每个路径录一个文件（/qqmusic/ws → qqmusic_ws.jsonl），每行一条：
//
//	{"ms":12,"kind":"text","data":{...原文...}}
//	{"ms":340,"kind":"binary","len":15234}    // effect-ws 的 JPEG 帧
//
// # 为什么要同时连多个
//
// 去重逻辑只能靠**对比**观察，单连接看不见：router.go 对根订阅者会在切换时跳过事件
// （FullState 已推送），对 per-player 订阅者则始终推送。同时连 /ws 与 /<player>/ws，
// 两份 jsonl 的差集就是抑制掉的那些 —— 这是「消息遗漏是不是有意的」唯一的实证办法。
//
// 所有连接共享**同一个时间原点**（进程启动时刻），否则跨文件的 ms 无法比对，
// 而比对正是本工具存在的理由。
//
// # 为什么存 RawMessage 而不是解析后再存
//
// 字段顺序是与 overlay 的契约（player/eventjson_test.go:4，服务端靠 OrderedMap 保证）。
// 解析成 map 再 Marshal 会按 Go 的 map 序重排 —— 录出来的「原文」就成了假的，
// 而这份录音要当作文档示例的唯一来源。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// origin 所有连接共享的时间原点，令跨文件的 ms 可比对。
var origin = time.Now()

type record struct {
	MS   int64           `json:"ms"`
	Kind string          `json:"kind"`
	Path string          `json:"path,omitempty"` // http 快照：抓的是哪个端点
	Code int             `json:"code,omitempty"` // http 快照：状态码
	Data json.RawMessage `json:"data,omitempty"`
	Len  int             `json:"len,omitempty"`
}

// httpSnapSuffixes HTTP 快照要抓的端点，由 WS 路径推导前缀：
// /ws → /all_lyrics…，/qqmusic/ws → /qqmusic/all_lyrics…
//
// 只抓这四个：它们与 WS 事件一一对应，是「HTTP 的 data 是否等于 WS 的 data」这个
// 问题的全部战场。/health-check 与 /service-status 不跟随播放器，无需按秒采样。
var httpSnapSuffixes = []string{"/all_lyrics", "/lyric_update", "/status_update", "/song_info"}

func main() {
	addr := flag.String("addr", "127.0.0.1:8765", "服务地址")
	out := flag.String("out", "", "输出目录（必填）")
	dur := flag.Duration("duration", 0, "录制时长，0 = 直到 Ctrl+C")
	wait := flag.Duration("wait", 0, "服务未就绪时最多等多久（0 = 不等，连不上就退）")
	snap := flag.Duration("snap", 0, "每隔多久抓一次 HTTP 快照（0 = 不抓）。端点由 WS 路径推导")
	flag.Parse()

	paths := flag.Args()
	if *out == "" || len(paths) == 0 {
		fmt.Println("用法: wsrecord -out <目录> <路径>...")
		fmt.Println("例:   wsrecord -out ./rec /ws /qqmusic/ws")
		os.Exit(2)
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Println("建目录失败:", err)
		os.Exit(1)
	}

	// 等服务就绪。录制几乎总是「先开录、再启动被测程序」的顺序，连不上就立刻退出
	// 会让人白等一轮——尤其录制跑在后台时，失败要过很久才被发现。
	if *wait > 0 && !waitReady(*addr, *wait) {
		fmt.Printf("等了 %v，%s 仍未就绪\n", *wait, *addr)
		os.Exit(1)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for _, p := range paths {
		// /qqmusic/ws → qqmusic_ws.jsonl
		name := strings.TrimPrefix(p, "/")
		name = strings.ReplaceAll(name, "/", "_") + ".jsonl"
		f, err := os.Create(filepath.Join(*out, name))
		if err != nil {
			fmt.Println("建文件失败:", err)
			os.Exit(1)
		}

		wg.Add(1)
		go func(p string, f *os.File) {
			defer wg.Done()
			defer f.Close()
			// -SSE 后缀走 SSE（HTTP 长连接 + text/event-stream），其余走 WebSocket。
			// 两者都是「流」，共享同一个 origin，故可与 WS 事件逐毫秒对齐。
			if strings.HasSuffix(p, "-SSE") {
				sseLoop(p, *addr, f, stop)
			} else {
				recordLoop(p, *addr, f, stop)
			}
		}(p, f)
	}

	// HTTP 快照：与流共享 origin，用来回答「同一时刻 HTTP 说的和 WS 说的是否一致」。
	if *snap > 0 {
		f, err := os.Create(filepath.Join(*out, "http_snap.jsonl"))
		if err != nil {
			fmt.Println("建文件失败:", err)
			os.Exit(1)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer f.Close()
			snapLoop(*addr, snapTargets(paths), *snap, f, stop)
		}()
	}

	if *dur > 0 {
		fmt.Printf("\n录制 %v… （Ctrl+C 可提前结束）\n\n", *dur)
	} else {
		fmt.Print("\n录制中… Ctrl+C 结束\n\n")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	// duration 为 0 时 timeout 为 nil —— nil channel 永久阻塞，select 只等信号。
	var timeout <-chan time.Time
	if *dur > 0 {
		timeout = time.After(*dur)
	}
	select {
	case <-sig:
	case <-timeout:
	}

	fmt.Println("\n结束，关闭连接…")
	close(stop)
	wg.Wait()
	fmt.Println("完成，产物在", *out)
}

// recordLoop 录一个路径，断线自动重连，直到 stop 关闭。
//
// 被测程序在录制期间重启是常态（换版本、验修复、崩溃重启），而重连之后服务端会重发
// buildInitEvents —— 那本身就是要观察的东西。不重连的话，重启之后的一切都录不到，
// 且失败是**静默**的：文件还在、进程还在跑，只是再也不会有新行。
func recordLoop(path, addr string, f *os.File, stop <-chan struct{}) {
	url := "ws://" + addr + path
	enc := json.NewEncoder(f)
	attempt := 0

	for {
		select {
		case <-stop:
			return
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			continue
		}

		attempt++
		if attempt == 1 {
			fmt.Printf("已连接 %s → %s\n", url, filepath.Base(f.Name()))
		} else {
			ms := time.Since(origin).Milliseconds()
			fmt.Printf("[%6dms] %-16s <重连 #%d>\n", ms, path, attempt-1)
			// 落盘一条标记：分析时才知道这里断过，init 事件会重发一轮
			enc.Encode(record{MS: ms, Kind: "reconnect"})
		}

		// stop 时关掉连接，解阻塞下面的 ReadMessage
		closed := make(chan struct{})
		go func() {
			select {
			case <-stop:
				conn.Close()
			case <-closed:
			}
		}()

		record1(path, conn, enc)
		close(closed)
		conn.Close()
	}
}

// snapTargets 从 WS 路径推导要抓的 HTTP 端点：/ws → /all_lyrics…，/qqmusic/ws → /qqmusic/all_lyrics…
func snapTargets(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if !strings.HasSuffix(p, "/ws") {
			continue // SSE 路径不参与推导
		}
		prefix := strings.TrimSuffix(p, "/ws")
		for _, sfx := range httpSnapSuffixes {
			t := prefix + sfx
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// snapLoop 定时抓 HTTP 快照。
func snapLoop(addr string, targets []string, every time.Duration, f *os.File, stop <-chan struct{}) {
	if len(targets) == 0 {
		return
	}
	fmt.Printf("HTTP 快照：每 %v 抓 %d 个端点 → http_snap.jsonl\n", every, len(targets))

	enc := json.NewEncoder(f)
	client := &http.Client{Timeout: 3 * time.Second}
	tick := time.NewTicker(every)
	defer tick.Stop()

	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		for _, t := range targets {
			ms := time.Since(origin).Milliseconds()
			rec := record{MS: ms, Kind: "http", Path: t}

			resp, err := client.Get("http://" + addr + t)
			if err != nil {
				rec.Code = 0
				enc.Encode(rec)
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			rec.Code = resp.StatusCode
			rec.Data = json.RawMessage(body) // 原文照录，理由同 WS：字段顺序是契约
			enc.Encode(rec)
		}
	}
}

// sseLoop 跟一条 SSE 流，断线重连。
//
// SSE 的 wire format 是 `data: <payload>\n\n`（server.go 的 handleSSE）。只取 data 行、
// 原样落盘，与 WS 的 record 同构，故两份 jsonl 可直接对比。
func sseLoop(path, addr string, f *os.File, stop <-chan struct{}) {
	url := "http://" + addr + path
	enc := json.NewEncoder(f)
	attempt := 0

	for {
		select {
		case <-stop:
			return
		default:
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			continue
		}

		attempt++
		if attempt == 1 {
			fmt.Printf("已连接 %s → %s\n", url, filepath.Base(f.Name()))
		} else {
			ms := time.Since(origin).Milliseconds()
			fmt.Printf("[%6dms] %-16s <重连 #%d>\n", ms, path, attempt-1)
			enc.Encode(record{MS: ms, Kind: "reconnect"})
		}

		closed := make(chan struct{})
		go func() {
			select {
			case <-stop:
				resp.Body.Close()
			case <-closed:
			}
		}()

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // all_lyrics 可能很大，默认 64KB 会截断
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue // 事件流的空行/注释/event: 行
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}
			ms := time.Since(origin).Milliseconds()
			enc.Encode(record{MS: ms, Kind: "sse", Data: json.RawMessage(payload)})
			fmt.Printf("[%6dms] %-16s %s\n", ms, path, summarize([]byte(payload)))
		}
		close(closed)
		resp.Body.Close()
	}
}

// waitReady 轮询 TCP 端口直到能连上或超时。
//
// 只探 TCP，不做 WS 握手：握手成功会真的建立一个订阅者，服务端立刻推 init 事件，
// 而这个探测连接马上就关——那会在服务端日志里留下一次莫名的「WS 连接/断开」。
func waitReady(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		fmt.Printf("\r等待 %s 就绪…（剩 %ds）", addr, int(time.Until(deadline).Seconds()))
		time.Sleep(time.Second)
	}
}

// record1 录一个连接直到断开。enc 由调用方持有，跨重连复用同一个文件。
func record1(path string, conn *websocket.Conn, enc *json.Encoder) {
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return // 连接关闭（含 Ctrl+C 主动关）
		}

		ms := time.Since(origin).Milliseconds()
		rec := record{MS: ms}

		switch mt {
		case websocket.TextMessage:
			rec.Kind = "text"
			rec.Data = json.RawMessage(data) // 原文照录，不解析
			fmt.Printf("[%6dms] %-16s %s\n", ms, path, summarize(data))
		case websocket.BinaryMessage:
			// effect-ws 的 JPEG 帧：只记长度，不落盘图像（几十 KB/帧，录一分钟就上百 MB）
			rec.Kind = "binary"
			rec.Len = len(data)
			fmt.Printf("[%6dms] %-16s <binary %d bytes>\n", ms, path, len(data))
		default:
			continue
		}

		if err := enc.Encode(rec); err != nil {
			fmt.Println("写入失败:", err)
			return
		}
	}
}

// summarize 从事件里抠出 type 供 stdout 速览。
// 只为看得见发生了什么，落盘的仍是原文。
func summarize(data []byte) string {
	var probe struct {
		Type   string `json:"type"`
		Player string `json:"player"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Sprintf("<非 JSON %d bytes>", len(data))
	}
	s := probe.Type
	if probe.Player != "" {
		s += " (" + probe.Player + ")"
	}
	return s
}
