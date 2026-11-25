//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// watch 是 dump 逐拍列出的一个地址：模块内 RVA；RVA 非 4 对齐时列所在 dword 中的那个字节。
type watch struct {
	module string
	rva    uint32
}

func (x watch) String() string { return fmt.Sprintf("%s:0x%X", x.module, x.rva) }

// watchList 实现 flag.Value，-watch 可重复。
type watchList []watch

func (w *watchList) String() string {
	parts := make([]string, len(*w))
	for i, x := range *w {
		parts[i] = x.String()
	}
	return strings.Join(parts, ",")
}

func (w *watchList) Set(s string) error {
	mod, num, ok := strings.Cut(s, ":")
	if !ok || mod == "" {
		return fmt.Errorf("格式应为 模块:0xRVA，如 QQMusic.dll:0xD3BBA8")
	}
	rva, err := strconv.ParseUint(num, 0, 32)
	if err != nil {
		return fmt.Errorf("RVA %q：%w", num, err)
	}
	*w = append(*w, watch{mod, uint32(rva)})
	return nil
}

// watchSeries 返回该地址的取值历史与字节位（-1=整个 dword）。录制期间没变过的 dword 不在
// d.series 里，取录制结束时的节内容作常量。
func watchSeries(h *Header, d *dataset, w watch) (*series, int, error) {
	for i, sec := range h.Sections {
		if !strings.EqualFold(sec.Module, w.module) || w.rva < sec.RVA || w.rva >= sec.RVA+sec.Size {
			continue
		}
		rel := w.rva - sec.RVA
		off, lane := rel&^3, int(rel&3)
		if lane == 0 {
			lane = -1
		}
		if s := d.series[uint64(i)<<32|uint64(off)]; s != nil {
			return s, lane, nil
		}
		if st := d.final[i]; int(off)+4 <= len(st) {
			return &series{init: binary.LittleEndian.Uint32(st[off:])}, lane, nil
		}
		return nil, 0, fmt.Errorf("%s 所在节 %s 从未读成功", w, sec.Name)
	}
	return nil, 0, fmt.Errorf("%s 不在录到的可写节里", w)
}

// dump 按变化次数从少到多列出每个变过的 dword 的取值时间线，供人工对照暂停、恢复与换歌时刻；
// 再把 watches 逐拍列出（每 0.6 s 一行，任一取值或进度变化时另起一行，停走时长与首步由此量出）。
func dump(path string, maxChanges int, watches []watch) error {
	h, d, err := load(path)
	if err != nil {
		return err
	}
	fmt.Printf("QQ 音乐 %s（偏移表 %s），%d 拍；溢出拍 %d，节读失败 %d 次\n", h.Version, h.Offsets, len(d.ts),
		d.overflowTicks, d.failedReads)
	keys := make([]uint64, 0, len(d.series))
	for k, s := range d.series {
		if len(s.ticks) <= maxChanges {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := d.series[keys[i]], d.series[keys[j]]
		if len(a.ticks) != len(b.ticks) {
			return len(a.ticks) < len(b.ticks)
		}
		return keys[i] < keys[j]
	})
	fmt.Printf("共 %d 个变过的 dword，列出变化不超过 %d 次的 %d 个\n", len(d.series), maxChanges, len(keys))
	for _, k := range keys {
		s := d.series[k]
		sec := h.Sections[k>>32]
		off := uint32(k)
		fmt.Printf("\n%s %s+0x%X（模块 RVA 0x%X）init=0x%X 变化 %d 次:", sec.Module, sec.Name, off, sec.RVA+off, s.init, len(s.ticks))
		for i, t := range s.ticks {
			fmt.Printf(" %.1fs=0x%X", float64(d.ts[t])/1000, s.vals[i])
		}
	}
	fmt.Println()

	type col struct {
		w    watch
		s    *series
		lane int
	}
	var cols []col
	for _, w := range watches {
		s, lane, err := watchSeries(h, d, w)
		if err != nil {
			fmt.Println("跳过：", err)
			continue
		}
		cols = append(cols, col{w, s, lane})
	}
	if len(cols) == 0 {
		return nil
	}
	fmt.Println("\n== 逐拍（每 0.6 s 一行，取值或进度变化时另起一行）==")
	var prev []uint32
	last := int64(0)
	for i, t := range d.ts {
		cur := make([]uint32, len(cols))
		for k, c := range cols {
			cur[k] = laneVal(c.s.at(i), c.lane)
		}
		if prev != nil && slices.Equal(cur, prev) && d.prog[i] == d.prog[i-1] && t-last < 600 {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%6.1fs", float64(t)/1000)
		for k, c := range cols {
			fmt.Fprintf(&b, " %s=%#x", c.w, cur[k])
		}
		fmt.Printf("%s 进度=%6d %q\n", b.String(), d.prog[i], d.names[i])
		prev, last = cur, t
	}
	return nil
}
