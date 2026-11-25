package qqmusic

// 本文件是歌词关联读取者各用例共用的桩与夹具，不含用例。依赖全部为桩：文件在内存里，Restart Manager 与编号兜底请求
// 记下每次调用，QQ 的内存是假的地址空间，时间是 testing/synctest 泡内的假时钟。.dat 夹具由测试内实现的
// oi_symmetry_encrypt2 现造，路径、歌名与编号都是虚构的。

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	stdlog "log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

const (
	testQQConfigDir = `C:\QQTest\Tencent\QQMusic`
	testQQCacheDir  = `D:\QQTestCache\`
	testQQPID       = 4242
	testQQVersion   = "22.82.0.0"

	testConfigPath = testQQConfigDir + `\ConfigInfoXML1.dat`
	testLyricDir   = testQQCacheDir + `QQMusicLyricNew`
	testTablePath  = testLyricDir + `\LyricAssociation.dat`
	testIndexPath  = testLyricDir + `\lyricinfo.dat`
	// testDefaultTablePath 是配置读不出时退回的缓存目录 <配置目录>\QQMusicCache\ 下的关联表。
	testDefaultTablePath = testQQConfigDir + `\QQMusicCache\QQMusicLyricNew\LyricAssociation.dat`

	testAssocLRC  = "[00:01.00]关联第一行\n[00:03.00]关联第二行\n"
	testAssocLRC2 = "[00:02.00]另一份第一行\n[00:04.00]另一份第二行\n[00:06.00]另一份第三行\n"
	testTransLRC2 = "[00:01.00]译文一\n[00:03.00]译文二\n"
)

// testTEAEncrypt 按 oi_symmetry_encrypt2 加密 payload（密钥 assoc.Key；首字节高位、填充与盐取固定值），assoc.DecryptDat 可解。
func testTEAEncrypt(payload []byte) []byte {
	pad := (8 - (len(payload)+10)%8) % 8
	plain := append([]byte{0xA8 | byte(pad)}, bytes.Repeat([]byte{0x5A}, pad+2)...)
	plain = append(plain, payload...)
	plain = append(plain, make([]byte, 7)...)
	var k [4]uint32
	for i := range k {
		k[i] = binary.BigEndian.Uint32(assoc.Key[4*i:])
	}
	out := make([]byte, len(plain))
	var prevCipher, prevIn [8]byte
	for off := 0; off < len(plain); off += 8 {
		var in, enc [8]byte
		for j := range in {
			in[j] = plain[off+j] ^ prevCipher[j]
		}
		y, z := binary.BigEndian.Uint32(in[:]), binary.BigEndian.Uint32(in[4:])
		var sum uint32
		for range 16 {
			sum += 0x9E3779B9
			y += ((z << 4) + k[0]) ^ (z + sum) ^ ((z >> 5) + k[1])
			z += ((y << 4) + k[2]) ^ (y + sum) ^ ((y >> 5) + k[3])
		}
		binary.BigEndian.PutUint32(enc[:], y)
		binary.BigEndian.PutUint32(enc[4:], z)
		for j := range enc {
			out[off+j] = enc[j] ^ prevIn[j]
		}
		copy(prevCipher[:], out[off:off+8])
		prevIn = in
	}
	return out
}

// testDat 把 XML 编码为带 BOM 的 UTF-16LE 并加密，即 QQ 的 .dat 文件。
func testDat(xml string) []byte {
	b := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(xml)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return testTEAEncrypt(b)
}

const testXMLDecl = `<?xml version="1.0" encoding="utf-8"?>`

// assocItem 是关联表的一条。
type assocItem struct {
	song, lyric string
	byUser      bool
	offset      int
}

func tableDat(items ...assocItem) []byte {
	var b strings.Builder
	b.WriteString(testXMLDecl + "<AssociateInfos>")
	for _, it := range items {
		u := "0"
		if it.byUser {
			u = "1"
		}
		fmt.Fprintf(&b, `<Associate songUrl="%s" lyricUrl="%s" byUser="%s" LyricOffset="%d"/>`, it.song, it.lyric, u, it.offset)
	}
	b.WriteString("</AssociateInfos>")
	return testDat(b.String())
}

// indexDat 是只有一条的歌词索引：原文 main 的翻译 trans、音译 roma。
func indexDat(main, trans, roma string) []byte {
	return testDat(fmt.Sprintf(testXMLDecl+`<LyricInfos><Lyric url="%s" tsurl="%s" romaurl="%s" id="1"/></LyricInfos>`, main, trans, roma))
}

// configDat 是只有缓存目录设置的 ConfigInfoXML1.dat。
func configDat(cachePath string) []byte {
	return testDat(testXMLDecl + `<Config><Domain DomainName="CachePath.ini" Path="` + cachePath + `"/></Config>`)
}

func midURL(mid string) string { return "http://stream4.qqmusic.qq.com/" + mid + ".wma" }
func idURL(id uint32) string   { return fmt.Sprintf("http://streamundefined.qqmusic.qq.com/%d.wma", id) }

// memFile 是内存文件系统里的一项；dir 为真即目录。
type memFile struct {
	data []byte
	mod  time.Time
	dir  bool
}

type fsRead struct {
	path string
	at   time.Duration
}

// memFS 是读取者的 read 与 stat 桩：路径不区分大小写；每次 put 都给出新的修改时间（与时钟无关），
// 不 put 即（大小，修改时间）不变。read 按 readShared 的约定：不存在为 fs.ErrNotExist，超过 max 为 errLocalTooLarge。
type memFS struct {
	mu      sync.Mutex
	t0      time.Time
	seq     int
	files   map[string]memFile
	reads   []fsRead
	stats   []fsRead
	onRead  func(path string)       // 非空时在每次读取之前于锁外调用，可阻塞或 panic
	readErr func(path string) error // 非空且返回非 nil 时，读取返回该错误
	onStat  func(path string)       // 非空时在每次 stat 之前于锁外调用，可阻塞或推进时钟
}

func newMemFS() *memFS { return &memFS{files: map[string]memFile{}} }

func (m *memFS) put(path string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.files[strings.ToLower(path)] = memFile{data: data, mod: time.Unix(1_700_000_000, int64(m.seq))}
}

// putSameStat 改写 path 的内容，大小与修改时间不变（文件须已存在）。
func (m *memFS) putSameStat(path string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files[strings.ToLower(path)]
	if len(f.data) != len(data) {
		panic("putSameStat: 长度须相同")
	}
	f.data = data
	m.files[strings.ToLower(path)] = f
}

// putKeepMod 改写 path 的内容，修改时间不变、大小随内容（文件须已存在）。
func (m *memFS) putKeepMod(path string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files[strings.ToLower(path)]
	f.data = data
	m.files[strings.ToLower(path)] = f
}

// hide 让 path 暂时不存在，返回原来的那一项；unhide 原样放回（大小与修改时间都不变），如断开又接回的网络盘。
func (m *memFS) hide(path string) memFile {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files[strings.ToLower(path)]
	delete(m.files, strings.ToLower(path))
	return f
}

func (m *memFS) unhide(path string, f memFile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[strings.ToLower(path)] = f
}

func (m *memFS) mkdir(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[strings.ToLower(path)] = memFile{dir: true}
}

func (m *memFS) remove(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, strings.ToLower(path))
}

func (m *memFS) setHooks(onRead func(string), readErr func(string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRead, m.readErr = onRead, readErr
}

func (m *memFS) read(path string, max int64) ([]byte, error) {
	m.mu.Lock()
	m.reads = append(m.reads, fsRead{path, time.Since(m.t0)})
	hook, errFn := m.onRead, m.readErr
	m.mu.Unlock()
	if hook != nil {
		hook(path)
	}
	if errFn != nil {
		if err := errFn(path); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[strings.ToLower(path)]
	if !ok || f.dir {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	if int64(len(f.data)) > max {
		return nil, errLocalTooLarge
	}
	return bytes.Clone(f.data), nil
}

func (m *memFS) stat(path string) (os.FileInfo, error) {
	m.mu.Lock()
	m.stats = append(m.stats, fsRead{path, time.Since(m.t0)})
	hook := m.onStat
	m.mu.Unlock()
	if hook != nil {
		hook(path)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[strings.ToLower(path)]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return memInfo{name: filepath.Base(path), size: int64(len(f.data)), mod: f.mod, dir: f.dir}, nil
}

// readsOf 等读取者停下，返回读 path（不区分大小写）的各次时刻。
func (m *memFS) readsOf(path string) []time.Duration {
	synctest.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []time.Duration
	for _, r := range m.reads {
		if strings.EqualFold(r.path, path) {
			out = append(out, r.at)
		}
	}
	return out
}

// statsOf 等读取者停下，返回 stat path（不区分大小写）的各次时刻。
func (m *memFS) statsOf(path string) []time.Duration {
	synctest.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []time.Duration
	for _, s := range m.stats {
		if strings.EqualFold(s.path, path) {
			out = append(out, s.at)
		}
	}
	return out
}

// statPathsAt 等读取者停下，返回在时刻 at stat 过的路径（转小写、排序，可重复）。
func (m *memFS) statPathsAt(at time.Duration) []string {
	synctest.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, s := range m.stats {
		if s.at == at {
			out = append(out, strings.ToLower(s.path))
		}
	}
	slices.Sort(out)
	return out
}

// statCount 等读取者停下，返回全部 stat 的次数。
func (m *memFS) statCount() int {
	synctest.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.stats)
}

// readCount 等读取者停下，返回全部读取的次数。
func (m *memFS) readCount() int {
	synctest.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reads)
}

type memInfo struct {
	name string
	size int64
	mod  time.Time
	dir  bool
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) ModTime() time.Time { return i.mod }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }
func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

type rmCall struct {
	at    time.Duration
	paths []string
}

// fakeRM 是 Restart Manager 查询的桩：记下每次会话注册的路径；fn 为 nil 时无人占用。
type fakeRM struct {
	mu    sync.Mutex
	t0    time.Time
	calls []rmCall
	fn    func(paths []string) ([]uint32, error)
}

func (f *fakeRM) holders(paths ...string) ([]uint32, error) {
	f.mu.Lock()
	f.calls = append(f.calls, rmCall{time.Since(f.t0), slices.Clone(paths)})
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(paths)
}

func (f *fakeRM) setFn(fn func(paths []string) ([]uint32, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
}

// callTimes 等读取者停下，返回各次查询的时刻。
func (f *fakeRM) callTimes() []time.Duration {
	synctest.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []time.Duration
	for _, c := range f.calls {
		out = append(out, c.at)
	}
	return out
}

// registered 等读取者停下，返回各次查询注册过的全部路径。
func (f *fakeRM) registered() []string {
	synctest.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.paths...)
	}
	return out
}

// openBy 返回一个占用查询：注册的路径里有 open 之一（不区分大小写）时，占用者为 pids。
func openBy(open []string, pids ...uint32) func(paths []string) ([]uint32, error) {
	return func(paths []string) ([]uint32, error) {
		for _, p := range paths {
			for _, o := range open {
				if strings.EqualFold(p, o) {
					return pids, nil
				}
			}
		}
		return nil, nil
	}
}

// fakeDetails 是编号兜底请求的桩（设计 §6.4 第 2 步「兜底」），按所查编号返回、记下每次请求的时刻：resolveID（按 songid 查
// songmid）与 resolveMid（按 songmid 查 songid）各一个。vals 里有所查编号即成功；fn 非空时取代 vals（可阻塞）；都没有即报
// errDetailOffline（同核验不过或请求失败：真实的请求函数对另一首的详情同样只报错）。
type fakeDetails[K comparable, V any] struct {
	mu    sync.Mutex
	t0    time.Time
	calls []time.Duration
	vals  map[K]V
	fn    func(k K) (V, error)
}

var errDetailOffline = i18n.Errorf("offline")

func newFakeDetails[K comparable, V any]() *fakeDetails[K, V] {
	return &fakeDetails[K, V]{vals: map[K]V{}}
}

func (f *fakeDetails[K, V]) resolve(k K) (V, error) {
	f.mu.Lock()
	f.calls = append(f.calls, time.Since(f.t0))
	fn := f.fn
	v, ok := f.vals[k]
	f.mu.Unlock()
	switch {
	case fn != nil:
		return fn(k)
	case ok:
		return v, nil
	}
	var zero V
	return zero, errDetailOffline
}

func (f *fakeDetails[K, V]) set(k K, v V) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vals[k] = v
}

func (f *fakeDetails[K, V]) setFn(fn func(k K) (V, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
}

func (f *fakeDetails[K, V]) setT0(t0 time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t0 = t0
}

// callTimes 等读取者停下，返回各次请求的时刻（不论所查编号）。
func (f *fakeDetails[K, V]) callTimes() []time.Duration {
	synctest.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// —— 读 QQ 内存的桩（当前歌曲记录，设计 §6.4 第 4 步）——

// 假地址空间里扫描不读的几种区域类型与状态（x/sys/windows 没有这几个常量）。
const (
	memFree   = 0x10000
	memMapped = 0x40000
	memImage  = 0x1000000
)

var (
	errFakeNoAccess = errors.New("fake: no access")
	errFakePartial  = errors.New("fake: partial copy")
)

// fakeRegion 是假地址空间里的一个区域；data 为 nil 时内容全为 0、长度为 size（不占内存，供大区域）。
type fakeRegion struct {
	memRegion
	data []byte
}

// privRegion 是一个已提交、私有、保护恰为 PAGE_READWRITE 的区域，即扫描要读的那种。
func privRegion(base uintptr, data []byte) fakeRegion {
	return fakeRegion{memRegion{base: base, size: uintptr(len(data)), state: windows.MEM_COMMIT, protect: windows.PAGE_READWRITE, typ: memPrivate}, data}
}

type fakeRead struct {
	addr uintptr
	n    int
}

// fakeSpace 是 memSource 的桩：regions 按 base 升序、互不重叠，其间与第一个之前为空闲区域，最后一个之后地址空间到头
// （query 报 ERROR_INVALID_PARAMETER，同 VirtualQueryEx）。qfail 非空且返回非 nil 时，query 报该错误。
// read 只在一个区域之内读：越过区域末尾时读到末尾为止并返回 errFakePartial，不在任何区域里返回 errFakeNoAccess。
// fail 非空且返回 k ≥ 0 时，本次照常拷满，却只报读到 k 字节并返回 errFakeNoAccess：缓冲区里 k 之后的内容不该被用到。
// log 为真时记下每次读的地址与长度。不记录时 query 与 read 都不分配内存。
type fakeSpace struct {
	regions []fakeRegion
	qfail   func(addr uintptr) error
	fail    func(addr uintptr, n int) int
	log     bool
	reads   []fakeRead
	queries int
}

func (s *fakeSpace) source() memSource { return memSource{query: s.query, read: s.read} }

func (s *fakeSpace) query(addr uintptr) (memRegion, error) {
	s.queries++
	if s.qfail != nil {
		if err := s.qfail(addr); err != nil {
			return memRegion{}, err
		}
	}
	for _, r := range s.regions {
		switch {
		case addr < r.base:
			return memRegion{base: addr, size: r.base - addr, state: memFree}, nil
		case addr-r.base < r.size:
			return r.memRegion, nil
		}
	}
	return memRegion{}, windows.ERROR_INVALID_PARAMETER
}

func (s *fakeSpace) read(addr uintptr, buf []byte) (int, error) {
	if s.log {
		s.reads = append(s.reads, fakeRead{addr, len(buf)})
	}
	for _, r := range s.regions {
		if addr < r.base || addr-r.base >= r.size {
			continue
		}
		off := addr - r.base
		n := int(min(uintptr(len(buf)), r.size-off))
		if r.data != nil {
			copy(buf[:n], r.data[off:])
		} else {
			clear(buf[:n])
		}
		if s.fail != nil {
			if k := s.fail(addr, len(buf)); k >= 0 {
				return min(k, n), errFakeNoAccess
			}
		}
		if n < len(buf) {
			return n, errFakePartial
		}
		return n, nil
	}
	return 0, errFakeNoAccess
}

// recordBytes 是 QQ 内存里一首歌的记录：专辑、歌手、歌名、网址、参数直接相接（设计 §2.2）。
func recordBytes(album, singer, name, url, param0 string) []byte {
	return []byte(album + singer + name + url + "0=" + param0 + "&1=128&2=" + param0)
}

// utf16z 是 s 的 UTF-16LE 编码，带结尾的 NUL。
func utf16z(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return append(b, 0, 0)
}

// ssoField 是 24 字节的 MSVC 32 位 std::string：+0x00 起是 head（内联的字节，或 4 字节的堆指针），+0x10 长度，+0x14 容量
// （大于 15 即在堆上）。
func ssoField(head []byte, length, capacity uint32) []byte {
	f := make([]byte, 24)
	copy(f[:16], head)
	binary.LittleEndian.PutUint32(f[0x10:], length)
	binary.LittleEndian.PutUint32(f[0x14:], capacity)
	return f
}

// narrowField 是 b 照 MSVC 的放法所成的 std::string：短于 16 字节的内联（容量 15，其后为 NUL）；其余在堆上的 ptr 处，容量
// len(b)|15，串由调用方放到 ptr 处。
func narrowField(b []byte, ptr uint32) []byte {
	if len(b) < 16 {
		return ssoField(b, uint32(len(b)), 15)
	}
	return ssoField(binary.LittleEndian.AppendUint32(nil, ptr), uint32(len(b)), uint32(len(b))|15)
}

// 假内存的布局：Struct1 在映像区（扫描不读），按 22.82 的偏移放三个 32 位指针，指向其后私有可写区里的三个 UTF-16 串
// （扫描会读到，UTF-16 不会命中 UTF-8 的前缀）；useNarrow 之后改按 22.16 的偏移放三个 24 字节的 std::string，堆上的串在
// 同一私有可写区，各带结尾的 NUL（不会命中前缀）。记录在 fakeRecordBase 起的私有可写区，每条之前垫 16 字节；用例另加的区域
// 按地址插在其间；fakeTailBase 处另有一个小的私有可写区，是一次扫描的末块。
const (
	fakeStruct1    = 0x10000
	fakeStrBase    = 0x20000
	fakeRecordBase = 0x100000
	fakeTailBase   = 0x7F0000
	fakeAlbum      = "测试专辑"
	fakeSinger     = "测试歌手"
)

// fakeMem 是读取者读 QQ 内存的桩：setPlaying 改 Struct1 所指的专辑、歌手、歌名，putRecord 往记录区追加一条；自地址 0 起的
// 查询即一次扫描的开始，记下时刻。onRead 非空时在读记录区、另加的区域与末块之前于锁外调用，可阻塞；qfail、fail 同 fakeSpace。
// closes 是关闭句柄的次数。
type fakeMem struct {
	mu      sync.Mutex
	t0      time.Time
	narrow  bool      // 窄字符模型（22.16），见 useNarrow
	strs    [3]string // 专辑、歌手、歌名
	records []byte
	extra   []fakeRegion
	scans   []time.Duration
	onRead  func(addr uintptr)
	qfail   func(addr uintptr) error
	fail    func(addr uintptr, n int) int
	closes  int
}

func (m *fakeMem) setPlaying(album, singer, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.strs = [3]string{album, singer, name}
}

// useNarrow 改用 22.16 的窄字符模型：Struct1 里的三段照 narrowField 放，串的字节原样放入、不转码（setPlaying 可给非 UTF-8
// 的字节）。须在 source 之前调用。
func (m *fakeMem) useNarrow() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.narrow = true
}

// version 是假内存所仿的版本，调用方持锁。
func (m *fakeMem) version() string {
	if m.narrow {
		return "22.16"
	}
	return "22.82"
}

func (m *fakeMem) putRecord(album, singer, name, url, param0 string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, bytes.Repeat([]byte{0xEE}, 16)...)
	m.records = append(m.records, recordBytes(album, singer, name, url, param0)...)
}

// play 让 Struct1 指向 name（专辑、歌手取 fakeAlbum、fakeSinger），并放上它的记录：网址 url、参数 0 为 param0。
func (m *fakeMem) play(name, url, param0 string) {
	m.setPlaying(fakeAlbum, fakeSinger, name)
	m.putRecord(fakeAlbum, fakeSinger, name, url, param0)
}

func (m *fakeMem) clearRecords() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = nil
}

// setExtra 换掉用例另加的区域（地址须在 fakeRecordBase 之后、fakeTailBase 之前）。
func (m *fakeMem) setExtra(rs ...fakeRegion) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.extra = rs
}

func (m *fakeMem) setHooks(onRead func(addr uintptr), fail func(addr uintptr, n int) int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRead, m.fail = onRead, fail
}

func (m *fakeMem) setQueryFail(qfail func(addr uintptr) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.qfail = qfail
}

// space 按当前状态拼出地址空间，调用方持锁。
func (m *fakeMem) space() *fakeSpace {
	offs := knownVersions[m.version()]
	fields := [...]uintptr{offs.AlbumOff, offs.SingerOff, offs.NameOff}
	var strs, st []byte
	if m.narrow {
		st = make([]byte, max(fields[0], fields[1], fields[2])+24)
		for i, off := range fields {
			s := []byte(m.strs[i])
			copy(st[off:], narrowField(s, uint32(fakeStrBase+len(strs))))
			if len(s) >= 16 {
				strs = append(append(strs, s...), 0)
			}
		}
	} else {
		st = make([]byte, 12)
		for i, off := range fields {
			binary.LittleEndian.PutUint32(st[off:], uint32(fakeStrBase+len(strs)))
			strs = append(strs, utf16z(m.strs[i])...)
		}
	}
	image := fakeRegion{memRegion{base: fakeStruct1, size: uintptr(len(st)), state: windows.MEM_COMMIT, protect: windows.PAGE_READWRITE, typ: memImage}, st}
	rs := []fakeRegion{image, privRegion(fakeStrBase, strs), privRegion(fakeRecordBase, append(bytes.Repeat([]byte{0xEE}, 16), m.records...))}
	rs = append(rs, m.extra...)
	rs = append(rs, privRegion(fakeTailBase, bytes.Repeat([]byte{0xEE}, 64)))
	sort.Slice(rs, func(i, j int) bool { return rs[i].base < rs[j].base })
	return &fakeSpace{regions: rs, qfail: m.qfail, fail: m.fail}
}

func (m *fakeMem) query(addr uintptr) (memRegion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if addr == 0 {
		m.scans = append(m.scans, time.Since(m.t0))
	}
	return m.space().query(addr)
}

func (m *fakeMem) read(addr uintptr, buf []byte) (int, error) {
	m.mu.Lock()
	hook := m.onRead
	m.mu.Unlock()
	if hook != nil && addr >= fakeRecordBase {
		hook(addr)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.space().read(addr, buf)
}

func (m *fakeMem) source() *recordSource {
	m.mu.Lock()
	offs := knownVersions[m.version()]
	m.mu.Unlock()
	return &recordSource{
		mem: memSource{query: m.query, read: m.read}, struct1: fakeStruct1, offs: offs,
		close: func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.closes++
		},
	}
}

// scanTimes 等读取者停下，返回各次扫描开始的时刻。
func (m *fakeMem) scanTimes() []time.Duration {
	synctest.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.scans)
}

func (m *fakeMem) closeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closes
}

// readerEnv 是一个读取者的全部桩。newReaderEnv 的配置指向 testQQCacheDir，关联表与索引尚不存在；假内存里没有记录，
// Struct1 的三个串为空。noRecord 为真时读取者的 record 为 nil（本版本不读记录），假内存照常可查扫描次数。
type readerEnv struct {
	fs          *memFS
	rm          *fakeRM
	byID        *fakeDetails[uint32, string] // resolveID
	byMid       *fakeDetails[string, uint32] // resolveMid
	mem         *fakeMem
	noRecord    bool
	configDir   string
	pid         uint32
	beforeStart func() // 非空时在泡内、启动读取者之前调用（如 arm 一个 gate）
}

func newReaderEnv() *readerEnv {
	e := &readerEnv{fs: newMemFS(), rm: &fakeRM{}, byID: newFakeDetails[uint32, string](), byMid: newFakeDetails[string, uint32](),
		mem: &fakeMem{}, configDir: testQQConfigDir, pid: testQQPID}
	e.fs.put(testConfigPath, configDat(testQQCacheDir))
	return e
}

func (e *readerEnv) deps() assocDeps {
	d := assocDeps{
		configDir: e.configDir, read: e.fs.read, stat: e.fs.stat, holders: e.rm.holders, resolveID: e.byID.resolve,
		resolveMid: e.byMid.resolve, now: time.Now, qqPID: e.pid, qqVersion: testQQVersion,
	}
	if !e.noRecord {
		d.record = e.mem.source()
	}
	return d
}

func (e *readerEnv) setT0(t0 time.Time) {
	e.mem.mu.Lock()
	e.mem.t0 = t0
	e.mem.mu.Unlock()
	e.fs.mu.Lock()
	e.fs.t0 = t0
	e.fs.mu.Unlock()
	e.rm.mu.Lock()
	e.rm.t0 = t0
	e.rm.mu.Unlock()
	e.byID.setT0(t0)
	e.byMid.setT0(t0)
}

// runAssocReader 在 synctest 的泡里以 e 的桩启动一个读取者，等它做完启动时的定位与解析，再执行 body；body 返回后
// 取消，断言读取者 goroutine 已退出、计数归零。
func runAssocReader(t *testing.T, e *readerEnv, body func(t *testing.T, r *assocReader)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		e.setT0(time.Now())
		if e.beforeStart != nil {
			e.beforeStart()
		}
		r, ok := startAssocReader(ctx, e.deps())
		if !ok {
			t.Fatal("startAssocReader 返回假，want 启动一个读取者")
		}
		synctest.Wait()
		body(t, r)
		cancel()
		synctest.Wait()
		if n := assocReaders.Load(); n != 0 {
			t.Fatalf("取消之后存活的读取者 %d 个，want 0", n)
		}
	})
}

// sleepUntil 把泡内时钟推进到读取者启动之后 at，再等读取者停下。
func sleepUntil(e *readerEnv, at time.Duration) {
	e.fs.mu.Lock()
	t0 := e.fs.t0
	e.fs.mu.Unlock()
	time.Sleep(time.Until(t0.Add(at)))
	synctest.Wait()
}

// latestResult 等读取者停下，取走结果通道里的那一条；通道为空即失败。
func latestResult(t *testing.T, r *assocReader) assocResult {
	t.Helper()
	synctest.Wait()
	select {
	case res := <-r.Results():
		return res
	default:
		t.Fatal("结果通道为空，want 一条结果")
		return assocResult{}
	}
}

// noAssocResult 等读取者停下，断言结果通道为空。
func noAssocResult(t *testing.T, r *assocReader) {
	t.Helper()
	synctest.Wait()
	select {
	case res := <-r.Results():
		t.Fatalf("意外的结果 %+v，want 通道为空", res)
	default:
	}
}

// lineTexts 返回各行的原文与翻译，供断言。
func lineTexts(lines []lyricLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.Text+"|"+l.SubText)
	}
	return out
}

// wantDesired 断言结果是 gen 的期望状态，来源、偏移、待重试、采纳的本地路径与行（原文|翻译）如所给。
func wantDesired(t *testing.T, got assocResult, gen uint64, src lyricSource, lyricOffsetMs int, pending bool, localPath string, lines ...string) {
	t.Helper()
	if got.gen != gen || got.kind != resDesired || got.source != src || got.lyricOffsetMs != lyricOffsetMs ||
		got.pending != pending || got.localPath != localPath || !slices.Equal(lineTexts(got.lines), lines) {
		t.Fatalf("结果 gen %d kind %d source %d offset %d pending %v local %q lines %q\n"+
			"want gen %d kind %d source %d offset %d pending %v local %q lines %q",
			got.gen, got.kind, got.source, got.lyricOffsetMs, got.pending, got.localPath, lineTexts(got.lines),
			gen, resDesired, src, lyricOffsetMs, pending, localPath, lines)
	}
}

var (
	assocLines  = []string{"关联第一行|", "关联第二行|"}
	assocLines2 = []string{"另一份第一行|", "另一份第二行|", "另一份第三行|"}
)

// gate 让桩阻塞到用例放行。须在泡内 arm：泡外建的通道上的阻塞不算 durably blocked，synctest.Wait 等不到它。
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func (g *gate) arm()     { g.ch = make(chan struct{}) }
func (g *gate) wait()    { <-g.ch }
func (g *gate) release() { g.once.Do(func() { close(g.ch) }) }

type assocReport struct {
	key   string
	tags  map[string]string
	extra map[string]any
}

// stubAssocReports 把 reportOnce 换成记录调用的桩，用例结束时还原。须在启动读取者之前调用。
func stubAssocReports(t *testing.T) func() []assocReport {
	t.Helper()
	var mu sync.Mutex
	var got []assocReport
	orig := reportOnce
	reportOnce = func(key, _ string, tags map[string]string, extra map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, assocReport{key, tags, extra})
	}
	t.Cleanup(func() { reportOnce = orig })
	return func() []assocReport {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// captureAssocLog 把标准库 log 的输出（logger 的各方法都经它）改向缓冲区，用例结束时还原；返回读取当前内容的函数。
func captureAssocLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	stdlog.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { stdlog.SetOutput(os.Stderr) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// countLog 返回日志 out 里图标为 icon（"+"、"!" 等）、含 sub 的行数。
func countLog(out, icon, sub string) int {
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[QQMusic] ["+icon+"] ") && strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
