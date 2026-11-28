package main

// 自动更新的下载层（issue #213、#214，设计见 doc/design/update-source-race-design.md）：两个来源同时测速，
// 从一个来源取得一个附件，写成 .new 并核对，失败或过慢时换源，全部附件下完才交给放置。与替换 exe 分开，
// 测试以 httptest 服务器与临时目录驱动。
//
// 错误分两类，决定能不能换源（设计 §4.4）：
//   - 来源失败（*sourceError）：请求出错、状态码非 200、读响应体出错或中断、整文件时限到期、低速判据
//     触发、收到的字节数与网关下发的大小不等。换一个来源可能解决。
//   - 其余：SHA256 不符（AGENTS.md §3.5，删除文件、终止）与本地错误（建、写、Sync、Close）。换源无济于事。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/taskbar"
	"Metabox-Nexus-PlayerCap/telemetry"
)

// updateAsset 是网关下发的一个附件。Size 为 0、Digest 为空时不做相应核对。
type updateAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// updateSource 是一个下载前缀。
type updateSource struct {
	Name   string // 日志里的称呼：前缀 URL 的主机名，解析不出时为前缀本身
	Prefix string
}

func (s updateSource) url(tag, file string) string { return s.Prefix + tag + "/" + file }

// updateSources 由网关下发的两个前缀构成候选来源：国内在前、全球在后（平局与测不出时取国内，与此前
// 「测不出就用国内前缀」一致）；空串剔除；两者相同只留国内那一个。
//
// 日志以主机名指称来源，不写死「GitHub」「国内镜像」：网关改了前缀，日志仍然准确。
func updateSources(globalPrefix, chinaPrefix string) []updateSource {
	var out []updateSource
	for _, p := range []string{chinaPrefix, globalPrefix} {
		if p == "" || (len(out) > 0 && out[0].Prefix == p) {
			continue
		}
		name := p
		if u, err := url.Parse(p); err == nil && u.Hostname() != "" {
			name = u.Hostname()
		}
		out = append(out, updateSource{Name: name, Prefix: p})
	}
	return out
}

// fetchParams 是测速与下载的时限与门槛（设计 §4.2、§4.3），集中在一处，测试以小值运行。
type fetchParams struct {
	RaceWindow, SlowWindow, Sample, FileTimeout time.Duration
	RaceCap, SlowMin                            int64
}

// updateFetchParams 是生产参数：测速 5 s 或 1 MiB；任意 15 s 收到不足 750 KiB（50 KiB/s）即判慢，
// 每秒采样；整文件 5 分钟。
var updateFetchParams = fetchParams{
	RaceWindow:  5 * time.Second,
	RaceCap:     1 << 20,
	SlowWindow:  15 * time.Second,
	SlowMin:     750 << 10,
	Sample:      time.Second,
	FileTimeout: 5 * time.Minute,
}

// sourceError 标记来源失败（设计 §4.4）：可以换一个来源重下。
type sourceError struct{ err error }

func (e *sourceError) Error() string { return e.err.Error() }
func (e *sourceError) Unwrap() error { return e.err }

func isSourceError(err error) bool {
	var se *sourceError
	return errors.As(err, &se)
}

// slowError、sizeMismatchError、sha256MismatchError 仅作内部错误分类标签，
// 供 update_transfer 产出稳定短枚举，不改变对外错误文案与可换源判定。
type slowError struct{ err error }

func (e *slowError) Error() string { return e.err.Error() }
func (e *slowError) Unwrap() error { return e.err }

type sizeMismatchError struct{ err error }

func (e *sizeMismatchError) Error() string { return e.err.Error() }
func (e *sizeMismatchError) Unwrap() error { return e.err }

type sha256MismatchError struct{ err error }

func (e *sha256MismatchError) Error() string { return e.err.Error() }
func (e *sha256MismatchError) Unwrap() error { return e.err }

func isSlowError(err error) bool {
	var se *slowError
	return errors.As(err, &se)
}

