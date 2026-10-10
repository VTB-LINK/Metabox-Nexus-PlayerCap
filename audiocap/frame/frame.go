// Package frame 定义 helper→主服务 stdout 的自包含音频帧线格式（v1）及其增量解码器，并提供
// 帧时间戳所用的 QPC 毫秒换算。纯 Go、无平台依赖，可在 CI 运行（布局与校验见
// doc/design/audio-waveform-peak-design.md §3，分帧重对齐见 audio-waveform-capture-design.md §2.5）。
//
// 线格式 v1（全部 encoding/binary.LittleEndian，无对齐填充；括号内为帧内偏移）：
//
//	magic(0,u32) | payloadLen(4,u32) | version(8,u32) | headerLen(12,u32) | flags(16,u32) |
//	epoch(20,u32) | ts(24,f64) | rms(32,f32) | peakL(36,f32) | peakR(40,f32) | playerLen(44,u32) |
//	bins(headerLen,f32×N) | player(headerLen+4N)
//
// payloadLen 为其后的全部字节数，帧长 = 8 + payloadLen；N = (帧长 − headerLen − playerLen) / 4。
// headerLen 是定长头总长（v1 为 48，恒为 4 的倍数）：兼容的新增字段追加在 playerLen 之后并增大
// headerLen，version 不变；解码器跳过 48..headerLen 之间不认识的字段。不兼容的改动才升 version。
// magic 是 desync 后按魔数重对齐的锚；payloadLen 有上限 MaxPayload，越界即判坏帧丢弃防 OOM。
//
// 解码器把 helper 的 stdout 当**不可信数据**：坏字节、半帧、超长前缀、越界内部长度均只丢帧不 panic、
// 能对齐到下一个完整帧（§2.5「把污染降级为丢几帧后自恢复」）。
package frame

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Magic 是分帧同步魔数，供 desync 后重对齐。按小端写出后，线上字节序列为 57 41 54 56，即 ASCII "WATV"。
const Magic uint32 = 0x56544157

// MaxPayload 是 payloadLen 上限。超过即判坏帧丢弃，防被杂散字节骗成巨额分配（OOM）。
const MaxPayload = 1 << 16

// Version 是本包编码与接受的线格式版本。
const Version uint32 = 1

// HeaderLen 是 v1 定长头总长，即 bins 在帧内的起始偏移。
const HeaderLen = 48

// FlagNormalized 是 flags 的 bit0：本路已启用音量归一化。其余位保留，发送方置 0，接收方忽略。
const FlagNormalized uint32 = 1 << 0

// prefixLen 是 magic(u32)+payloadLen(u32)，Decoder 分帧用。
const prefixLen = 8

// 坏帧哨兵错误，每条 §3 校验一个；Next 返回的错误可用 errors.Is 区分。
var (
	errShort         = errors.New("帧长小于定长头")
	errVersion       = errors.New("version 不受支持")
	errHeaderShort   = errors.New("headerLen 小于定长头")
	errHeaderAlign   = errors.New("headerLen 非 4 的倍数")
	errHeaderOverrun = errors.New("headerLen 大于帧长")
	errPlayerOverrun = errors.New("headerLen+playerLen 大于帧长")
	errBinsAlign     = errors.New("bins 字节数非 4 的倍数")
)

// magicBytes 是 Magic 的小端字节串，用于在缓冲里定位/重对齐。
var magicBytes = binary.LittleEndian.AppendUint32(nil, Magic)

// Frame 是一帧自包含波形数据。
type Frame struct {
	Player   string  // 播放器名；空串=root 空闲帧（无活跃播放器）
	Flags    uint32  // FlagNormalized 等标志位；空闲帧为 0
	Epoch    uint32  // 每次捕获激活取一个全局计数值，不保证单调，helper 重启后从 1 重新计数；空闲帧为 0
	TSMillis float64 // 开机起算的毫秒数，来源为系统 QPC；与墙钟无关，仅用于帧间相对时间
	RMS      float32 // 该窗均方根
	PeakL    float32 // 左声道（声道 0）sample peak
	PeakR    float32 // 右声道（声道 1）sample peak
	Bins     []float32
}

// Encode 把 f 按 v1 布局追加编码到 dst 后返回新 slice。
// 编码侧是可信输入（player 为播放器名、bins 数受 fftSize 约束），故不校验长度上限。
func Encode(dst []byte, f Frame) []byte {
	le := binary.LittleEndian
	payloadLen := HeaderLen - prefixLen + 4*len(f.Bins) + len(f.Player)

	dst = le.AppendUint32(dst, Magic)
	dst = le.AppendUint32(dst, uint32(payloadLen))
	dst = le.AppendUint32(dst, Version)
	dst = le.AppendUint32(dst, HeaderLen)
	dst = le.AppendUint32(dst, f.Flags)
	dst = le.AppendUint32(dst, f.Epoch)
	dst = le.AppendUint64(dst, math.Float64bits(f.TSMillis))
	dst = le.AppendUint32(dst, math.Float32bits(f.RMS))
	dst = le.AppendUint32(dst, math.Float32bits(f.PeakL))
	dst = le.AppendUint32(dst, math.Float32bits(f.PeakR))
	dst = le.AppendUint32(dst, uint32(len(f.Player)))
	for _, b := range f.Bins {
		dst = le.AppendUint32(dst, math.Float32bits(b))
	}
	return append(dst, f.Player...)
}

// Decoder 从字节流增量解帧，能在坏字节后按 Magic 重同步。零值可用。
type Decoder struct {
	buf []byte
}

