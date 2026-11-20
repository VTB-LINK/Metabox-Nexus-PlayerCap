package main

// 本文件只测一件事：自动更新的下载层（updatefetch.go，issue #213、#214）——来源列表、一次请求的读取与
// 低速判据、写成 .new 并核对大小与 SHA256、进度行的收尾换行、两个来源同时测速，以及把全部附件下载为
// .new 的编排（测速接续、换源、失败时全部删除）。
//
// 全部以 testFetchParams 与 httptest 服务器运行，不触及 os.Executable 与正在运行的 exe。「慢」服务器
// 每 100 ms 发 1 KiB（10 KiB/s），低速门槛是 600 ms 内 48 KiB（80 KiB/s），两者差 8 倍，留给 CI
// （Windows，2 核）上的计时抖动。
//
// 错误「是哪一条文案」用 errMatchesKey 按当前语言的译文判断，不写死中文或英文。

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

var testFetchParams = fetchParams{
	RaceWindow:  400 * time.Millisecond,
	RaceCap:     64 << 10,
	SlowWindow:  600 * time.Millisecond,
	SlowMin:     48 << 10,
	Sample:      50 * time.Millisecond,
	FileTimeout: 5 * time.Second,
}

const testFileSize = 256 << 10

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func assetFor(b []byte) updateAsset {
	return updateAsset{Name: "PlayerCap.exe", Size: int64(len(b)), Digest: sha256Digest(b)}
}

var fmtVerb = regexp.MustCompile(`%(\[\d+\])?[.\d]*[a-z]`)

// errMatchesKey 判 err 的全文是否由英文键 key 在当前语言下的译文格式化而来：译文的字面段原样匹配，
// %d 只匹配整数，其余占位符匹配任意文本。%d 若也放任意文本，以占位符开头的键（「%d 分钟内未下载完」）
// 会把外面多包的一层（「下载中断: 0 分钟内未下载完」）也认作匹配。
func errMatchesKey(err error, key string) bool {
	if err == nil {
		return false
	}
	tr := i18n.T(key)
	re := `(?s)^`
	last := 0
	for _, loc := range fmtVerb.FindAllStringIndex(tr, -1) {
		re += regexp.QuoteMeta(tr[last:loc[0]])
		if tr[loc[1]-1] == 'd' {
			re += `-?\d+`
		} else {
			re += `.*`
		}
		last = loc[1]
	}
	re += regexp.QuoteMeta(tr[last:]) + `$`
	return regexp.MustCompile(re).MatchString(err.Error())
}

// serveFile 起一个带 Content-Length、一次发出 body 的服务器，返回它与收到的请求数。
func serveFile(t *testing.T, body []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// servePaced 起一个不带 Content-Length 的服务器：先一次发出 body 的前 burst 字节，再每 every 发 chunk
// 字节，请求被取消即停。转入限速的时刻写进返回的通道。
func servePaced(t *testing.T, body []byte, burst, chunk int, every time.Duration) (*httptest.Server, <-chan time.Time) {
	t.Helper()
	switched := make(chan time.Time, 1)
	srv := httptest.NewServer(pacedHandler(body, burst, chunk, every, switched))
	t.Cleanup(srv.Close)
	return srv, switched
}

// pacedHandler 是 servePaced 的应答；switched 为 nil 时不记转入限速的时刻。
func pacedHandler(body []byte, burst, chunk int, every time.Duration, switched chan<- time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		sent := min(burst, len(body))
		w.Write(body[:sent])
		f.Flush()
		select {
		case switched <- time.Now():
		default:
		}
		for sent < len(body) {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(every):
			}
			end := min(sent+chunk, len(body))
			if _, err := w.Write(body[sent:end]); err != nil {
				return
			}
			f.Flush()
			sent = end
		}
	}
}

// serveSlow 是 Global Constraints 里的「慢」服务器：每 100 ms 发 1 KiB。
func serveSlow(t *testing.T, body []byte) *httptest.Server {
	srv, _ := servePaced(t, body, 0, 1<<10, 100*time.Millisecond)
	return srv
}

func newTestTransfer(p fetchParams, url string, guarded bool) *transfer {
	return newTransfer(p, updateSource{Name: "test", Prefix: url}, url, guarded)
}

// download 对 url 走一遍 newTransfer、open、receive，写到临时目录里的 .new。
func download(t *testing.T, p fetchParams, url string, guarded bool, asset updateAsset) (string, *transfer, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "PlayerCap.exe.new")
	tr := newTestTransfer(p, url, guarded)
	if err := tr.open(&http.Client{}); err != nil {
		tr.close()
		return dest, tr, err
	}
	return dest, tr, receive(tr, nil, false, asset, dest)
}

func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("写出的文件与原文件不同：%d 字节，应为 %d 字节", len(got), len(want))
	}
}

func assertRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s 应已删除，Lstat 返回 %v", path, err)
	}
}

// TestUpdateSources 钉住来源列表：国内在前、全球在后，空串剔除，相同只留一个，Name 为前缀的主机名。
//
// 变异自证（实测）：顺序改为全球在前；不剔除空串；不去重；Name 取整个前缀——均红。
func TestUpdateSources(t *testing.T) {
	const gh = "https://github.com/o/r/releases/download/"
	const ge = "https://gitee.com/o/r/releases/download/"
	cases := []struct {
		name          string
		global, china string
		want          []updateSource
	}{
		{"主机名", gh, ge, []updateSource{{"gitee.com", ge}, {"github.com", gh}}},
		{"国内在前", "g/", "c/", []updateSource{{"c/", "c/"}, {"g/", "g/"}}},
		{"全球为空", "", "c/", []updateSource{{"c/", "c/"}}},
		{"国内为空", "g/", "", []updateSource{{"g/", "g/"}}},
		{"都为空", "", "", nil},
		{"两者相同", "same/", "same/", []updateSource{{"same/", "same/"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := updateSources(c.global, c.china)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("updateSources(%q, %q) = %+v，应为 %+v", c.global, c.china, got, c.want)
			}
		})
	}
	if got := (updateSource{Prefix: gh}).url("v4.0.0", "a.exe"); got != gh+"v4.0.0/a.exe" {
		t.Fatalf("url = %q", got)
	}
}

// TestReceiveWithPrefix：先从 transfer 读出一段作 prefix，再由 receive 接着写完——文件逐字节相同、
// SHA256 通过，服务器只收到 1 次请求。
//
// 变异自证（实测）：receive 不写 prefix（只写余下部分）→ SHA256 不符，红；receive 另起请求读余下部分 →
// 请求数 2，红。
func TestReceiveWithPrefix(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, hits := serveFile(t, body)
	tr := newTestTransfer(testFetchParams, srv.URL, false)
	if err := tr.open(&http.Client{}); err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, 10<<10)
	if _, err := io.ReadFull(tr, prefix); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "a.new")
	if err := receive(tr, prefix, false, assetFor(body), dest); err != nil {
		t.Fatalf("receive: %v", err)
	}
	assertFileEquals(t, dest, body)
	if n := hits.Load(); n != 1 {
		t.Fatalf("服务器收到 %d 次请求，应为 1 次", n)
	}
}

// TestReceiveDonePrefix：prefix 已是整个响应体（done）时，receive 只写 prefix、不再读 transfer。
//
// 变异自证（实测）：忽略 done、照常读 transfer → 判多出，红。
func TestReceiveDonePrefix(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, _ := serveFile(t, body)
	tr := newTestTransfer(testFetchParams, srv.URL, false)
	if err := tr.open(&http.Client{}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "a.new")
	if err := receive(tr, body, true, assetFor(body), dest); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Fatalf("done 时仍从 transfer 读了 %d 字节", n)
	}
	assertFileEquals(t, dest, body)
}

