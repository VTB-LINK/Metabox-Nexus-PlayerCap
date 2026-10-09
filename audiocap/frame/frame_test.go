package frame

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

// putU32 是测试辅助：小端写入一个 u32，用于构造或改写原始帧头。
func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

func TestRoundTrip(t *testing.T) {
	f := Frame{Player: "kugou", Epoch: 3, TSMillis: 123.456, RMS: 0.42, Bins: []float32{0, 0.5, 1, 0.25}}
	var d Decoder
	d.Push(Encode(nil, f))
	got, ok, err := d.Next()
	if err != nil || !ok {
		t.Fatalf("Next ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, f) {
		t.Fatalf("got %+v want %+v", got, f)
	}
}

func TestResyncAfterGarbage(t *testing.T) {
	good := Encode(nil, Frame{Player: "a", Bins: []float32{1}})
	var d Decoder
	d.Push([]byte{0xDE, 0xAD, 0xBE}) // 垃圾前缀
	d.Push(good)                     // 紧跟一帧好的
	// 第一次 Next 可能报坏帧(跳过垃圾)或直接对齐到 good；循环取直到拿到好帧
	var got Frame
	var ok bool
	for i := 0; i < 4 && !ok; i++ {
		got, ok, _ = d.Next()
	}
	if !ok || got.Player != "a" {
		t.Fatalf("resync 失败 got=%+v ok=%v", got, ok)
	}
}

func TestOversizedLengthRejected(t *testing.T) {
	var d Decoder
	// Magic + 一个超过 MaxPayload 的长度前缀
	buf := make([]byte, 8)
	putU32(buf[0:], Magic)
	putU32(buf[4:], uint32(MaxPayload+1))
	d.Push(buf)
	_, ok, err := d.Next()
	if ok || err == nil {
		t.Fatalf("超长前缀应判坏帧: ok=%v err=%v", ok, err)
	}
}

// TestRoundTripAllFields：全字段往返，并按 spec §3 的偏移逐项核对线上字节。
func TestRoundTripAllFields(t *testing.T) {
	f := Frame{Player: "kugou", Flags: FlagNormalized, Epoch: 3, TSMillis: 123456.789, RMS: 0.42, PeakL: 0.9, PeakR: 1.25, Bins: []float32{0, 0.5, 1, 0.25}}
	b := Encode(nil, f)
	le := binary.LittleEndian

	if got, want := le.Uint32(b[4:]), uint32(len(b)-8); got != want {
		t.Fatalf("payloadLen = %d, 期望 %d", got, want)
	}
	// 线上常量按 spec §3 写字面量，不引用包常量，改常量值即红。
	for _, c := range []struct {
		name string
		off  int
		want uint32
	}{
		{"magic", 0, 0x56544157},
		{"version", 8, 1},
		{"headerLen", 12, 48},
		{"flags", 16, 1},
		{"epoch", 20, 3},
		{"rms", 32, math.Float32bits(0.42)},
		{"peakL", 36, math.Float32bits(0.9)},
		{"peakR", 40, math.Float32bits(1.25)},
		{"playerLen", 44, 5},
	} {
		if got := le.Uint32(b[c.off:]); got != c.want {
			t.Errorf("%s@%d = %#x, 期望 %#x", c.name, c.off, got, c.want)
		}
	}
	if got := math.Float64frombits(le.Uint64(b[24:])); got != f.TSMillis {
		t.Errorf("ts@24 = %v, 期望 %v", got, f.TSMillis)
	}
	for i, v := range f.Bins {
		if got := le.Uint32(b[48+4*i:]); got != math.Float32bits(v) {
			t.Errorf("bins[%d]@%d = %#x, 期望 %#x", i, 48+4*i, got, math.Float32bits(v))
		}
	}
	if got := string(b[48+4*4:]); got != "kugou" {
		t.Errorf("player@%d = %q, 期望 %q", 48+4*4, got, "kugou")
	}

	var d Decoder
	d.Push(b)
	got, ok, err := d.Next()
	if err != nil || !ok {
		t.Fatalf("Next ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, f) {
		t.Fatalf("got %+v want %+v", got, f)
	}
}

// TestIdleFrameRoundTrip：空闲帧（无 player、无 bins）只有定长头，解码后 Bins 为 nil。
func TestIdleFrameRoundTrip(t *testing.T) {
	f := Frame{TSMillis: 1}
	b := Encode(nil, f)
	if len(b) != 48 {
		t.Fatalf("空闲帧长 = %d, 期望 48", len(b))
	}
	var d Decoder
	d.Push(b)
	got, ok, err := d.Next()
	if err != nil || !ok {
		t.Fatalf("Next ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, f) {
		t.Fatalf("got %#v want %#v", got, f) // %#v 区分 nil 与空切片
	}
}

// TestBadHeaderRejected：每个子用例使好帧恰好违反 spec §3 的一条校验，坏帧之后须接上下一个好帧。
func TestBadHeaderRejected(t *testing.T) {
	want := Frame{Player: "kugou", Bins: []float32{0.1, 0.2, 0.3, 0.4}}
	good := Encode(nil, want)
	if len(good) != 69 {
		t.Fatalf("测试前提失效：好帧长 = %d, 期望 69", len(good))
	}
	cases := []struct {
		name   string
		mutate func(b []byte)
		want   error
	}{
		{"version=2", func(b []byte) { putU32(b[8:], 2) }, errVersion},
		{"version=0", func(b []byte) { putU32(b[8:], 0) }, errVersion},
		{"headerLen=44", func(b []byte) { putU32(b[12:], 44) }, errHeaderShort},
		// 剩余 69-50-3=16 字节为 4 的倍数，不会被 bins 对齐校验兜住。
		{"headerLen=50", func(b []byte) { putU32(b[12:], 50); putU32(b[44:], 3) }, errHeaderAlign},
		{"headerLen=72", func(b []byte) { putU32(b[12:], 72) }, errHeaderOverrun},
		// 剩余 69-48-25=-4 为 4 的倍数，不会被 bins 对齐校验兜住。
		{"playerLen=25", func(b []byte) { putU32(b[44:], uint32(len(b)-48+4)) }, errPlayerOverrun},
		// 按 u32 相加时 48+0xFFFFFFFC 回绕为 44，会漏过本条、落到 bins 对齐校验。
		{"playerLen=0xFFFFFFFC", func(b []byte) { putU32(b[44:], 0xFFFFFFFC) }, errPlayerOverrun},
		{"playerLen=6", func(b []byte) { putU32(b[44:], 6) }, errBinsAlign},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bad := bytes.Clone(good)
			c.mutate(bad)
			var d Decoder
			d.Push(bad)
			d.Push(good)
			if _, ok, err := d.Next(); ok || !errors.Is(err, c.want) {
				t.Fatalf("坏帧 Next ok=%v err=%v, 期望 %v", ok, err, c.want)
			}
			got, ok, err := d.Next()
			if err != nil || !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("坏帧后的好帧 Next got=%+v ok=%v err=%v", got, ok, err)
			}
		})
	}
}