// fetchStats 记录单轮 fetchAssets 的双源测速、切线与低速观测指标。
type fetchStats struct {
	RaceWinner     string // "gitee" | "github" | "single"
	ChinaKBps      int64
	GlobalKBps     int64
	SwitchedSource bool
	SlowTriggered  bool
}

func raceEntryKBps(e raceEntry) int64 {
	if e.err != nil || e.bytes <= 0 || e.elapsed <= 0 {
		return 0
	}
	return int64(float64(e.bytes) / 1024.0 / e.elapsed.Seconds())
}

func classifyRaceWinner(src updateSource, isSecondSource bool) string {
	lower := strings.ToLower(src.Name)
	switch {
	case strings.Contains(lower, "gitee"):
		return "gitee"
	case strings.Contains(lower, "github"):
		return "github"
	case isSecondSource:
		return "github"
	default:
		return "gitee"
	}
}

func classifyUpdateOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	var pe *placeError
	if errors.As(err, &pe) {
		return "place_err"
	}
	var shaErr *sha256MismatchError
	if errors.As(err, &shaErr) {
		return "sha256_mismatch"
	}
	var sizeErr *sizeMismatchError
	if errors.As(err, &sizeErr) {
		return "size_mismatch"
	}
	return "source_err"
}

// transfer 是对一个来源的一次 GET。时限与低速判据都挂在请求的 context 上：取消它，阻塞中的
// 连接、读取都随之返回。
//
// 时限不用 http.Client.Timeout：与低速判据同在一个 context，取消原因（时限或过慢）可经 context.Cause
// 区分；测速胜者沿用测速时的请求作正式下载，时限自测速发起时算起（设计 §4.3）。
type transfer struct {
	src   updateSource
	url   string
	start time.Time
	n     atomic.Int64 // 已收响应体字节

	ctx       context.Context
	cancel    context.CancelCauseFunc // 外层：低速判据与 close 取消
	stopTimer context.CancelFunc      // 内层：整文件时限
	body      io.ReadCloser
	length    int64         // 响应头的 Content-Length，未知为 -1
	watching  chan struct{} // 低速判据 goroutine 退出时关闭；不设判据时为 nil
}

// newTransfer 记发起时刻、建立时限为 p.FileTimeout 的 context；guarded 为真时起低速判据 goroutine。
// 不发请求。用完须 close（receive 会关）。
func newTransfer(p fetchParams, src updateSource, url string, guarded bool) *transfer {
	t := &transfer{src: src, url: url, start: time.Now(), length: -1}
	base, cancel := context.WithCancelCause(context.Background())
	t.ctx, t.stopTimer = context.WithTimeoutCause(base, p.FileTimeout,
		i18n.Errorf("not finished within %d min", int(p.FileTimeout/time.Minute)))
	t.cancel = cancel
	if guarded {
		t.watching = make(chan struct{})
		go func() {
			defer telemetry.Guard()
			defer close(t.watching)
			watchSlow(t.ctx, cancel, &t.n, t.start, p)
		}()
	}
	return t
}

// open 发 GET 并等到响应头；请求出错或状态码非 200 返回 *sourceError。重定向照常跟随（GitHub 的
// 下载地址先 302 到另一主机），跳转的耗时同样计入时限与低速窗口。
func (t *transfer) open(client *http.Client) error {
	req, err := http.NewRequestWithContext(t.ctx, http.MethodGet, t.url, nil)
	if err == nil {
		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return &sourceError{i18n.Errorf("HTTP %d", resp.StatusCode)}
			}
			t.body, t.length = resp.Body, resp.ContentLength
			return nil
		}
	}
	return t.fail(i18n.Errorf("connection failed: %v", err))
}

// Read 读响应体并累计 t.n；io.EOF 原样返回，其余错误包成 *sourceError。
func (t *transfer) Read(b []byte) (int, error) {
	n, err := t.body.Read(b)
	t.n.Add(int64(n))
	if err != nil && err != io.EOF {
		return n, t.fail(i18n.Errorf("download interrupted: %v", err))
	}
	return n, err
}