// TestTransferStatus404：状态码非 200 是来源失败。
//
// 变异自证（实测）：open 不查状态码 → 返回 nil，红；状态码错误不包 sourceError → 红。
func TestTransferStatus404(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	tr := newTestTransfer(testFetchParams, srv.URL, false)
	defer tr.close()
	err := tr.open(&http.Client{})
	if !isSourceError(err) || !errMatchesKey(err, "HTTP %d") {
		t.Fatalf("open 返回 %v，应为来源失败「HTTP 404」", err)
	}
}

// TestTransferConnRefused：连不上是来源失败。
//
// 变异自证（实测）：请求出错不包 sourceError → 红。
func TestTransferConnRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String() + "/a.exe"
	ln.Close()
	tr := newTestTransfer(testFetchParams, url, false)
	defer tr.close()
	err = tr.open(&http.Client{})
	if !isSourceError(err) || !errMatchesKey(err, "connection failed: %v") {
		t.Fatalf("open 返回 %v，应为来源失败「连接失败」", err)
	}
}

// TestReceiveTruncatedNoContentLength：不带 Content-Length 的响应发到一半正常结束（截断），按网关下发的
// 大小判为来源失败，不落到 SHA256 不符；.new 不留。
//
// 变异自证（实测）：去掉大小核对 → 落到 SHA256 不符、不是来源失败，红；大小不符不包 sourceError → 红；
// 失败时不删 .new → 红。
func TestReceiveTruncatedNoContentLength(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)
	dest, _, err := download(t, testFetchParams, srv.URL, false, assetFor(body))
	if !isSourceError(err) || !errMatchesKey(err, "size mismatch: expected %d bytes, got %d") {
		t.Fatalf("receive 返回 %v，应为来源失败「大小不符」", err)
	}
	assertRemoved(t, dest)
}

// TestReceiveOversize：收到的字节超过网关下发的大小是来源失败。
//
// 变异自证（实测）：去掉「多出」的判定、只比总长 → 报成大小不符，红；多出不包 sourceError → 红。
func TestReceiveOversize(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, _ := serveFile(t, append(append([]byte{}, body...), 0x5a))
	dest, _, err := download(t, testFetchParams, srv.URL, false, assetFor(body))
	if !isSourceError(err) || !errMatchesKey(err, "received more than the expected %d bytes") {
		t.Fatalf("receive 返回 %v，应为来源失败「超过应有的字节」", err)
	}
	assertRemoved(t, dest)
}

// TestReceiveErrorPage200：来源以 200 返回几 KB 的错误页（如 Gitee 限流页），网关下发了大小时按大小不符
// 换源，不得落到 SHA256 不符而终止。
//
// 变异自证（实测）：SHA256 核对挪到大小核对之前 → 不是来源失败，红。
func TestReceiveErrorPage200(t *testing.T) {
	body := randomBytes(t, testFileSize)
	page := []byte("<html><body>" + strings.Repeat("rate limited ", 230) + "</body></html>")
	srv, _ := serveFile(t, page)
	dest, _, err := download(t, testFetchParams, srv.URL, false, assetFor(body))
	if !isSourceError(err) || !errMatchesKey(err, "size mismatch: expected %d bytes, got %d") {
		t.Fatalf("receive 返回 %v，应为来源失败「大小不符」", err)
	}
	assertRemoved(t, dest)
}

// TestReceiveDigestMismatch：大小对而 SHA256 不符，不是来源失败（AGENTS.md §3.5：删文件、终止，不换源）。
//
// 变异自证（实测）：SHA256 不符包成 sourceError → 红；不核对 SHA256 → 返回 nil，红；不删 .new → 红。
func TestReceiveDigestMismatch(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, _ := serveFile(t, body)
	asset := assetFor(body)
	asset.Digest = sha256Digest([]byte("other"))
	dest, _, err := download(t, testFetchParams, srv.URL, false, asset)
	if err == nil || isSourceError(err) || !errMatchesKey(err, "SHA256 verification failed: expected %s, got %s") {
		t.Fatalf("receive 返回 %v，应为非来源失败的 SHA256 不符", err)
	}
	assertRemoved(t, dest)
}

// TestReceiveNoSizeNoDigest：网关下发的 Size 为 0、Digest 为空时不做两项核对，照常成功。
//
// 变异自证（实测）：Size 为 0 时仍按 LimitReader(t, Size-len(prefix)+1) 读 → 只写出 1 字节，红；
// Digest 为空仍核对 → 红。
func TestReceiveNoSizeNoDigest(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, _ := serveFile(t, body)
	dest, _, err := download(t, testFetchParams, srv.URL, false, updateAsset{Name: "a.exe"})
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	assertFileEquals(t, dest, body)
}

// faultyFile 是可注入 Sync、Close 错误的写出目标，底下是真文件（Close 时真的关闭，删得掉）。
type faultyFile struct {
	*os.File
	syncErr, closeErr error
	closed            bool
}

func (f *faultyFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *faultyFile) Close() error {
	f.closed = true
	if err := f.File.Close(); err != nil {
		return err
	}
	return f.closeErr
}

func swapCreateUpdateFile(t *testing.T, create func(string) (updateFile, error)) {
	t.Helper()
	orig := createUpdateFile
	t.Cleanup(func() { createUpdateFile = orig })
	createUpdateFile = create
}

func injectFaultyFile(t *testing.T, ff *faultyFile) {
	swapCreateUpdateFile(t, func(path string) (updateFile, error) {
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		ff.File = f
		return ff, nil
	})
}

// TestReceiveSyncFails：Sync 失败是本地错误，Close 仍被调用，.new 删除。
//
// 变异自证（实测）：不检查 Sync 的返回值 → 返回 nil，红；Sync 失败后不 Close → 红；Sync 错误包成
// sourceError → 红。
func TestReceiveSyncFails(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, _ := serveFile(t, body)
	ff := &faultyFile{syncErr: errors.New("sync failed")}
	injectFaultyFile(t, ff)
	dest, _, err := download(t, testFetchParams, srv.URL, false, assetFor(body))
	if err == nil || isSourceError(err) || !errMatchesKey(err, "failed to flush file: %v") {
		t.Fatalf("receive 返回 %v，应为非来源失败的「写入磁盘失败」", err)
	}
	if !ff.closed {
		t.Fatal("Sync 失败后没有 Close")
	}
	assertRemoved(t, dest)
}

// TestReceiveCloseFails：Close 失败是本地错误，.new 删除。
//
// 变异自证（实测）：不检查 Close 的返回值 → 返回 nil，红。
func TestReceiveCloseFails(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, _ := serveFile(t, body)
	ff := &faultyFile{closeErr: errors.New("close failed")}
	injectFaultyFile(t, ff)
	dest, _, err := download(t, testFetchParams, srv.URL, false, assetFor(body))
	if err == nil || isSourceError(err) || !errMatchesKey(err, "failed to close file: %v") {
		t.Fatalf("receive 返回 %v，应为非来源失败的「关闭文件失败」", err)
	}
	assertRemoved(t, dest)
}

// TestReceiveCreateFails：建文件失败是本地错误。
//
// 变异自证（实测）：建文件错误包成 sourceError → 红。
func TestReceiveCreateFails(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, _ := serveFile(t, body)
	swapCreateUpdateFile(t, func(string) (updateFile, error) { return nil, errors.New("access denied") })
	_, _, err := download(t, testFetchParams, srv.URL, false, assetFor(body))
	if err == nil || isSourceError(err) || !errMatchesKey(err, "failed to create file: %v") {
		t.Fatalf("receive 返回 %v，应为非来源失败的「创建文件失败」", err)
	}
}

