//go:build windows

package main

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"time"
)

// 录像格式：gzip( gob(Header), gob(Tick), gob(Tick), ... )。

type Section struct {
	Module  string
	ModBase uint64
	Name    string
	RVA     uint32
	Size    uint32
}

type Header struct {
	PID        uint32
	Version    string
	Offsets    string // 读歌名/进度所用偏移表的版本号；与 Version 不同即为距离回退
	Struct1    uint64
	IntervalMs int
	Start      time.Time
	Sections   []Section
}

// Tick 是一拍。Full[i] 非空 = 第 i 节整节内容（首拍与溢出后重同步）；Sec/Off/Val 为相对上一拍
// 变化的 dword；Overflow>0 = 本拍变化过多只记个数（各节随后整节重同步）；Failed = 本拍读失败的节。
type Tick struct {
	T        int64 // 距录制开始的毫秒数
	Name     string
	Prog     uint32
	Full     [][]byte
	Sec      []uint16
	Off      []uint32
	Val      []uint32
	Overflow int
	Failed   []uint16
}

const maxChangesPerTick = 200000

// series 是一个 dword 的取值历史：ticks[j] 起取 vals[j]，之前为 init。
type series struct {
	init  uint32
	ticks []int32
	vals  []uint32
}

func (s *series) at(k int) uint32 {
	j := sort.Search(len(s.ticks), func(j int) bool { return int(s.ticks[j]) > k })
	if j == 0 {
		return s.init
	}
	return s.vals[j-1]
}

// dataset 是解出的录像：每拍的时刻、歌名与进度，以及各节每个变过的 dword 的取值历史。
type dataset struct {
	ts                         []int64
	names                      []string
	prog                       []uint32
	series                     map[uint64]*series // 键 = 节号<<32 | 节内字节偏移；只收变过的 dword
	final                      [][]byte           // [节]：录制结束时的内容，nil=从未读成功；未变过的 dword 取这里
	overflowTicks, failedReads int
}

func (d *dataset) set(tick, sec int, st []byte, off, v uint32) {
	if int(off)+4 > len(st) {
		return
	}
	old := binary.LittleEndian.Uint32(st[off:])
	if old == v {
		return
	}
	key := uint64(sec)<<32 | uint64(off)
	s := d.series[key]
	if s == nil {
		s = &series{init: old}
		d.series[key] = s
	}
	s.ticks = append(s.ticks, int32(tick))
	s.vals = append(s.vals, v)
	binary.LittleEndian.PutUint32(st[off:], v)
}

func load(path string) (*Header, *dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	dec := gob.NewDecoder(gz)
	var h Header
	if err := dec.Decode(&h); err != nil {
		return nil, nil, fmt.Errorf("读文件头: %w", err)
	}
	n := len(h.Sections)
	d := &dataset{series: map[uint64]*series{}}
	state := make([][]byte, n)
	for i := 0; ; i++ {
		var tk Tick // 每拍新建：gob 不会清零流里缺省的字段
		if err := dec.Decode(&tk); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				fmt.Println("注意：文件末尾截断（录制被强行中止？），只用截断前的数据")
				break
			}
			return nil, nil, err
		}
		d.ts = append(d.ts, tk.T)
		d.names = append(d.names, tk.Name)
		d.prog = append(d.prog, tk.Prog)
		if tk.Overflow > 0 {
			d.overflowTicks++
		} else {
			for s, full := range tk.Full {
				if len(full) == 0 || s >= n {
					continue
				}
				if state[s] == nil {
					state[s] = slices.Clone(full)
				} else {
					for off := 0; off+4 <= min(len(full), len(state[s])); off += 4 {
						d.set(i, s, state[s], uint32(off), binary.LittleEndian.Uint32(full[off:]))
					}
				}
			}
			for j := range tk.Sec {
				if s := int(tk.Sec[j]); s < n && state[s] != nil {
					d.set(i, s, state[s], tk.Off[j], tk.Val[j])
				}
			}
		}
		d.failedReads += len(tk.Failed)
	}
	d.final = state
	return &h, d, nil
}

func laneVal(v uint32, lane int) uint32 {
	if lane < 0 {
		return v
	}
	return v >> (8 * lane) & 0xFF
}