// fail 把请求或读取的错误包成来源失败。context 已被取消时以取消原因（过慢、时限）为准，不再包「连接
// 失败」「下载中断」与底层的 URL、context 错误：日志里直接看到为什么放弃这个来源。
func (t *transfer) fail(err error) error {
	if cause := context.Cause(t.ctx); cause != nil {
		err = cause
	}
	return &sourceError{err}
}

// close 取消请求、关闭响应体、等低速判据 goroutine 退出；可重复调用。
func (t *transfer) close() {
	t.cancel(nil)
	t.stopTimer()
	if t.body != nil {
		t.body.Close()
	}
	if t.watching != nil {
		<-t.watching
	}
}

// watchSlow 是低速判据（设计 §4.3）：每 p.Sample 采样一次已收字节数；距 start 满 p.SlowWindow、且
// 最近 p.SlowWindow 内收到的少于 p.SlowMin 时，以「速度过慢」为原因取消请求。ctx 结束即返回。
//
// 采样由计时器驱动，不依赖读取返回：连接不断、完全不来数据时读取一直阻塞，也要判出。只检查以采样时刻
// 结束的窗口：任意连续 SlowWindow+Sample 内收到不足 SlowMin 必判出，计时器抖动另计（用户裁定，设计 §2）。
func watchSlow(ctx context.Context, cancel context.CancelCauseFunc, n *atomic.Int64, start time.Time, p fetchParams) {
	tick := time.NewTicker(p.Sample)
	defer tick.Stop()
	// samples[0] 是窗口起点的累计字节数，最多留一个窗口加一个；发起时刻的 0 作首个样本。
	keep := int(p.SlowWindow/p.Sample) + 1
	samples := []int64{0}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			samples = append(samples, n.Load())
			if len(samples) > keep {
				samples = samples[1:]
			}
			got := samples[len(samples)-1] - samples[0]
			if now.Sub(start) >= p.SlowWindow && got < p.SlowMin {
				cancel(&slowError{i18n.Errorf("too slow: %d KB in the last %d s", got>>10, int(p.SlowWindow/time.Second))})
				return
			}
		}
	}
}

// updateFile 是 .new 的写出目标，测试替换 createUpdateFile 以注入 Sync、Close 的失败。
type updateFile interface {
	io.Writer
	Sync() error
	Close() error
}

var createUpdateFile = func(path string) (updateFile, error) { return os.Create(path) }