// TestTransferFileTimeout：整文件时限由 transfer 的 context 控制，到期是来源失败，错误以时限为准。
//
// 变异自证（实测）：不设时限 → 慢服务器约 25 s 发完、返回 nil，红；Read 不取 context 的取消原因 → 文本
// 是「下载中断: 0 分钟内未下载完」，红。
func TestTransferFileTimeout(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv := serveSlow(t, body)
	p := testFetchParams
	p.FileTimeout = 300 * time.Millisecond
	_, _, err := download(t, p, srv.URL, false, assetFor(body))
	if !isSourceError(err) || !errMatchesKey(err, "not finished within %d min") {
		t.Fatalf("返回 %v，应为来源失败「N 分钟内未下载完」", err)
	}
}

// TestTransferFollowsRedirect：下载地址先 302 跳到另一主机（GitHub 即如此），请求与计数都跟随跳转。
//
// 变异自证（实测）：open 改用 http.DefaultTransport.RoundTrip（不跟随跳转）→ 返回「HTTP 302」，红。
func TestTransferFollowsRedirect(t *testing.T) {
	body := randomBytes(t, testFileSize)
	b, _ := serveFile(t, body)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/asset", http.StatusFound)
	}))
	t.Cleanup(a.Close)
	dest, tr, err := download(t, testFetchParams, a.URL, false, assetFor(body))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if n := tr.n.Load(); n != int64(len(body)) {
		t.Fatalf("计数 %d，应为 %d", n, len(body))
	}
	assertFileEquals(t, dest, body)
}

// assertTooSlow 断言 err 是低速判据判出的来源失败。
func assertTooSlow(t *testing.T, err error) {
	t.Helper()
	if !isSourceError(err) || !errMatchesKey(err, "too slow: %d KB in the last %d s") {
		t.Fatalf("返回 %v，应为来源失败「速度过慢」", err)
	}
}

// TestSlowGuardTriggers：持续低于门槛，约一个窗口后判出。
//
// 变异自证（实测）：删掉低速判据的 goroutine → 慢服务器要 25 s，到 FileTimeout 才失败、文本是时限，红；
// 未满一个窗口就判（去掉「距 start 满 SlowWindow」）→ 首个采样即判出、耗时不足一个窗口，红。
func TestSlowGuardTriggers(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv := serveSlow(t, body)
	start := time.Now()
	_, _, err := download(t, testFetchParams, srv.URL, true, assetFor(body))
	elapsed := time.Since(start)
	assertTooSlow(t, err)
	if lo, hi := testFetchParams.SlowWindow, testFetchParams.SlowWindow+500*time.Millisecond; elapsed < lo || elapsed > hi {
		t.Fatalf("耗时 %v，应在 [%v, %v]", elapsed, lo, hi)
	}
}

// TestSlowGuardStall：发一段后停住、连接不断，同样判出——采样不依赖读取返回。
//
// 变异自证（实测）：采样改由 Read 返回驱动（不用计时器）→ 读阻塞、判不出，到 FileTimeout 才失败，红；
// 同一变异下 TestSlowGuardTriggers 仍绿（慢服务器持续有数据，Read 照常返回）。
func TestSlowGuardStall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 8<<10))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	body := randomBytes(t, testFileSize)
	start := time.Now()
	_, _, err := download(t, testFetchParams, srv.URL, true, assetFor(body))
	elapsed := time.Since(start)
	assertTooSlow(t, err)
	if lo, hi := testFetchParams.SlowWindow, testFetchParams.SlowWindow+500*time.Millisecond; elapsed < lo || elapsed > hi {
		t.Fatalf("耗时 %v，应在 [%v, %v]", elapsed, lo, hi)
	}
}

// TestSlowGuardFastThenSlow：先快后慢，在慢下来后一个窗口内判出——判的是最近一个窗口，不是自发起以来的平均。
//
// 变异自证（实测）：增量改为自发起以来累计（不丢旧样本）→ 128 KiB 一直计在内、判不出，红。
func TestSlowGuardFastThenSlow(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, switched := servePaced(t, body, 128<<10, 1<<10, 100*time.Millisecond)
	_, _, err := download(t, testFetchParams, srv.URL, true, assetFor(body))
	end := time.Now()
	assertTooSlow(t, err)
	at := <-switched
	if limit := at.Add(testFetchParams.SlowWindow + 500*time.Millisecond); end.After(limit) {
		t.Fatalf("转慢之后 %v 才判出，应不超过 %v", end.Sub(at), testFetchParams.SlowWindow+500*time.Millisecond)
	}
}

// TestSlowGuardNotTriggeredWhenFast：持续高于门槛不触发。服务器每 100 ms 发 32 KiB（320 KiB/s，门槛的
// 4 倍），256 KiB 约 0.8 s 发完，跨过首个采样窗口，判据确实检查过它。
//
// 变异自证（实测）：比较方向写反（增量不少于门槛即判）→ 首个窗口即判出，红；门槛按 KB 与字节数比较
// （增量右移 10 位再比）→ 判出，红。
func TestSlowGuardNotTriggeredWhenFast(t *testing.T) {
	body := randomBytes(t, testFileSize)
	srv, _ := servePaced(t, body, 0, 32<<10, 100*time.Millisecond)
	dest, _, err := download(t, testFetchParams, srv.URL, true, assetFor(body))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	assertFileEquals(t, dest, body)
}

// TestUnguardedSlowCompletes：最后一个来源不设低速判据——低于门槛但在整文件时限内发完即成功。服务器
// 以 40 KiB/s 发 64 KiB（约 1.6 s，超过一个低速窗口、低于门槛 80 KiB/s）。
//
// 变异自证（实测）：guarded 为假时也起低速判据 → 判出过慢，红。
func TestUnguardedSlowCompletes(t *testing.T) {
	body := randomBytes(t, 64<<10)
	srv, _ := servePaced(t, body, 0, 4<<10, 100*time.Millisecond)
	dest, _, err := download(t, testFetchParams, srv.URL, false, assetFor(body))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	assertFileEquals(t, dest, body)
}

// TestProgressEndLine：打印过进度之后 endLine 追加且只追加一个换行；从未打印过时不输出。receive 写盘
// 结束（成功或失败）时进度行已换行，随后的日志不接在进度行末尾。
//
// 变异自证（实测）：endLine 不判是否打印过、总是换行 → 连调两次出两个换行，红；换行后不清标记 → 同上，红；
// receive 里不调 endLine → 第三段红。
func TestProgressEndLine(t *testing.T) {
	var buf bytes.Buffer
	pw := &progressWriter{out: &buf, total: 100}
	pw.Write(make([]byte, 50))
	before := buf.Len()
	if before == 0 {
		t.Fatal("前提不成立：写过一半没有打印进度")
	}
	pw.endLine()
	pw.endLine()
	if got := buf.String()[before:]; got != "\n" {
		t.Fatalf("endLine 追加了 %q，应为一个换行", got)
	}

	var quiet bytes.Buffer
	pw = &progressWriter{out: &quiet} // total 为 0：不打印进度
	pw.Write(make([]byte, 50))
	pw.endLine()
	if quiet.Len() != 0 {
		t.Fatalf("从未打印过进度，endLine 却输出了 %q", quiet.String())
	}

	// receive 把进度打到 os.Stdout，这里换成临时文件读回。
	body := randomBytes(t, testFileSize)
	srv, _ := serveFile(t, body)
	wrong := assetFor(body)
	wrong.Digest = sha256Digest(nil)
	for name, asset := range map[string]updateAsset{"成功": assetFor(body), "SHA256 不符": wrong} {
		f, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if err != nil {
			t.Fatal(err)
		}
		orig := os.Stdout
		os.Stdout = f
		_, _, err = download(t, testFetchParams, srv.URL, false, asset)
		os.Stdout = orig
		f.Close()
		if (err == nil) != (name == "成功") {
			t.Fatalf("%s：receive 返回 %v", name, err)
		}
		out, _ := os.ReadFile(f.Name())
		if len(out) == 0 || out[len(out)-1] != '\n' {
			t.Fatalf("%s：receive 结束时进度行未换行，输出末尾为 %q", name, out[max(0, len(out)-20):])
		}
	}
}