// Push 喂入从 stdout 读到的字节（任意切片边界）。
func (d *Decoder) Push(b []byte) {
	d.buf = append(d.buf, b...)
}

// Next 取一帧。
//
//	(frame, true, nil)   成功解出一帧
//	(_, false, err)      发现 Magic 但其后是坏帧（超长/版本不符/内部越界），已跳过该 Magic，调用方记日志后继续调 Next
//	(_, false, nil)      缓冲不足（半帧或找不到 Magic），等 Push 更多字节
//
// 任何输入都不 panic、不 OOM。
func (d *Decoder) Next() (Frame, bool, error) {
	// 定位 Magic。找不到就丢弃到末尾，仅保留可能跨 Push 边界的半个 Magic（末 len-1 字节）。
	i := bytes.Index(d.buf, magicBytes)
	if i < 0 {
		if keep := len(magicBytes) - 1; len(d.buf) > keep {
			d.buf = d.buf[len(d.buf)-keep:]
		}
		return Frame{}, false, nil
	}
	if i > 0 {
		d.buf = d.buf[i:] // 静默丢弃 Magic 前的垃圾，重对齐
	}

	if len(d.buf) < prefixLen {
		return Frame{}, false, nil // 前缀不全，等更多
	}
	payloadLen := binary.LittleEndian.Uint32(d.buf[4:prefixLen])
	if payloadLen > MaxPayload {
		d.buf = d.buf[len(magicBytes):] // 跳过这个 Magic，去找下一个
		return Frame{}, false, fmt.Errorf("frame: payloadLen %d 超过上限 %d，判坏帧", payloadLen, MaxPayload)
	}

	total := prefixLen + int(payloadLen)
	if len(d.buf) < total {
		return Frame{}, false, nil // 整帧未到齐，等更多
	}

	f, err := decodePayload(d.buf[prefixLen:total])
	if err != nil {
		d.buf = d.buf[len(magicBytes):] // 坏帧：跳过 Magic 继续找下一个
		return Frame{}, false, err
	}
	d.buf = d.buf[total:]
	return f, true, nil
}

// decodePayload 解 payload（magic 与 payloadLen 之后的字节，长度恰为 payloadLen），按 spec §3
// 「校验」的顺序逐项判定，任一不成立即返回包装了对应哨兵的 err，由调用方降级为坏帧。
func decodePayload(p []byte) (Frame, error) {
	le := binary.LittleEndian
	frameLen := int64(prefixLen + len(p))
	// 先于任何字段读取：此后定长头内的偏移均不越界。
	if frameLen < HeaderLen {
		return Frame{}, fmt.Errorf("frame: 帧长 %d: %w", frameLen, errShort)
	}
	// at 把帧内偏移换成 p 内偏移。
	at := func(off int) []byte { return p[off-prefixLen:] }

	if v := le.Uint32(at(8)); v != Version {
		return Frame{}, fmt.Errorf("frame: version %d: %w", v, errVersion)
	}
	// 按 int64 比较：两个 u32 相加不回绕。
	hdr := int64(le.Uint32(at(12)))
	playerLen := int64(le.Uint32(at(44)))
	switch {
	case hdr < HeaderLen:
		return Frame{}, fmt.Errorf("frame: headerLen %d: %w", hdr, errHeaderShort)
	case hdr%4 != 0:
		return Frame{}, fmt.Errorf("frame: headerLen %d: %w", hdr, errHeaderAlign)
	case hdr > frameLen:
		return Frame{}, fmt.Errorf("frame: headerLen %d，帧长 %d: %w", hdr, frameLen, errHeaderOverrun)
	case hdr+playerLen > frameLen:
		return Frame{}, fmt.Errorf("frame: headerLen %d，playerLen %d，帧长 %d: %w", hdr, playerLen, frameLen, errPlayerOverrun)
	case (frameLen-hdr-playerLen)%4 != 0:
		return Frame{}, fmt.Errorf("frame: bins 字节数 %d: %w", frameLen-hdr-playerLen, errBinsAlign)
	}

	f := Frame{
		Flags:    le.Uint32(at(16)),
		Epoch:    le.Uint32(at(20)),
		TSMillis: math.Float64frombits(le.Uint64(at(24))),
		RMS:      math.Float32frombits(le.Uint32(at(32))),
		PeakL:    math.Float32frombits(le.Uint32(at(36))),
		PeakR:    math.Float32frombits(le.Uint32(at(40))),
	}
	// 从 headerLen 处读 bins 与 player，跳过 48..headerLen 之间不认识的字段。
	n := int((frameLen - hdr - playerLen) / 4)
	rest := at(int(hdr))
	if n > 0 {
		f.Bins = make([]float32, n)
		for i := range f.Bins {
			f.Bins[i] = math.Float32frombits(le.Uint32(rest[i*4:]))
		}
	}
	f.Player = string(rest[4*n : 4*n+int(playerLen)])
	return f, nil
}

// QPCMillis 把 QueryPerformanceCounter 计数按频率换算成开机起算的毫秒数，freq<=0 返回 0（换算
// 不可用）。helper 真帧与 manager 空闲帧共用本函数，二者时间基一致。counter < 2^53 时 float64(counter)
// 精确，在 10MHz 频率下约合 28 年开机时长。
func QPCMillis(counter, freq int64) float64 {
	if freq <= 0 {
		return 0
	}
	return float64(counter) * 1000 / float64(freq)
}
