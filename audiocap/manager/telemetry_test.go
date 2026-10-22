package manager

// telemetry.go 的纯逻辑用例：stderr 尾部的容量与行数上限、脱敏、读泵的保真与背压、退出分类、事件构造。
// 无构建约束，GOOS=linux 可编译。接线（spawnHelper、demux、reapHelper、teardownHelper 等）见
// manager_telemetry_windows_test.go。

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// linesOf 返回 n 行、每行 width 字节（含换行）的文本，第 i 行以 "L<i>:" 开头，便于断言保留的是哪几行。
func linesOf(n, width int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		head := fmt.Sprintf("L%d:", i)
		b.WriteString(head + strings.Repeat("x", width-len(head)-1) + "\n")
	}
	return b.String()
}

// TestTailBufferCapacity 钉住 8 KiB 的容量边界与「截断点落在行中才丢首行」。
//
// 变异自证（实测）：丢首行的判断改成只看是否截断过（不看截断点前一字节是不是换行）→「截断点恰在换行」红；
// 去掉丢首行 →「超出一字节」与 TestTailBufferLines 的「截断后整段没有换行」红（多于 40 行的两例取末 40 行，
// 首行本就不在其中，不红）；容量比较改成 >= →「恰好容量」红。
func TestTailBufferCapacity(t *testing.T) {
	full := linesOf(32, 256) // 恰为 8192 字节、32 行
	if len(full) != tailCapBytes {
		t.Fatalf("夹具长度 %d，期望 %d", len(full), tailCapBytes)
	}
	for _, c := range []struct {
		name   string
		writes []string
		want   string
	}{
		{"恰好容量：不截断，32 行都在", []string{full}, full},
		{"超出一字节：首行的第一个字节被挤掉，残行丢弃", []string{"Z" + full}, ""},
		{"截断点恰在换行：首行完整，保留", []string{"\n" + full}, full},
		{"单次写大于容量：只留最后 8 KiB，残行丢弃", []string{linesOf(100, 100)}, ""},
		{"多次小写累计超出", splitEvery(linesOf(100, 100), 7), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var tb tailBuffer
			total := ""
			for _, w := range c.writes {
				n, err := tb.Write([]byte(w))
				if n != len(w) || err != nil {
					t.Fatalf("Write 返回 (%d, %v)，期望 (%d, nil)", n, err, len(w))
				}
				total += w
			}
			if len(tb.buf) > tailCapBytes {
				t.Fatalf("缓冲 %d 字节，超过上限 %d", len(tb.buf), tailCapBytes)
			}
			if want := total[max(0, len(total)-tailCapBytes):]; string(tb.buf) != want {
				t.Fatalf("缓冲不是最近 %d 字节", tailCapBytes)
			}
			want := c.want
			if want == "" {
				// 期望为「最近 8 KiB 去掉开头的残行，再取最后 40 行」。
				kept := total[len(total)-tailCapBytes:]
				kept = kept[strings.IndexByte(kept, '\n')+1:]
				want = lastLines(kept, tailMaxLines)
			}
			if got := tb.Snapshot(); got != want {
				t.Fatalf("Snapshot 首行 %q，期望首行 %q（共 %d / %d 字节）", firstLine(got), firstLine(want), len(got), len(want))
			}
		})
	}
}