// serverStats 记一个测试服务器收到的请求数与写出的响应体字节数。
type serverStats struct{ hits, sent atomic.Int64 }

type countingWriter struct {
	http.ResponseWriter
	sent *atomic.Int64
}

func (w countingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.sent.Add(int64(n))
	return n, err
}

func (w countingWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

// serveCounted 起一个以 h 应答的服务器，计请求数与写出的字节数。
func serveCounted(t *testing.T, h http.HandlerFunc) (*httptest.Server, *serverStats) {
	t.Helper()
	st := &serverStats{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.hits.Add(1)
		h(countingWriter{w, &st.sent}, r)
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

// serveSilent 起一个发出响应头后不发数据的服务器，请求被取消（至多 10 s）即返回。
func serveSilent(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// abortHandler 声明 body 全长，发出前 n 字节后即断开连接。
func abortHandler(body []byte, n int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body[:n])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
}

// serveAbort 起一个以 abortHandler 应答的服务器。
func serveAbort(t *testing.T, body []byte, n int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(abortHandler(body, n))
	t.Cleanup(srv.Close)
	return srv
}

// sourceOf 以测试服务器的地址作来源前缀；服务器不看路径。
func sourceOf(srv *httptest.Server) updateSource {
	return updateSource{Name: srv.URL, Prefix: srv.URL + "/"}
}

// refusedSource 是一个连不上的来源：端口刚释放、无人监听。
func refusedSource(t *testing.T) updateSource {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + ln.Addr().String()
	ln.Close()
	return updateSource{Name: addr, Prefix: addr + "/"}
}

// race 对 a、b 测速，返回结果与用时。测试结束时关闭胜者，且先于服务器关闭：服务器的 Close 要等应答返回。
func race(t *testing.T, p fetchParams, a, b updateSource) (raceResult, time.Duration) {
	t.Helper()
	start := time.Now()
	res := raceSources(&http.Client{}, p, []updateSource{a, b}, "v1", "a.exe")
	elapsed := time.Since(start)
	if res.winner != nil {
		t.Cleanup(res.winner.close)
	}
	return res, elapsed
}

func assertWinner(t *testing.T, res raceResult, want updateSource) {
	t.Helper()
	if res.winner == nil {
		t.Fatalf("无胜者，应为 %s；%s", want.Name, entriesString(res.entries))
	}
	if res.winner.src != want {
		t.Fatalf("胜者为 %s，应为 %s；%s", res.winner.src.Name, want.Name, entriesString(res.entries))
	}
}

// entriesString 供失败信息：字段未导出，%+v 打不出 err 的文本。
func entriesString(es []raceEntry) string {
	var b strings.Builder
	for _, e := range es {
		fmt.Fprintf(&b, "[%s：%d 字节，%v，%v]", e.src.Name, e.bytes, e.elapsed, e.err)
	}
	return b.String()
}

func assertPrefixOf(t *testing.T, prefix, body []byte) {
	t.Helper()
	if int64(len(prefix)) < testFetchParams.RaceCap || !bytes.Equal(prefix, body[:len(prefix)]) {
		t.Fatalf("prefix %d 字节，应不少于 %d 字节且与文件开头相同", len(prefix), testFetchParams.RaceCap)
	}
}

// TestRaceFastWins：快的一边先收满 RaceCap 即判定、胜出，不等窗口到期；prefix 是胜者按顺序收到的字节；
// 败者在判定后被取消，不记为判负。快的一边在后、在前各测一次。
//
// 变异自证（实测）：去掉「先收满或先读完即判定」→ 用时满 RaceWindow，红；胜者固定取 sources[0] →「快的在后」红；
// entries 的判负原因取读取 goroutine 最终的错误（含判定后被取消的）→ 败者有判负原因，红。
func TestRaceFastWins(t *testing.T) {
	body := randomBytes(t, testFileSize)
	for name, fastFirst := range map[string]bool{"快的在后": false, "快的在前": true} {
		t.Run(name, func(t *testing.T) {
			fast, _ := serveFile(t, body)
			win, slow := sourceOf(fast), sourceOf(serveSlow(t, body))
			a, b := slow, win
			if fastFirst {
				a, b = win, slow
			}
			res, elapsed := race(t, testFetchParams, a, b)
			assertWinner(t, res, win)
			assertPrefixOf(t, res.prefix, body)
			if elapsed >= testFetchParams.RaceWindow {
				t.Fatalf("用时 %v，收满即应判定，不应等到窗口 %v", elapsed, testFetchParams.RaceWindow)
			}
			for _, e := range res.entries {
				if e.err != nil {
					t.Fatalf("%s 记了判负原因 %v，两边都不应判负", e.src.Name, e.err)
				}
			}
		})
	}
}

// TestRaceFailedLoses：判负的一边不能胜出，另一边慢也胜；判负原因记在 entries。404、连接被拒、读到一半
// 断开三种；「读到一半断开」的一边判负前已收 32 KiB，多于慢的一边窗口内收到的约 4 KiB。
//
// 变异自证（实测）：判负者照样按字节参与比较 →「读到一半断开」红；open 失败不判负 →「404」「连接被拒」红。
func TestRaceFailedLoses(t *testing.T) {
	body := randomBytes(t, testFileSize)
	cases := map[string]func(t *testing.T) updateSource{
		"404": func(t *testing.T) updateSource {
			srv := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(srv.Close)
			return sourceOf(srv)
		},
		"连接被拒":   refusedSource,
		"读到一半断开": func(t *testing.T) updateSource { return sourceOf(serveAbort(t, body, 32<<10)) },
	}
	for name, failing := range cases {
		t.Run(name, func(t *testing.T) {
			slow := sourceOf(serveSlow(t, body))
			res, _ := race(t, testFetchParams, failing(t), slow)
			assertWinner(t, res, slow)
			if !isSourceError(res.entries[0].err) || res.entries[1].err != nil {
				t.Fatalf("entries 的判负原因为 %v、%v，应为来源失败、nil", res.entries[0].err, res.entries[1].err)
			}
		})
	}
}

// TestRaceBothFail：两边都判负 → 无胜者，两条 entries 都有判负原因；两边都结束即判定，不等窗口到期。
//
// 变异自证（实测）：去掉「两边都结束即判定」→ 用时满 RaceWindow，红；判负者照样参与比较 → 有胜者，红。
func TestRaceBothFail(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	res, elapsed := race(t, testFetchParams, sourceOf(srv), sourceOf(srv))
	if res.winner != nil {
		t.Fatalf("胜者为 %s，应无胜者", res.winner.src.Name)
	}
	for _, e := range res.entries {
		if !isSourceError(e.err) {
			t.Fatalf("%s 的判负原因为 %v，应为来源失败", e.src.Name, e.err)
		}
	}
	if elapsed >= testFetchParams.RaceWindow {
		t.Fatalf("用时 %v，两边都结束即应判定，不应等到窗口 %v", elapsed, testFetchParams.RaceWindow)
	}
}

// TestRaceSmallFileDone：文件小于 RaceCap，在窗口内读完的一边不判负、胜出，done 为真，prefix 即整个文件。
//
// 变异自证（实测）：读到 EOF 记为判负 → 慢的一边胜出，红；读到 EOF 不置 done → 红。
func TestRaceSmallFileDone(t *testing.T) {
	small := randomBytes(t, 8<<10)
	fast, _ := serveFile(t, small)
	win := sourceOf(fast)
	res, _ := race(t, testFetchParams, sourceOf(serveSlow(t, small)), win)
	assertWinner(t, res, win)
	if !res.done || !bytes.Equal(res.prefix, small) {
		t.Fatalf("done = %v、prefix %d 字节，应为 true 与整个文件（%d 字节）", res.done, len(res.prefix), len(small))
	}
}

// TestRaceFirstDoneWins：先读完整个响应体与先收满同等对待，立即胜出，不等另一边也读完再按字节平局判给
// sources[0]。两例中 sources[1] 快，sources[0] 较慢、带 Content-Length、约 200 ms 读完（在窗口内）：
//   - 「恰好 RaceCap」：文件恰好 RaceCap 字节。带 Content-Length 的响应体读尽时，最后一次读取连同数据返回
//     io.EOF（net/http/transfer.go），读取 goroutine 以读完而非收满退出；done 为真即证明构造的是这种情形。
//   - 「8 KiB」：文件小于 RaceCap。
//
// 判定时 sources[0] 尚未读完：它在 entries 里未判负，字节数小于文件大小。
//
// 变异自证（实测）：只把收满算作先完成（条件加回 !eof）→ 两例都等到 sources[0] 读完，字节相同判给 sources[0]，红。
func TestRaceFirstDoneWins(t *testing.T) {
	cases := map[string]struct{ size, chunk int }{
		"恰好 RaceCap": {int(testFetchParams.RaceCap), 16 << 10},
		"8 KiB":      {8 << 10, 2 << 10},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			body := randomBytes(t, c.size)
			fast, _ := serveFile(t, body)
			paced := pacedHandler(body, 0, c.chunk, 50*time.Millisecond, nil)
			slower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				paced(w, r)
			}))
			t.Cleanup(slower.Close)
			win := sourceOf(fast)
			res, _ := race(t, testFetchParams, sourceOf(slower), win)
			assertWinner(t, res, win)
			if !res.done || !bytes.Equal(res.prefix, body) {
				t.Fatalf("done = %v、prefix %d 字节，应为 true 与整个文件（%d 字节）", res.done, len(res.prefix), len(body))
			}
			if e := res.entries[0]; e.err != nil || e.bytes >= int64(len(body)) {
				t.Fatalf("判定时 sources[0] 应仍在读、未读完：%s", entriesString(res.entries))
			}
		})
	}
}