// receive 把 prefix 与 t 的余下部分写成 destPath 并核对（设计 §4.4）；done 为真表示 prefix 已是全部
// 响应体，不再读 t。prefix 是测速时已收到的字节，计入大小与 SHA256，不重新下载。
//
// 返回前关闭 t；失败时删除 destPath。来源失败返回 *sourceError，SHA256 不符与本地错误返回普通 error。
// 大小核对先于 SHA256：截断或错误页由来源造成，换源可解，不应落到「SHA256 不符即终止」。
func receive(t *transfer, prefix []byte, done bool, asset updateAsset, destPath string) (err error) {
	defer t.close()
	out, err := createUpdateFile(destPath)
	if err != nil {
		return i18n.Errorf("failed to create file: %v", err)
	}
	defer func() {
		if err != nil {
			os.Remove(destPath)
		}
	}()

	total := asset.Size
	if total <= 0 {
		total = t.length
	}
	if total > 0 {
		taskbar.Progress(0) // 每个文件从 0 重新起跑（文件数不固定，逐个跑满进度条）
	}
	hasher := sha256.New()
	pw := &progressWriter{out: os.Stdout, total: total}
	dst := io.MultiWriter(out, hasher, pw)

	n := int64(len(prefix))
	_, err = dst.Write(prefix)
	if err == nil && !done {
		var src io.Reader = t
		if asset.Size > 0 {
			// 多读 1 字节：读到即判多出，不必读完来源多发的部分。
			src = io.LimitReader(t, asset.Size-n+1)
		}
		var c int64
		c, err = io.Copy(dst, src)
		n += c
	}
	// 进度行之后的日志（含调用方打的失败）另起一行。
	pw.endLine()
	// 读取的错误已是 *sourceError，其余来自写出目标，是本地错误。
	if err != nil && !isSourceError(err) {
		err = i18n.Errorf("download interrupted: %v", err)
	}
	// 落盘的边界：Sync 与 Close 的错误都要检查，Sync 失败也要 Close——Windows 上未关闭的文件删不掉。
	if err == nil {
		if e := out.Sync(); e != nil {
			err = i18n.Errorf("failed to flush file: %v", e)
		}
	}
	if e := out.Close(); e != nil && err == nil {
		err = i18n.Errorf("failed to close file: %v", e)
	}
	if err != nil {
		return err
	}

	if asset.Size > 0 && n > asset.Size {
		return &sourceError{&sizeMismatchError{i18n.Errorf("received more than the expected %d bytes", asset.Size)}}
	}
	if asset.Size > 0 && n != asset.Size {
		return &sourceError{&sizeMismatchError{i18n.Errorf("size mismatch: expected %d bytes, got %d", asset.Size, n)}}
	}
	if asset.Digest != "" {
		actual := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if actual != asset.Digest {
			return &sha256MismatchError{i18n.Errorf("SHA256 verification failed: expected %s, got %s", asset.Digest, actual)}
		}
		mainLog.Success("SHA256 verification succeeded")
	}
	mainLog.Success("Download complete (%.1f MB)", float64(n)/1024/1024)
	return nil
}

// raceEntry 是一个来源在判定时的测速结果：收到多少、用时多少，或判负原因。
type raceEntry struct {
	src     updateSource
	bytes   int64
	elapsed time.Duration
	err     error // 判负原因；nil 为未判负
}

// raceResult 是测速的判定。
type raceResult struct {
	winner  *transfer   // nil：无胜者
	prefix  []byte      // 胜者在测速期间收到的字节，按收到的顺序
	done    bool        // 胜者在测速期间已读完响应体
	err     error       // 胜者在交接前出错（判定时尚未返回的 open 或读取，*sourceError）；非 nil 时 winner 已关闭
	entries []raceEntry // 顺序同 sources
}