func splitEvery(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestTailBufferLines 钉住行数上限与各种边角：多于 40 行只留最后 40 行；末行没有换行照常计入；
// 截断后整段没有换行（一行超过 8 KiB）则返回空串；空缓冲与 nil 缓冲返回空串。
//
// 变异自证（实测）：lastLines 把末尾换行也算一行 →「恰 40 行」「恰 41 行」红；行数上限改成 41 →「恰 41 行」
// 「41 行且末行无换行」红；去掉 nil 接收者判断 →「nil 缓冲」panic。
func TestTailBufferLines(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
	}{
		{"空缓冲", "", ""},
		{"一行无换行", "abc", "abc"},
		{"多行无末尾换行", "a\nb\nc", "a\nb\nc"},
		{"恰 40 行", linesOf(40, 10), linesOf(40, 10)},
		{"恰 41 行：丢第 0 行", linesOf(41, 10), linesOf(41, 10)[10:]},
		{"41 行且末行无换行", linesOf(40, 10) + "tail", linesOf(40, 10)[10:] + "tail"},
		{"只有换行", "\n\n\n", "\n\n\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var tb tailBuffer
			tb.Write([]byte(c.in))
			if got := tb.Snapshot(); got != c.want {
				t.Fatalf("Snapshot = %q，期望 %q", got, c.want)
			}
		})
	}

	t.Run("截断后整段没有换行", func(t *testing.T) {
		var tb tailBuffer
		tb.Write([]byte(strings.Repeat("y", 3*tailCapBytes)))
		if got := tb.Snapshot(); got != "" {
			t.Fatalf("一行超过容量时 Snapshot 应为空串（整段是残行），得到 %d 字节", len(got))
		}
	})
	t.Run("nil 缓冲", func(t *testing.T) {
		var tb *tailBuffer
		if got := tb.Snapshot(); got != "" {
			t.Fatalf("nil 缓冲 Snapshot = %q", got)
		}
	})
}

// TestTailBufferConcurrent 并发写与取快照（配合 -race）：任何时刻快照都不超过 40 行与 8 KiB。
func TestTailBufferConcurrent(t *testing.T) {
	var tb tailBuffer
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				fmt.Fprintf(&tb, "writer %d line %d %s\n", w, i, strings.Repeat("z", i%50))
			}
		}(w)
	}
	stop := make(chan struct{})
	var rwg sync.WaitGroup
	rwg.Add(1)
	go func() {
		defer rwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s := tb.Snapshot()
			if len(s) > tailCapBytes || strings.Count(strings.TrimSuffix(s, "\n"), "\n") >= tailMaxLines {
				t.Errorf("快照 %d 字节、%d 个换行，超出上限", len(s), strings.Count(s, "\n"))
				return
			}
		}
	}()
	wg.Wait()
	close(stop)
	rwg.Wait()
	if got := strings.Count(tb.Snapshot(), "\n"); got != tailMaxLines {
		t.Fatalf("写满后快照 %d 行，期望 %d", got, tailMaxLines)
	}
}