// TestRaceTieGoesFirst：两边都在响应头之后不发数据 → 窗口到期判定（entries 的用时约为 RaceWindow），
// 字节数相同（都为 0）取 sources[0]。胜者此时正阻塞在一次读取中：交接等这次读取返回，这里由低速判据
// 结束，result.err 为来源失败。
//
// 变异自证（实测）：平局取 sources[1] → 红；不等胜者的读取 goroutine 退出就返回 → result.err 为 nil，红。
func TestRaceTieGoesFirst(t *testing.T) {
	a, b := sourceOf(serveSilent(t)), sourceOf(serveSilent(t))
	res, _ := race(t, testFetchParams, a, b)
	assertWinner(t, res, a)
	lo, hi := testFetchParams.RaceWindow, testFetchParams.RaceWindow+500*time.Millisecond
	for _, e := range res.entries {
		if e.err != nil || e.bytes != 0 || e.elapsed < lo || e.elapsed > hi {
			t.Fatalf("%s，应都未判负、0 字节、用时在 [%v, %v]", entriesString(res.entries), lo, hi)
		}
	}
	if !isSourceError(res.err) || !errMatchesKey(res.err, "too slow: %d KB in the last %d s") {
		t.Fatalf("result.err = %v，应为胜者那次读取被低速判据结束的来源失败", res.err)
	}
}

// TestRaceSilentVsSlow：sources[0] 连上但不发数据、sources[1] 慢但有数据 → sources[1] 胜：比的是收到的字节。
//
// 变异自证（实测）：比较方向写反（取字节最少者）→ 不发数据的一边胜出，红。
func TestRaceSilentVsSlow(t *testing.T) {
	body := randomBytes(t, testFileSize)
	slow := sourceOf(serveSlow(t, body))
	res, _ := race(t, testFetchParams, sourceOf(serveSilent(t)), slow)
	assertWinner(t, res, slow)
}

// TestRaceLoserCanceled：判定后败者的请求立即取消，服务器看到请求的 context 结束。低速窗口放大到 10 s：
// 取消只能来自判定，不会来自败者自己的低速判据。
//
// 变异自证（实测）：败者不取消（判定后既不 cancel 也不 close）→ 1 s 内服务器未看到取消，红。
func TestRaceLoserCanceled(t *testing.T) {
	body := randomBytes(t, testFileSize)
	p := testFetchParams
	p.SlowWindow = 10 * time.Second
	canceled := make(chan struct{})
	paced := pacedHandler(body, 0, 1<<10, 100*time.Millisecond, nil)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paced(w, r)
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(slow.Close)
	fast, _ := serveFile(t, body)
	res, _ := race(t, p, sourceOf(slow), sourceOf(fast))
	assertWinner(t, res, sourceOf(fast))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("判定后 1 s 内败者的服务器没有看到请求被取消")
	}
}

// TestRaceWinnerContinues：胜者的连接接续作正式下载：receive(winner, prefix, done) 写出的文件与原文件相同，
// 胜者服务器只收到 1 次请求、发出的字节数等于文件大小。胜者先一次发出 RaceCap，余下每 100 ms 发 32 KiB
// （320 KiB/s，低速门槛的 4 倍），约 0.6 s 发完，跨过 RaceWindow。
//
// 变异自证（实测）：测速请求带上 RaceWindow 时限 → 余下部分读到一半请求被取消，receive 失败，红；判定后
// 胜者也取消 → receive 失败，红。
func TestRaceWinnerContinues(t *testing.T) {
	body := randomBytes(t, testFileSize)
	fast, st := serveCounted(t, pacedHandler(body, int(testFetchParams.RaceCap), 32<<10, 100*time.Millisecond, nil))
	win := sourceOf(fast)
	res, _ := race(t, testFetchParams, sourceOf(serveSlow(t, body)), win)
	assertWinner(t, res, win)
	if res.err != nil {
		t.Fatalf("result.err = %v", res.err)
	}
	dest := filepath.Join(t.TempDir(), "a.new")
	if err := receive(res.winner, res.prefix, res.done, assetFor(body), dest); err != nil {
		t.Fatalf("receive: %v", err)
	}
	assertFileEquals(t, dest, body)
	if hits, sent := st.hits.Load(), st.sent.Load(); hits != 1 || sent != int64(len(body)) {
		t.Fatalf("胜者服务器收到 %d 次请求、发出 %d 字节，应为 1 次、%d 字节", hits, sent, len(body))
	}
}

// TestRaceWinnerReadErrorAfterDecision：胜者发出 RaceCap 字节后即断开。交接前读到断开时 result.err 为来源
// 失败（winner 已关闭）；交接后读到时 receive 返回来源失败。两种时序都允许，断言其一成立。网关不下发大小
// 与 SHA256：截断只能由读取错误发现。
//
// 变异自证（实测）：收满也置 done（当作读完）→ receive 不再读、返回 nil，红。
func TestRaceWinnerReadErrorAfterDecision(t *testing.T) {
	body := randomBytes(t, testFileSize)
	win := sourceOf(serveAbort(t, body, int(testFetchParams.RaceCap)))
	res, _ := race(t, testFetchParams, sourceOf(serveSlow(t, body)), win)
	assertWinner(t, res, win)
	err := res.err
	if err == nil {
		err = receive(res.winner, res.prefix, res.done, updateAsset{Name: "a.exe"}, filepath.Join(t.TempDir(), "a.new"))
	}
	if !isSourceError(err) {
		t.Fatalf("result.err 与 receive 返回 %v，应为来源失败", err)
	}
}