// raceSources 对 sources（恰两个）同时请求 file，按设计 §4.2 判定。两个请求都 guarded。
// 返回时：读取任何响应体的 goroutine 都已退出；败者已关闭；胜者未关闭，可直接交给 receive。
//
// 某来源收满 p.RaceCap 或读完整个响应体、各来源都已判负、距发起满 p.RaceWindow，三者先到时判定。胜者为先
// 收满或先读完者；窗口到期时为未判负者中收到字节最多的，相同取顺序在前者（国内前缀）。读完与收满同等对待：
// 文件恰好 RaceCap 字节时，最后一次读取会连同数据返回 io.EOF；文件不足 RaceCap 时，先读完者即更快。
//
// 请求的 context 只有整文件时限，不带 RaceWindow：胜者的请求就是正式下载的请求。判定后胜者不取消、只停止
// 读取；它若正阻塞在一次读取中，等这次读取返回（同一响应体任一时刻只有一个 goroutine 读），至迟由低速判据
// 或整文件时限结束。
func raceSources(client *http.Client, p fetchParams, sources []updateSource, tag, file string) raceResult {
	type racer struct {
		t   *transfer
		buf []byte    // 收到的字节，容量为 p.RaceCap
		eof bool      // 读完了响应体
		err error     // open 或 Read 的错误（*sourceError）
		end time.Time // 读取 goroutine 退出的时刻
	}
	rs := make([]*racer, len(sources))
	exited := make(chan int, len(sources)) // 读取 goroutine 退出时发出自己的下标
	stop := make(chan struct{})            // 判定后关闭：读取 goroutine 读完手上这一次即退出
	for i, src := range sources {
		r := &racer{t: newTransfer(p, src, src.url(tag, file), true), buf: make([]byte, 0, p.RaceCap)}
		rs[i] = r
		go func() {
			defer telemetry.Guard()
			defer func() {
				r.end = time.Now()
				exited <- i
			}()
			if r.err = r.t.open(client); r.err != nil {
				return
			}
			for len(r.buf) < cap(r.buf) {
				select {
				case <-stop:
					return
				default:
				}
				n, err := r.t.Read(r.buf[len(r.buf):cap(r.buf)])
				r.buf = r.buf[:len(r.buf)+n]
				if err == io.EOF {
					r.eof = true
					return
				}
				if err != nil {
					r.err = err
					return
				}
			}
		}()
	}

	// 判定之前退出的读取 goroutine：判定前 stop 未关闭，无错误退出即收满或读完，第一个这样的来源胜出。
	ended := make([]bool, len(rs))
	running, first := len(rs), -1
	note := func(i int) {
		ended[i] = true
		running--
		if first < 0 && rs[i].err == nil {
			first = i
		}
	}
	timer := time.NewTimer(p.RaceWindow)
	defer timer.Stop()
	for timeUp := false; first < 0 && running > 0 && !timeUp; {
		select {
		case i := <-exited:
			note(i)
		case <-timer.C:
			timeUp = true
		}
	}
	// 与时限同时就绪的退出（select 随机取一）也算在判定之前：已判负的来源不能当作仍在读而胜出。
	for drained := false; !drained; {
		select {
		case i := <-exited:
			note(i)
		default:
			drained = true
		}
	}

	now := time.Now()
	res := raceResult{entries: make([]raceEntry, len(rs))}
	for i, r := range rs {
		e := raceEntry{src: sources[i], bytes: r.t.n.Load(), elapsed: now.Sub(r.t.start)}
		if ended[i] {
			e.elapsed, e.err = r.end.Sub(r.t.start), r.err
		}
		res.entries[i] = e
	}
	w := first
	if w < 0 {
		for i, e := range res.entries {
			if e.err == nil && (w < 0 || e.bytes > res.entries[w].bytes) {
				w = i
			}
		}
	}

	// 败者取消，胜者停止读取。败者等读取 goroutine 退出后再 close：open 写 t.body，close 读它。
	close(stop)
	for i, r := range rs {
		if i != w {
			r.t.cancel(nil)
		}
	}
	for ; running > 0; running-- {
		<-exited
	}
	for i, r := range rs {
		if i != w {
			r.t.close()
		}
	}
	if w >= 0 {
		r := rs[w]
		res.winner, res.prefix, res.done, res.err = r.t, r.buf, r.eof, r.err
		if r.err != nil {
			r.t.close()
		}
	}
	return res
}

// updateHTTPClient 返回下载用的 client。不设 Timeout：时限由 transfer 的 context 管（设计 §4.3）。Transport
// 用默认的 http.DefaultTransport：它读 HTTP_PROXY / HTTPS_PROXY，经代理上网的机器照常更新。
func updateHTTPClient() *http.Client { return &http.Client{} }

// fetchAssets 把 assets 依次下载为 <dir>/<名字>.new 并核对，不放置（设计 §4.2–§4.4、§4.7）。sources 是
// updateSources 的结果：两个来源时先对 assets[0] 测速，胜者的连接接续下载它；一个来源时不测速。
//
// 当前来源失败（*sourceError）且备用来源未用过时，换到备用来源从头重下这个文件，余下文件都用它。每次至多换
// 一次，换过去的来源是最后一个，不设低速判据。SHA256 不符与本地错误不换源。
//
// 成功返回各文件的 .new 路径，顺序同 assets；任一文件最终失败时删除本次已下载的全部 .new 并返回带文件名的
// 错误（以 %w 包住原错误），一个文件也不放置（#214：逐个下载、放置时 exe 先被替换，其后的附件下载失败，
// 下一次启动已是新版、不再更新，那些附件一直是旧的或缺失）。tried 是下载阶段请求过的来源，按首次请求的顺序，
// 测速败者不计。
func fetchAssets(client *http.Client, p fetchParams, sources []updateSource, tag string, assets []updateAsset, dir string) (tmpPaths []string, tried []updateSource, err error) {
	tmpPaths, tried, _, err = fetchAssetsWithStats(client, p, sources, tag, assets, dir)
	return tmpPaths, tried, err
}