// TestRedactUserDirs 钉住用户目录脱敏：中文、空格、正斜杠、%q 转义后的双反斜杠、引号与行尾作结尾、一处与多处；
// 反例（盘符缺失、不是 Users 目录、Users 只是前缀）原样不动。
//
// 变异自证（实测）：去掉 (?i) → 写作 Users 的各例红；用户名字符集改成 \w+ →「中文用户名」「含空格」红；
// 分隔符只认反斜杠 →「正斜杠」红；分隔符不许连写 →「双反斜杠」红；用户名字符集不排除换行 →「行尾结束」红。
func TestRedactUserDirs(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
	}{
		{"中文用户名", `C:\Users\张三\x`, `C:\Users\<user>\x`},
		{"小写盘符与 users，含空格", `c:\users\a b\x`, `c:\users\<user>\x`},
		{"正斜杠（Go 栈里的源码路径）", `C:/Users/bob/go/src/main.go:12`, `C:/Users/<user>/go/src/main.go:12`},
		{"双反斜杠（%q 转义）", `exec: "C:\\Users\\bob\\a.exe": file does not exist`, `exec: "C:\\Users\\<user>\\a.exe": file does not exist`},
		{"用户名后紧跟引号", `"C:\Users\bob" next`, `"C:\Users\<user>" next`},
		{"串尾结束", `home is C:\Users\bob`, `home is C:\Users\<user>`},
		{"行尾结束", "C:\\Users\\bob\nnext line", "C:\\Users\\<user>\nnext line"},
		{"多处、多行", "D:\\Users\\a\\1 E:/USERS/b/2\nF:\\Users\\c d\\3", "D:\\Users\\<user>\\1 E:/USERS/<user>/2\nF:\\Users\\<user>\\3"},
		{"反例：不是 Users 目录", `C:\Program Files\PlayerCap\x.exe`, `C:\Program Files\PlayerCap\x.exe`},
		{"反例：没有盘符", `\Users\bob\x`, `\Users\bob\x`},
		{"反例：Users 只是前缀", `C:\Usersx\bob\x`, `C:\Usersx\bob\x`},
		{"反例：Users 后没有用户名", `C:\Users\`, `C:\Users\`},
		{"反例：无路径", `[helper] activate failed hr=0x88890008 pid=1234`, `[helper] activate failed hr=0x88890008 pid=1234`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := redactUserDirs(c.in); got != c.want {
				t.Fatalf("redactUserDirs(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// TestSnapshotRedacts 断言快照经过脱敏。变异自证（实测）：Snapshot 不调 redactUserDirs 即红。
func TestSnapshotRedacts(t *testing.T) {
	var tb tailBuffer
	tb.Write([]byte("[helper] load C:\\Users\\张三\\AppData\\x.dll failed\n"))
	got := tb.Snapshot()
	if strings.Contains(got, "张三") || !strings.Contains(got, `C:\Users\<user>\AppData`) {
		t.Fatalf("快照未脱敏: %q", got)
	}
}

// chunkReader 每次 Read 返回随机长度（1～max 字节）的一块，最后返回 err。
type chunkReader struct {
	data []byte
	rnd  *rand.Rand
	max  int
	err  error
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := min(1+r.rnd.Intn(r.max), len(r.data), len(p))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// errWriter 的每次 Write 都返回错误，记下收到的总字节数。
type errWriter struct{ n int }

func (w *errWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return 0, errors.New("console gone")
}

// TestPumpStderrFidelity 断言读泵逐字节保真：tee 到写端的字节与读到的相同、尾部与之一致；读端以 EOF
// 或其他错误结束都返回；写端出错照样读完。
//
// 变异自证（实测）：读泵不 tee 写端（丢字节）→「随机分块」红；写端出错即返回 →「写端出错」红；
// 先写写端、只把写成功的部分写入尾部 →「写端出错」红。
func TestPumpStderrFidelity(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	data := make([]byte, 50_000)
	for i := range data {
		data[i] = "abc \n\xe5\xbc\xa0"[rnd.Intn(8)] // 含换行与多字节 UTF-8 的片段
	}

	for _, endErr := range []error{io.EOF, errors.New("pipe broken")} {
		t.Run(fmt.Sprintf("随机分块，结束于 %v", endErr), func(t *testing.T) {
			var tail tailBuffer
			var out bytes.Buffer
			pumpStderr(&chunkReader{data: data, rnd: rnd, max: 5000, err: endErr}, &tail, &out)
			if !bytes.Equal(out.Bytes(), data) {
				t.Fatalf("写端收到 %d 字节，与读到的 %d 字节不同", out.Len(), len(data))
			}
			if !bytes.Equal(tail.buf, data[len(data)-tailCapBytes:]) {
				t.Fatal("尾部不是读到的最后 8 KiB")
			}
		})
	}

	t.Run("写端出错", func(t *testing.T) {
		var tail tailBuffer
		w := &errWriter{}
		pumpStderr(&chunkReader{data: data, rnd: rnd, max: 3000, err: io.EOF}, &tail, w)
		if w.n != len(data) {
			t.Fatalf("写端出错后读泵停了：写端共收到 %d 字节，期望 %d", w.n, len(data))
		}
		if !bytes.Equal(tail.buf, data[len(data)-tailCapBytes:]) {
			t.Fatal("写端出错时尾部没有照常记下")
		}
	})
}

// barrierWriter 在 release 关闭之前阻塞每次 Write；首次进入 Write 时关闭 entered。
type barrierWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	entered chan struct{}
	once    sync.Once
	release chan struct{}
}

func newBarrierWriter() *barrierWriter {
	return &barrierWriter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *barrierWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *barrierWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// TestPumpStderrBackpressure 断言写端卡住时读泵随之卡住（背压留在写入方），释放后继续、不丢字节。
// 读端是同步的 io.Pipe：读泵不再读时，写入方的第二次 Write 阻塞。
//
// 变异自证（实测）：读泵改成异步转发（另起 goroutine 写写端，读泵照读）→「第二块被挡住」红。
func TestPumpStderrBackpressure(t *testing.T) {
	pr, pw := io.Pipe()
	out := newBarrierWriter()
	var tail tailBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpStderr(pr, &tail, out)
	}()

	first, second := []byte("first chunk\n"), []byte("second chunk\n")
	if _, err := pw.Write(first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内读泵没有把第一块写往写端")
	}
	if got := tail.Snapshot(); got != string(first) {
		t.Fatalf("写端卡住前尾部应已有第一块，得到 %q", got)
	}

	wrote := make(chan struct{})
	go func() {
		pw.Write(second)
		pw.Close()
		close(wrote)
	}()
	select {
	case <-wrote:
		t.Fatal("写端卡住时第二块没有被挡住：背压没有留在写入方")
	case <-time.After(200 * time.Millisecond):
	}

	close(out.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("释放后读泵 5s 内未返回")
	}
	<-wrote
	if got, want := out.bytes(), append(append([]byte(nil), first...), second...); !bytes.Equal(got, want) {
		t.Fatalf("写端收到 %q，期望 %q", got, want)
	}
}

// TestClassifyExit 钉住「自行退出且不是 Ctrl+C / Ctrl+Break」的判定。killMarkedAt 与退出时刻均为注入值，
// 退出时刻代表进程真实的退出时刻。
//
// 变异自证（实测）：去掉 killMarkedAt 比较（只看 known 与退出码）→ 两例「先下手、进程随后才死」红；去掉
// 0xC000013A 排除 →「Ctrl+C / Ctrl+Break，未下手」红；不看 known →「未走到 Wait」两例红；比较写反 →「先死后下手」
// 与两例「先下手」红；边界改回「不晚于」（含相等）→「同一时刻」红。
func TestClassifyExit(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	var zero time.Time
	for _, c := range []struct {
		name   string
		e      exitInfo
		killAt time.Time
		want   bool
	}{
		{"自行退出，码 0", exitInfo{true, 0, t0}, zero, true},
		{"自行退出，码 1", exitInfo{true, 1, t0}, zero, true},
		{"自行退出，码 2（Go 致命错误）", exitInfo{true, 2, t0}, zero, true},
		{"自行退出，访问违例", exitInfo{true, 0xC0000005, t0}, zero, true},
		{"Ctrl+C / Ctrl+Break，未下手", exitInfo{true, statusControlCExit, t0}, zero, false},
		{"Ctrl+C / Ctrl+Break，下手之后", exitInfo{true, statusControlCExit, t0.Add(time.Second)}, t0, false},
		{"未走到 Wait（demux panic）", exitInfo{false, 2, t0}, zero, false},
		{"未走到 Wait 且下手过", exitInfo{false, 0, t0}, t0.Add(time.Second), false},
		{"看门狗或拆除先下手、进程随后才死（Kill 的码 1）", exitInfo{true, 1, t0.Add(300 * time.Microsecond)}, t0, false},
		{"拆除先下手、进程随后才死（Job 的码 0）", exitInfo{true, 0, t0.Add(150 * time.Microsecond)}, t0, false},
		{"先死后下手：进程已死、demux 未返回时下手", exitInfo{true, 3, t0}, t0.Add(time.Millisecond), true},
		// 时钟按中断步进，下手与被杀常读到相同的值：算 manager 所杀，不报（宁可漏报不误报）。
		{"同一时刻", exitInfo{true, 3, t0}, t0, false},
		{"早一个最小刻度", exitInfo{true, 3, t0.Add(-time.Nanosecond)}, t0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyExit(c.e, c.killAt); got != c.want {
				t.Fatalf("classifyExit(%+v, %v) = %v，期望 %v", c.e, c.killAt, got, c.want)
			}
		})
	}
}

// TestTelemetryEventPayloads 钉住三个事件的 key、tags 与 extra 的键与值，可选字段缺省时不出现。
//
// 变异自证（实测）：exit_code 改成十进制、alive_ms 改成秒、last_exit_code 无条件写、spawn_error 不脱敏、
// capture_gave_up 的 key 拼进播放器名，均红。
func TestTelemetryEventPayloads(t *testing.T) {
	for _, c := range []struct {
		name string
		got  TelemetryEvent
		want TelemetryEvent
	}{
		{
			"helper 自行退出",
			HelperExitedEvent(0xC0000005, 8500*time.Millisecond, 5, "tail"),
			TelemetryEvent{Key: "audiocap.helper_exited_abnormally", Tags: map[string]string{"exit_code": "0xC0000005"},
				Extra: map[string]any{"alive_ms": int64(8500), "quick_fails": 5, "stderr_tail": "tail"}},
		},
		{
			"helper 自行退出，码 0",
			HelperExitedEvent(0, 0, 0, ""),
			TelemetryEvent{Key: "audiocap.helper_exited_abnormally", Tags: map[string]string{"exit_code": "0x0"},
				Extra: map[string]any{"alive_ms": int64(0), "quick_fails": 0, "stderr_tail": ""}},
		},
		{
			"helper 放弃：快速退出，有上次退出码",
			HelperGaveUpEvent(causeQuickExit, 5, 2, true, nil, "tail"),
			TelemetryEvent{Key: "audiocap.helper_gave_up", Tags: map[string]string{"cause": "quick_exit"},
				Extra: map[string]any{"quick_fails": 5, "last_exit_code": "0x2", "stderr_tail": "tail"}},
		},
		{
			"helper 放弃：spawn 失败，从未有过 helper",
			HelperGaveUpEvent(causeSpawnFailure, 5, 0, false, errors.New(`exec: "C:\\Users\\bob\\p.exe": Access is denied.`), ""),
			TelemetryEvent{Key: "audiocap.helper_gave_up", Tags: map[string]string{"cause": "spawn_failure"},
				Extra: map[string]any{"quick_fails": 5, "spawn_error": `exec: "C:\\Users\\<user>\\p.exe": Access is denied.`, "stderr_tail": ""}},
		},
		{
			"捕获放弃",
			CaptureGaveUpEvent("cloudmusicv3", 5, "hr=0x88890008"),
			TelemetryEvent{Key: "audiocap.capture_gave_up", Tags: map[string]string{"player": "cloudmusicv3"},
				Extra: map[string]any{"failures": 5, "stderr_tail": "hr=0x88890008"}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.got.Key != c.want.Key {
				t.Errorf("Key = %q，期望 %q", c.got.Key, c.want.Key)
			}
			if c.got.Message == "" {
				t.Error("Message 为空")
			}
			if !reflect.DeepEqual(c.got.Tags, c.want.Tags) {
				t.Errorf("Tags = %v，期望 %v", c.got.Tags, c.want.Tags)
			}
			if !reflect.DeepEqual(c.got.Extra, c.want.Extra) {
				t.Errorf("Extra = %#v，期望 %#v", c.got.Extra, c.want.Extra)
			}
		})
	}
}

// TestSampleTelemetryEventsCoverAllFields 断言样本每种事件各一个、可选字段都填上：隐私门禁靠它看到全部键。
//
// 变异自证（实测）：样本里的 HelperGaveUpEvent 不传上次退出码或 spawn 错误，即红。
func TestSampleTelemetryEventsCoverAllFields(t *testing.T) {
	wantExtra := map[string][]string{
		keyHelperExited:  {"alive_ms", "quick_fails", "stderr_tail"},
		keyHelperGaveUp:  {"last_exit_code", "quick_fails", "spawn_error", "stderr_tail"},
		keyCaptureGaveUp: {"failures", "stderr_tail"},
	}
	samples := SampleTelemetryEvents()
	if len(samples) != len(wantExtra) {
		t.Fatalf("样本 %d 个，期望 %d 个（每种事件各一）", len(samples), len(wantExtra))
	}
	for _, ev := range samples {
		want, ok := wantExtra[ev.Key]
		if !ok {
			t.Errorf("样本里有未知的 key %q", ev.Key)
			continue
		}
		delete(wantExtra, ev.Key)
		var got []string
		for k := range ev.Extra {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s 样本的 extra 键 = %v，期望 %v", ev.Key, got, want)
		}
		if len(ev.Tags) != 1 {
			t.Errorf("%s 样本的 tags = %v，期望恰一个", ev.Key, ev.Tags)
		}
	}
}