// TestRaceNoGoroutineLeak：一次测速与 receive 之后关闭空闲连接，2 s 内 goroutine 数回到基线（服务器起好
// 之后取）。低速窗口放大到 10 s：败者只能由判定关闭，不会由它自己的低速判据结束。
//
// 变异自证（实测）：败者不关闭（判定后既不 cancel 也不 close）→ 败者的低速判据与连接 goroutine 留到整文件
// 时限，红。
func TestRaceNoGoroutineLeak(t *testing.T) {
	body := randomBytes(t, testFileSize)
	p := testFetchParams
	p.SlowWindow = 10 * time.Second
	fast, _ := serveFile(t, body)
	slow := serveSlow(t, body)
	client := &http.Client{}
	base := runtime.NumGoroutine()

	res := raceSources(client, p, []updateSource{sourceOf(slow), sourceOf(fast)}, "v1", "a.exe")
	assertWinner(t, res, sourceOf(fast))
	if res.err != nil {
		t.Fatalf("result.err = %v", res.err)
	}
	if err := receive(res.winner, res.prefix, res.done, assetFor(body), filepath.Join(t.TempDir(), "a.new")); err != nil {
		t.Fatalf("receive: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		client.CloseIdleConnections() // 胜者的连接读完后才回到空闲池，每轮都关
		n := runtime.NumGoroutine()
		if n <= base {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("2 s 后仍有 %d 个 goroutine，基线 %d：\n%s", n, base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRaceFollowsRedirect：sources[1] 先 302 跳到另一台服务器上的快服务器，测速跟随跳转，sources[1] 胜出。
//
// 变异自证（实测）：open 改用 http.DefaultTransport.RoundTrip（不跟随跳转）→ sources[1] 判负「HTTP 302」，红。
func TestRaceFollowsRedirect(t *testing.T) {
	body := randomBytes(t, testFileSize)
	fast, _ := serveFile(t, body)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fast.URL+"/asset", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	via := sourceOf(redirect)
	res, _ := race(t, testFetchParams, sourceOf(serveSlow(t, body)), via)
	assertWinner(t, res, via)
	assertPrefixOf(t, res.prefix, body)
}

// assetServer 是 fetchAssets 用例里的一个来源：按请求路径的最后一段（文件名）应答，逐文件计请求数。
type assetServer struct {
	src  updateSource
	mu   sync.Mutex
	hits map[string]int
}

func (s *assetServer) hitsOf(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[name]
}

func (s *assetServer) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, h := range s.hits {
		n += h
	}
	return n
}

// serveAssets 起一个来源：files 里的文件第 n 次（从 1 起）被请求时以 pick(name, n) 应答；pick 为 nil 或返回 nil 时
// 一次发出全部内容（带 Content-Length）。不在 files 里的返回 404。
func serveAssets(t *testing.T, files map[string][]byte, pick func(name string, n int) http.HandlerFunc) *assetServer {
	t.Helper()
	s := &assetServer{hits: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		s.mu.Lock()
		s.hits[name]++
		n := s.hits[name]
		s.mu.Unlock()
		body, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if pick != nil {
			if h := pick(name, n); h != nil {
				h(w, r)
				return
			}
		}
		sendAll(body)(w, r)
	}))
	t.Cleanup(srv.Close)
	s.src = sourceOf(srv)
	return s
}

// sendAll 一次发出 body，带 Content-Length。
func sendAll(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}
}

// slowHandler 是「慢」：每 100 ms 发 1 KiB。
func slowHandler(body []byte) http.HandlerFunc {
	return pacedHandler(body, 0, 1<<10, 100*time.Millisecond, nil)
}

// slowInRace 让 name 的第 1 次请求（测速）慢，其余照常。
func slowInRace(files map[string][]byte, name string) func(string, int) http.HandlerFunc {
	return func(n string, i int) http.HandlerFunc {
		if n == name && i == 1 {
			return slowHandler(files[n])
		}
		return nil
	}
}

// failOn 让 name 的每次请求都返回 404，其余照常。
func failOn(name string) func(string, int) http.HandlerFunc {
	return func(n string, _ int) http.HandlerFunc {
		if n == name {
			return http.NotFound
		}
		return nil
	}
}

// assetNames 是用例里附件的名字，首个为 exe，同 updateIfNeeded 排序后的形状。
var assetNames = []string{"PlayerCap.exe", "README.html", "PlayerCap.zip"}

// makeAssets 按 sizes 生成附件（名字依次取 assetNames，带大小与 SHA256）与各自的随机内容。
func makeAssets(t *testing.T, sizes ...int) ([]updateAsset, map[string][]byte) {
	t.Helper()
	assets := make([]updateAsset, len(sizes))
	files := map[string][]byte{}
	for i, n := range sizes {
		body := randomBytes(t, n)
		assets[i] = assetFor(body)
		assets[i].Name = assetNames[i]
		files[assetNames[i]] = body
	}
	return assets, files
}

// fetch 以 testFetchParams 对 sources 跑一遍 fetchAssets，写进新的临时目录。
func fetch(t *testing.T, assets []updateAsset, sources ...updateSource) (string, []string, []updateSource, error) {
	t.Helper()
	dir := t.TempDir()
	tmp, tried, err := fetchAssets(&http.Client{}, testFetchParams, sources, "v1", assets, dir)
	return dir, tmp, tried, err
}

// assertFetched 断言 tmp 依次是 dir 下各附件的 .new，内容与 files 相同。
func assertFetched(t *testing.T, dir string, tmp []string, assets []updateAsset, files map[string][]byte) {
	t.Helper()
	if len(tmp) != len(assets) {
		t.Fatalf("返回 %d 个 .new，应为 %d 个：%v", len(tmp), len(assets), tmp)
	}
	for i, a := range assets {
		if want := filepath.Join(dir, a.Name+".new"); tmp[i] != want {
			t.Fatalf("第 %d 个为 %s，应为 %s", i+1, tmp[i], want)
		}
		assertFileEquals(t, tmp[i], files[a.Name])
	}
}

func assertTried(t *testing.T, got []updateSource, want ...updateSource) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tried = %+v，应为 %+v", got, want)
	}
}

// assertFailedFile 断言 err 是「下载 name 失败：…」，文本含 name，且以 %w 包住的错误是 inner 这条文案。
func assertFailedFile(t *testing.T, err error, name, inner string) {
	t.Helper()
	if !errMatchesKey(err, "failed to download %s: %w") || !strings.Contains(err.Error(), name) ||
		!errMatchesKey(errors.Unwrap(err), inner) {
		t.Fatalf("fetchAssets 返回 %v，应为「下载 %s 失败」且包住「%s」", err, name, i18n.T(inner))
	}
}

// assertDirEmpty 断言 dir 下什么都没有：.new 已全部删除，也没有放置任何附件。
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) > 0 {
		t.Fatalf("%s 下应为空，实有 %v", dir, names)
	}
}