// TestBadFrameSkipsOnlyMagic：坏帧的 payloadLen 覆盖了其后的好帧时，解码器只跳过坏帧的 magic，
// 不按其声明长度整段丢弃，从而仍能对齐到被覆盖的好帧。
func TestBadFrameSkipsOnlyMagic(t *testing.T) {
	a := Encode(nil, Frame{Player: "kugou", Bins: []float32{0.1, 0.2}})
	want := Frame{Player: "qqmusic", Epoch: 2, TSMillis: 7, Bins: []float32{0.3}}
	b := Encode(nil, want)
	bad := bytes.Clone(a)
	putU32(bad[8:], 2)                       // version=2
	putU32(bad[4:], uint32(len(a)-8+len(b))) // 声明长度恰好覆盖到 b 的末尾

	var d Decoder
	d.Push(bad)
	d.Push(b)
	if _, ok, err := d.Next(); ok || !errors.Is(err, errVersion) {
		t.Fatalf("坏帧 Next ok=%v err=%v, 期望 %v", ok, err, errVersion)
	}
	got, ok, err := d.Next()
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("被坏帧声明长度覆盖的好帧 Next got=%+v ok=%v err=%v", got, ok, err)
	}
}

// TestShortPayloadRejected：帧长不足 48 时须在读任何字段之前判 errShort，不得越界 panic。
func TestShortPayloadRejected(t *testing.T) {
	for n := 0; n < 40; n++ {
		p := make([]byte, n) // 容量恰为 n：越界读即 panic，不会读到底层数组的多余字节
		if n >= 4 {
			putU32(p[0:], 1) // version
		}
		if n >= 8 {
			putU32(p[4:], 48) // headerLen
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("n=%d decodePayload panic: %v", n, r)
				}
			}()
			if _, err := decodePayload(p); !errors.Is(err, errShort) {
				t.Errorf("n=%d err=%v, 期望 %v", n, err, errShort)
			}
		}()
	}

	t.Run("decoder", func(t *testing.T) {
		want := Frame{Player: "kugou", Bins: []float32{0.1}}
		buf := make([]byte, 12)
		putU32(buf[0:], Magic)
		putU32(buf[4:], 4)
		putU32(buf[8:], 1) // version
		var d Decoder
		d.Push(buf)
		d.Push(Encode(nil, want))
		if _, ok, err := d.Next(); ok || !errors.Is(err, errShort) {
			t.Fatalf("短帧 Next ok=%v err=%v, 期望 %v", ok, err, errShort)
		}
		got, ok, err := d.Next()
		if err != nil || !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("短帧后的好帧 Next got=%+v ok=%v err=%v", got, ok, err)
		}
	})
}