func fetchAssetsWithStats(client *http.Client, p fetchParams, sources []updateSource, tag string, assets []updateAsset, dir string) (tmpPaths []string, tried []updateSource, stats fetchStats, err error) {
	stats.RaceWinner = "single"
	if len(sources) == 0 {
		return nil, nil, stats, i18n.Errorf("no download source provided")
	}
	defer func() {
		if err != nil {
			for _, path := range tmpPaths {
				os.Remove(path)
			}
			tmpPaths = nil
		}
	}()

	// cur 是当前来源；spare 是未用过的备用来源，-1 为没有。测速不出胜者时国内前缀（sources[0]）在先。
	cur, spare := 0, -1
	if len(sources) > 1 {
		spare = 1
	}
	var race *raceResult // 测速胜者，接续下载第一个文件
	if spare >= 0 && len(assets) > 0 {
		mainLog.Info("Testing download speed: %s", assets[0].Name)
		res := raceSources(client, p, sources[:2], tag, assets[0].Name)
		if len(res.entries) > 0 {
			stats.ChinaKBps = raceEntryKBps(res.entries[0])
		}
		if len(res.entries) > 1 {
			stats.GlobalKBps = raceEntryKBps(res.entries[1])
		}
		for _, e := range res.entries {
			if e.err != nil {
				if isSlowError(e.err) {
					stats.SlowTriggered = true
				}
				mainLog.Info("%s: speed test failed: %v", e.src.Name, e.err)
			} else {
				mainLog.Info("%s: %d KB / %.1f s", e.src.Name, e.bytes>>10, e.elapsed.Seconds())
			}
		}
		if res.winner == nil {
			mainLog.Warn("Speed test failed on all sources, trying %s first", sources[cur].Name)
		} else {
			isSecond := res.winner.src == sources[1]
			if isSecond {
				cur, spare = 1, 0
			}
			stats.RaceWinner = classifyRaceWinner(res.winner.src, isSecond)
			mainLog.Info("Using %s", sources[cur].Name)
			race = &res
		}
	}

	for i, a := range assets {
		mainLog.Info("[%d/%d] %s", i+1, len(assets), a.Name)
		dest := filepath.Join(dir, a.Name+".new")
		for {
			src := sources[cur]
			if len(tried) == 0 || tried[len(tried)-1] != src {
				tried = append(tried, src)
			}
			mainLog.Info("Downloading: %s", src.url(tag, a.Name))
			var e error
			if race != nil {
				// 胜者在交接前出错时已关闭，按它的来源失败处理。
				if e = race.err; e == nil {
					e = receive(race.winner, race.prefix, race.done, a, dest)
				}
				race = nil
			} else {
				t := newTransfer(p, src, src.url(tag, a.Name), spare >= 0)
				if e = t.open(client); e != nil {
					t.close()
				} else {
					e = receive(t, nil, false, a, dest)
				}
			}
			if e == nil {
				break
			}
			if isSlowError(e) {
				stats.SlowTriggered = true
			}
			if !isSourceError(e) || spare < 0 {
				// %w：调用方仍可区分来源失败与 SHA256 不符、本地错误。
				return tmpPaths, tried, stats, i18n.Errorf("failed to download %s: %w", a.Name, e)
			}
			stats.SwitchedSource = true
			mainLog.Warn("Download from %s failed: %v, switching to %s", src.Name, e, sources[spare].Name)
			cur, spare = spare, -1
		}
		tmpPaths = append(tmpPaths, dest)
	}
	return tmpPaths, tried, stats, nil
}