// TestFetchRaceThenRest：两个来源，sources[1] 快 → 第 1 个文件由测速胜者的连接接续下完、不重新请求，其余文件
// 都向 sources[1] 请求；sources[0] 只收到测速那 1 次请求。返回的 .new 依次对应各附件、内容正确；tried 只有
// sources[1]（测速败者不计）。
//
// 变异自证（实测）：胜者不接续、改为向它重新请求第 1 个文件 → sources[1] 对它收到 2 次请求，红；有胜者时
// 当前来源仍取 sources[0] → 红；tried 计入测速败者 → 红。
func TestFetchRaceThenRest(t *testing.T) {
	assets, files := makeAssets(t, testFileSize, 32<<10, 16<<10)
	s0 := serveAssets(t, files, slowInRace(files, assets[0].Name))
	s1 := serveAssets(t, files, nil)
	dir, tmp, tried, err := fetch(t, assets, s0.src, s1.src)
	if err != nil {
		t.Fatalf("fetchAssets: %v", err)
	}
	assertFetched(t, dir, tmp, assets, files)
	assertTried(t, tried, s1.src)
	if n := s0.total(); n != 1 {
		t.Fatalf("sources[0] 收到 %d 次请求，应只有测速那 1 次", n)
	}
	for _, a := range assets {
		if n := s1.hitsOf(a.Name); n != 1 {
			t.Fatalf("sources[1] 对 %s 收到 %d 次请求，应为 1 次", a.Name, n)
		}
	}
}

// TestFetchSwitchMidFile：测速胜者 sources[1] 在第 1 个文件中途断开 → 换到 sources[0] 从头重下第 1 个文件（它对
// 该文件收到第 2 次请求，不带 Range，写出的文件逐字节相同），余下文件都走 sources[0]；tried 依次为两个来源。
// 胜者先发出 128 KiB（多于 RaceCap，测速判定为收满）再断开。
//
// 变异自证（实测）：来源失败不换源、直接返回错误 → 红；换源只作用于当前文件、余下文件仍向原来源请求 → 红。
func TestFetchSwitchMidFile(t *testing.T) {
	assets, files := makeAssets(t, testFileSize, 32<<10, 16<<10)
	first := assets[0].Name
	ranges := make(chan string, 1)
	s0 := serveAssets(t, files, func(name string, n int) http.HandlerFunc {
		if name != first {
			return nil
		}
		if n == 1 {
			return slowHandler(files[name])
		}
		return func(w http.ResponseWriter, r *http.Request) {
			select {
			case ranges <- r.Header.Get("Range"):
			default:
			}
			sendAll(files[name])(w, r)
		}
	})
	s1 := serveAssets(t, files, func(name string, _ int) http.HandlerFunc {
		if name == first {
			return abortHandler(files[name], 128<<10)
		}
		return nil
	})
	dir, tmp, tried, err := fetch(t, assets, s0.src, s1.src)
	if err != nil {
		t.Fatalf("fetchAssets: %v", err)
	}
	assertFetched(t, dir, tmp, assets, files)
	assertTried(t, tried, s1.src, s0.src)
	if n := s0.hitsOf(first); n != 2 {
		t.Fatalf("sources[0] 对第 1 个文件收到 %d 次请求，应为测速与重下各 1 次", n)
	}
	if r := <-ranges; r != "" {
		t.Fatalf("重下带了 Range: %q，应从字节 0 起", r)
	}
	for _, a := range assets[1:] {
		if n0, n1 := s0.hitsOf(a.Name), s1.hitsOf(a.Name); n0 != 1 || n1 != 0 {
			t.Fatalf("%s：sources[0] 收到 %d 次、sources[1] 收到 %d 次，应为 1 次、0 次", a.Name, n0, n1)
		}
	}
}

// TestFetchSwitchOnSecondFile：第 2 个文件在当前来源 404 → 换源重下它；第 1 个文件已下完，不重下（新来源对它只有
// 测速那 1 次请求）。
//
// 变异自证（实测）：换源后从第 1 个文件重来 → sources[0] 对第 1 个文件收到 2 次请求，红。
func TestFetchSwitchOnSecondFile(t *testing.T) {
	assets, files := makeAssets(t, testFileSize, 32<<10, 16<<10)
	s0 := serveAssets(t, files, slowInRace(files, assets[0].Name))
	s1 := serveAssets(t, files, failOn(assets[1].Name))
	dir, tmp, tried, err := fetch(t, assets, s0.src, s1.src)
	if err != nil {
		t.Fatalf("fetchAssets: %v", err)
	}
	assertFetched(t, dir, tmp, assets, files)
	assertTried(t, tried, s1.src, s0.src)
	if n := s0.hitsOf(assets[0].Name); n != 1 {
		t.Fatalf("sources[0] 对第 1 个文件收到 %d 次请求，应只有测速那 1 次", n)
	}
	if n0, n1 := s0.hitsOf(assets[2].Name), s1.hitsOf(assets[2].Name); n0 != 1 || n1 != 0 {
		t.Fatalf("第 3 个文件：sources[0] 收到 %d 次、sources[1] 收到 %d 次，应为 1 次、0 次", n0, n1)
	}
}

// TestFetchSwitchOnlyOnce：第 2 个文件在 sources[1] 404、换到 sources[0]，第 3 个文件又在 sources[0] 404 → 不再换，
// 返回「下载 <第 3 个文件> 失败」，包住的仍是来源失败「HTTP 404」；tried 为两个来源；已下完的两个 .new 都删除；
// sources[1] 没有收到第 3 个文件的请求。
//
// 变异自证（实测）：换过之后不清备用来源（可来回换）→ 第 3 个文件换回 sources[1] 下完、返回 nil，红；失败时
// 不删已下完的 .new → 红；包文件名改用 %v → 错误链断开、不再是来源失败，红。
func TestFetchSwitchOnlyOnce(t *testing.T) {
	assets, files := makeAssets(t, testFileSize, 32<<10, 16<<10)
	s0 := serveAssets(t, files, func(name string, n int) http.HandlerFunc {
		switch {
		case name == assets[0].Name && n == 1:
			return slowHandler(files[name])
		case name == assets[2].Name:
			return http.NotFound
		}
		return nil
	})
	s1 := serveAssets(t, files, failOn(assets[1].Name))
	dir, tmp, tried, err := fetch(t, assets, s0.src, s1.src)
	if !isSourceError(err) {
		t.Fatalf("fetchAssets 返回 %v，应仍为来源失败", err)
	}
	assertFailedFile(t, err, assets[2].Name, "HTTP %d")
	if tmp != nil {
		t.Fatalf("失败时返回了 %v", tmp)
	}
	assertTried(t, tried, s1.src, s0.src)
	for _, a := range assets {
		assertRemoved(t, filepath.Join(dir, a.Name+".new"))
	}
	if n := s1.hitsOf(assets[2].Name); n != 0 {
		t.Fatalf("sources[1] 收到 %d 次第 3 个文件的请求，换过一次之后不应再换", n)
	}
}

// TestFetchLastSourceUnguarded：当前来源（测速胜者 sources[1]）在第 2 个文件上每 100 ms 只发 1 KiB，约一个低速窗口
// 后判慢、换源；新来源 sources[0] 以 40 KiB/s 发完 64 KiB（约 1.6 s，超过一个低速窗口、低于门槛 80 KiB/s，在时限
// 内）→ 成功：换源后的来源是最后一个，不设低速判据。另断言判慢在 SlowWindow + 500 ms 内换源：换源前的来源设了判据。
//
// 变异自证（实测）：换源后仍设低速判据 → 新来源也判慢、返回来源失败，红；当前来源也不设低速判据 → 要等整文件
// 时限 5 s 才换源，红。
func TestFetchLastSourceUnguarded(t *testing.T) {
	assets, files := makeAssets(t, testFileSize, 64<<10)
	first, second := assets[0].Name, assets[1].Name
	var mu sync.Mutex
	var slowAt, switchAt time.Time
	s0 := serveAssets(t, files, func(name string, n int) http.HandlerFunc {
		switch {
		case name == first && n == 1:
			return slowHandler(files[name])
		case name == second:
			mu.Lock()
			switchAt = time.Now()
			mu.Unlock()
			return pacedHandler(files[name], 0, 4<<10, 100*time.Millisecond, nil)
		}
		return nil
	})
	s1 := serveAssets(t, files, func(name string, _ int) http.HandlerFunc {
		if name != second {
			return nil
		}
		mu.Lock()
		slowAt = time.Now()
		mu.Unlock()
		return slowHandler(files[name])
	})
	dir, tmp, tried, err := fetch(t, assets, s0.src, s1.src)
	if err != nil {
		t.Fatalf("fetchAssets: %v", err)
	}
	assertFetched(t, dir, tmp, assets, files)
	assertTried(t, tried, s1.src, s0.src)
	mu.Lock()
	gap := switchAt.Sub(slowAt)
	mu.Unlock()
	if limit := testFetchParams.SlowWindow + 500*time.Millisecond; gap > limit {
		t.Fatalf("当前来源开始发慢之后 %v 才换源，应不超过 %v", gap, limit)
	}
}