// TestForwardCompatExtraHeader：headerLen=56 的帧在偏移 48..56 带 8 字节未知字段，
// 解码器须跳过它们，从 headerLen 处读 bins 与 player。
func TestForwardCompatExtraHeader(t *testing.T) {
	want := Frame{Player: "qqmusic", Flags: FlagNormalized, Epoch: 9, TSMillis: 42.5, RMS: 0.3, PeakL: 0.7, PeakR: 0.8, Bins: []float32{0.1, 0.2, 0.3}}
	enc := Encode(nil, want)
	b := append(bytes.Clone(enc[:48]), bytes.Repeat([]byte{0xAA}, 8)...)
	b = append(b, enc[48:]...)
	putU32(b[4:], uint32(len(b)-8))
	putU32(b[12:], 56)

	var d Decoder
	d.Push(b)
	got, ok, err := d.Next()
	if err != nil || !ok {
		t.Fatalf("Next ok=%v err=%v", ok, err)
	}
	unknown := math.Float32frombits(0xAAAAAAAA)
	for i, v := range got.Bins {
		if v == unknown {
			t.Fatalf("bins[%d] 读到了未知字段 0xAAAAAAAA", i)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestQPCMillis(t *testing.T) {
	const f = int64(10_000_000)
	if got := QPCMillis(123, 0); got != 0 {
		t.Errorf("freq<=0 应得 0, got %v", got)
	}
	if got := QPCMillis(f, f); got != 1000 {
		t.Errorf("整秒换算错: got %v", got)
	}
	if got := QPCMillis(2*f+f/4, f); got != 2250 {
		t.Errorf("含余数换算错: got %v", got)
	}
	if got := QPCMillis(1e14, 1e7); !(math.Abs(got-1e10) < 1e-3) {
		t.Errorf("大计数换算错: got %v want 1e10", got)
	}
	// counter*1000 超出 int64：按整数先乘后除会溢出。
	if got := QPCMillis(1e17, 1e7); !(math.Abs(got-1e13) < 1) {
		t.Errorf("超大计数换算错: got %v want 1e13", got)
	}
	// 频率非 1000 的整数倍：按整数把频率先除以 1000 会截断。
	if got := QPCMillis(3_579_545*5, 3_579_545); !(math.Abs(got-5000) < 1e-6) {
		t.Errorf("非整千频率换算错: got %v want 5000", got)
	}
	const c = int64(1e14)
	if a, b := QPCMillis(c, f), QPCMillis(c+1, f); !(b > a) {
		t.Errorf("大计数下相邻计数须严格递增: %v -> %v", a, b)
	}
}

// TestFragmentedPush：字节被任意切片喂入（模拟 stdout 分片读），应仍解出完整帧。
func TestFragmentedPush(t *testing.T) {
	f := Frame{Player: "wesing", Epoch: 7, TSMillis: -42.5, RMS: 0.9, Bins: []float32{0.1, 0.2, 0.3}}
	enc := Encode(nil, f)
	var d Decoder
	// 逐字节喂：每次只有最后一片喂完才应拿到帧
	for i := 0; i < len(enc); i++ {
		got, ok, err := func() (Frame, bool, error) { d.Push(enc[i : i+1]); return d.Next() }()
		if err != nil {
			t.Fatalf("i=%d 非预期错误: %v", i, err)
		}
		if ok {
			if i != len(enc)-1 {
				t.Fatalf("i=%d 过早解出帧", i)
			}
			if !reflect.DeepEqual(got, f) {
				t.Fatalf("got %+v want %+v", got, f)
			}
		}
	}
}

// TestMultipleFrames：一次缓冲里多帧应依次解出。
func TestMultipleFrames(t *testing.T) {
	frames := []Frame{
		{Player: "kugou", Epoch: 1, TSMillis: 1, RMS: 0.1, Bins: []float32{1, 2}},
		{Player: "", Epoch: 2, TSMillis: 2, RMS: 0.2, Bins: []float32{3}},
		{Player: "qqmusic", Epoch: 3, TSMillis: 3, RMS: 0.3, Bins: []float32{4, 5, 6}},
	}
	var buf []byte
	for _, f := range frames {
		buf = Encode(buf, f)
	}
	var d Decoder
	d.Push(buf)
	for i, want := range frames {
		got, ok, err := d.Next()
		if err != nil || !ok {
			t.Fatalf("帧 %d Next ok=%v err=%v", i, ok, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("帧 %d got %+v want %+v", i, got, want)
		}
	}
	if _, ok, _ := d.Next(); ok {
		t.Fatalf("多余帧")
	}
}

// TestGarbageStreamNoPanic：纯随机垃圾流喂入，Next 必须收敛（不 panic、不死循环、不 OOM）。
func TestGarbageStreamNoPanic(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	junk := make([]byte, 1<<16)
	for i := range junk {
		junk[i] = byte(rng.Intn(256))
	}
	var d Decoder
	d.Push(junk)
	for i := 0; ; i++ {
		if i > len(junk) {
			t.Fatalf("Next 未收敛，疑似死循环")
		}
		_, ok, err := d.Next()
		if !ok && err == nil {
			break // 缓冲吃干、等更多字节
		}
	}
}

// TestGarbageThenGoodFrame：一大段随机垃圾后紧跟好帧，仍能重对齐解出。
func TestGarbageThenGoodFrame(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	junk := make([]byte, 500)
	for i := range junk {
		junk[i] = byte(rng.Intn(256))
	}
	// 剔除垃圾里偶然生成的 magic，保证只有末尾那帧是真帧
	junk = bytes.ReplaceAll(junk, magicBytes, []byte{0, 0, 0, 0})
	good := Encode(nil, Frame{Player: "cloudmusic", Bins: []float32{0.5}})

	var d Decoder
	d.Push(junk)
	d.Push(good)
	var got Frame
	var ok bool
	for i := 0; i < 8 && !ok; i++ {
		got, ok, _ = d.Next()
	}
	if !ok || got.Player != "cloudmusic" {
		t.Fatalf("垃圾后重对齐失败 got=%+v ok=%v", got, ok)
	}
}