// TestFetchNoWinnerOrder：测速时两边都 404（无胜者），之后两边恢复 → 先向 sources[0] 请求、成功；tried 只有
// sources[0]（测速败者不计）；sources[1] 只收到测速那 1 次请求。
//
// 变异自证（实测）：无胜者时先试 sources[1] → 红；tried 计入测速败者 → 红。
func TestFetchNoWinnerOrder(t *testing.T) {
	assets, files := makeAssets(t, testFileSize, 32<<10)
	first := assets[0].Name
	notFoundInRace := func(name string, n int) http.HandlerFunc {
		if name == first && n == 1 {
			return http.NotFound
		}
		return nil
	}
	s0 := serveAssets(t, files, notFoundInRace)
	s1 := serveAssets(t, files, notFoundInRace)
	dir, tmp, tried, err := fetch(t, assets, s0.src, s1.src)
	if err != nil {
		t.Fatalf("fetchAssets: %v", err)
	}
	assertFetched(t, dir, tmp, assets, files)
	assertTried(t, tried, s0.src)
	if n := s1.total(); n != 1 {
		t.Fatalf("sources[1] 收到 %d 次请求，应只有测速那 1 次", n)
	}
}

// TestFetchSingleSource：只有一个来源 → 不测速，每个文件各请求 1 次；它是最后一个来源，不设低速判据：第 1 个文件
// 以 40 KiB/s 发完 64 KiB（超过一个低速窗口、低于门槛）照样成功。
//
// 变异自证（实测）：一个来源也设低速判据 → 判慢、返回来源失败，红；一个来源也测速（raceSources 只传它一个，
// 测速请求设低速判据）→ 红。
func TestFetchSingleSource(t *testing.T) {
	assets, files := makeAssets(t, 64<<10, 16<<10)
	s0 := serveAssets(t, files, func(name string, _ int) http.HandlerFunc {
		if name == assets[0].Name {
			return pacedHandler(files[name], 0, 4<<10, 100*time.Millisecond, nil)
		}
		return nil
	})
	dir, tmp, tried, err := fetch(t, assets, s0.src)
	if err != nil {
		t.Fatalf("fetchAssets: %v", err)
	}
	assertFetched(t, dir, tmp, assets, files)
	assertTried(t, tried, s0.src)
	for _, a := range assets {
		if n := s0.hitsOf(a.Name); n != 1 {
			t.Fatalf("%s 收到 %d 次请求，应为 1 次", a.Name, n)
		}
	}
}

// TestFetchDigestMismatchNoSwitch：当前来源发出的第 2 个文件大小对、SHA256 不符 → 返回「下载 <第 2 个文件> 失败」，
// 包住的是 SHA256 不符、不是来源失败，不换源（另一来源没有收到该文件的请求）；已下完的 .new 删除。
//
// 变异自证（实测）：任何错误都换源（去掉 isSourceError 的判断）→ 换到 sources[0] 下完、返回 nil，红；包文件名
// 改用 %v → 解不出包住的 SHA256 不符，红。
func TestFetchDigestMismatchNoSwitch(t *testing.T) {
	assets, files := makeAssets(t, testFileSize, 32<<10)
	second := assets[1].Name
	wrong := randomBytes(t, len(files[second]))
	s0 := serveAssets(t, files, slowInRace(files, assets[0].Name))
	s1 := serveAssets(t, files, func(name string, _ int) http.HandlerFunc {
		if name == second {
			return sendAll(wrong)
		}
		return nil
	})
	dir, tmp, tried, err := fetch(t, assets, s0.src, s1.src)
	assertFailedFile(t, err, second, "SHA256 verification failed: expected %s, got %s")
	if isSourceError(err) {
		t.Fatalf("fetchAssets 返回 %v，SHA256 不符不应是来源失败", err)
	}
	if tmp != nil {
		t.Fatalf("失败时返回了 %v", tmp)
	}
	assertTried(t, tried, s1.src)
	if n := s0.hitsOf(second); n != 0 {
		t.Fatalf("sources[0] 收到 %d 次第 2 个文件的请求，SHA256 不符不应换源", n)
	}
	assertDirEmpty(t, dir)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestFetchEmptySources：没有来源 → 返回错误，不发请求，不建文件。
//
// 变异自证（实测）：去掉没有来源的判断 → 取 sources[0] 越界 panic，红。
func TestFetchEmptySources(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("不应发请求：%s", r.URL)
		return nil, errors.New("unexpected request")
	})}
	assets, _ := makeAssets(t, 16<<10)
	dir := t.TempDir()
	tmp, tried, err := fetchAssets(client, testFetchParams, nil, "v1", assets, dir)
	if !errMatchesKey(err, "no download source provided") || tmp != nil || tried != nil {
		t.Fatalf("fetchAssets 返回 %v、%v、%v，应为「网关未下发下载地址」、nil、nil", tmp, tried, err)
	}
	assertDirEmpty(t, dir)
}

// TestFetchAllOrNothing（#214）：最后一个附件在两个来源都失败 → 返回错误；目录里什么都没有：前两个文件的 .new 已
// 删除，也没有任何附件被放置（fetchAssets 只下载、不放置）。
//
// 变异自证（实测）：两段改回逐个放置（fetchAssets 每下完一个即把 .new 改名为目标文件）→ 目录里留下前两个附件，红。
func TestFetchAllOrNothing(t *testing.T) {
	assets, files := makeAssets(t, testFileSize, 32<<10, 16<<10)
	last := assets[2].Name
	s0 := serveAssets(t, files, func(name string, n int) http.HandlerFunc {
		switch {
		case name == assets[0].Name && n == 1:
			return slowHandler(files[name])
		case name == last:
			return http.NotFound
		}
		return nil
	})
	s1 := serveAssets(t, files, failOn(last))
	dir, tmp, _, err := fetch(t, assets, s0.src, s1.src)
	if err == nil || tmp != nil {
		t.Fatalf("fetchAssets 返回 %v、%v，应失败且不返回路径", tmp, err)
	}
	assertDirEmpty(t, dir)
}

// TestUpdateHTTPClientUsesDefaultTransport：下载用的 client 不替换 Transport——http.DefaultTransport 读
// HTTP_PROXY / HTTPS_PROXY，经代理上网的机器照常更新；不设 Timeout，时限由 transfer 的 context 管（设计 §4.3）。
//
// 变异自证（实测）：Transport 换成 &http.Transport{} → 红；设 Timeout 为 5 分钟 → 红。
func TestUpdateHTTPClientUsesDefaultTransport(t *testing.T) {
	c := updateHTTPClient()
	if c.Transport != nil || c.Timeout != 0 {
		t.Fatalf("Transport = %v、Timeout = %v，应为 nil、0", c.Transport, c.Timeout)
	}
}
