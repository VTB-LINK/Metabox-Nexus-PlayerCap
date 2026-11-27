package procscan

import "testing"

func TestMainPID_ChromiumStyle(t *testing.T) {
	// 主进程父为 explorer（非同名），两个子进程父指向主进程。
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 200, Name: "cloudmusic.exe", PPID: 100},
		{PID: 300, Name: "cloudmusic.exe", PPID: 200},
		{PID: 400, Name: "cloudmusic.exe", PPID: 200},
	}
	got, ok := MainPID(procs, "cloudmusic.exe")
	if !ok || got != 200 {
		t.Fatalf("MainPID = %d, %v; want 200, true", got, ok)
	}
}

func TestMainPID_Single(t *testing.T) {
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 555, Name: "WeSing.exe", PPID: 100},
	}
	got, ok := MainPID(procs, "WeSing.exe")
	if !ok || got != 555 {
		t.Fatalf("MainPID = %d, %v; want 555, true", got, ok)
	}
}

func TestMainPID_MultipleMainsPicksLowestPID(t *testing.T) {
	// 两个独立主进程（父都非同名）→ 取 PID 最小；950 是 700 的子进程应被排除。
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 900, Name: "qqmusic.exe", PPID: 100},
		{PID: 700, Name: "qqmusic.exe", PPID: 100},
		{PID: 950, Name: "qqmusic.exe", PPID: 700},
	}
	got, ok := MainPID(procs, "qqmusic.exe")
	if !ok || got != 700 {
		t.Fatalf("MainPID = %d, %v; want 700, true", got, ok)
	}
}

func TestMainPID_NoMatch(t *testing.T) {
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 200, Name: "chrome.exe", PPID: 100},
	}
	got, ok := MainPID(procs, "KuGou.exe")
	if ok || got != 0 {
		t.Fatalf("MainPID = %d, %v; want 0, false", got, ok)
	}
}

func TestMainPID_CaseInsensitive(t *testing.T) {
	// 子进程用不同大小写的同名，也须被识别为子进程而排除；返回大小写不敏感命中的主进程。
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 210, Name: "QQMusic.exe", PPID: 100},
		{PID: 220, Name: "qqmusic.exe", PPID: 210},
	}
	got, ok := MainPID(procs, "qqmusic.exe")
	if !ok || got != 210 {
		t.Fatalf("MainPID = %d, %v; want 210, true", got, ok)
	}
}

func TestMainPID_EmptyName(t *testing.T) {
	procs := []Proc{
		{PID: 10, Name: "", PPID: 1},
		{PID: 100, Name: "KuGou.exe", PPID: 4},
	}
	if got, ok := MainPID(procs, ""); ok || got != 0 {
		t.Fatalf("MainPID(procs, \"\") = %d, %v; want 0, false", got, ok)
	}
}

// TestMainPIDAny 守护跨进程名家族的主进程根识别（如酷狗 HD_KuGou.exe 宿主与 KuGou.exe CEF 子进程）：
// 所有白名单名称必须合入同一家族集合再排除子进程，严禁分别求 MainPID 或做模糊匹配。
//
// 变异自证（实测变红）：MainPIDAny 不把多名字合并进同一 same 集合而分别求主进程再取最小 PID →
// 真机拓扑三个子用例均返回 15876（期望 25940），大小写不敏感跨名父子族返回 2000（期望 8000）。
func TestMainPIDAny(t *testing.T) {
	realKuGouTopo := []Proc{
		{PID: 3532, Name: "explorer.exe", PPID: 1000},
		{PID: 25940, Name: "HD_KuGou.exe", PPID: 3532},
		{PID: 15876, Name: "KuGou.exe", PPID: 25940}, // 更小 PID 的子进程，绝不能压过主进程 25940
		{PID: 19012, Name: "KuGou.exe", PPID: 25940},
		{PID: 26100, Name: "KuGou.exe", PPID: 25940},
		{PID: 26204, Name: "KuGou.exe", PPID: 25940},
		{PID: 26308, Name: "KuGou.exe", PPID: 25940},
	}

	cases := []struct {
		name   string
		procs  []Proc
		names  []string
		want   uint32
		wantOK bool
	}{
		{
			name:   "真机拓扑_HD_KuGou主进程与5个含更小PID的KuGou子进程",
			procs:  realKuGouTopo,
			names:  []string{"HD_KuGou.exe", "KuGou.exe"},
			want:   25940,
			wantOK: true,
		},
		{
			name:   "真机拓扑_参数顺序无关",
			procs:  realKuGouTopo,
			names:  []string{"KuGou.exe", "HD_KuGou.exe"},
			want:   25940,
			wantOK: true,
		},
		{
			name:   "真机拓扑_夹带空串参数被忽略",
			procs:  realKuGouTopo,
			names:  []string{"", "HD_KuGou.exe", "", "KuGou.exe"},
			want:   25940,
			wantOK: true,
		},
		{
			name: "传统拓扑_纯KuGou主进程与更小PID子进程",
			procs: []Proc{
				{PID: 1000, Name: "explorer.exe", PPID: 4},
				{PID: 5000, Name: "KuGou.exe", PPID: 1000},
				{PID: 3000, Name: "KuGou.exe", PPID: 5000},
			},
			names:  []string{"HD_KuGou.exe", "KuGou.exe"},
			want:   5000,
			wantOK: true,
		},
		{
			name: "大小写不敏感跨名父子族",
			procs: []Proc{
				{PID: 100, Name: "explorer.exe", PPID: 4},
				{PID: 8000, Name: "hd_kugou.EXE", PPID: 100},
				{PID: 2000, Name: "KUGOU.exe", PPID: 8000},
			},
			names:  []string{"HD_KuGou.exe", "KuGou.exe"},
			want:   8000,
			wantOK: true,
		},
		{
			name: "全成环兜底取最小PID",
			procs: []Proc{
				{PID: 500, Name: "HD_KuGou.exe", PPID: 400},
				{PID: 400, Name: "KuGou.exe", PPID: 500},
			},
			names:  []string{"HD_KuGou.exe", "KuGou.exe"},
			want:   400,
			wantOK: true,
		},
		{
			name: "反例_近似进程名不匹配",
			procs: []Proc{
				{PID: 100, Name: "KuGouConcept.exe", PPID: 4},
				{PID: 200, Name: "HD_KuGou2.exe", PPID: 4},
				{PID: 300, Name: "Fanxing_KuGou.exe", PPID: 4},
				{PID: 400, Name: "my_kugou.exe", PPID: 4},
			},
			names:  []string{"HD_KuGou.exe", "KuGou.exe"},
			want:   0,
			wantOK: false,
		},
		{
			name:   "反例_空名切片",
			procs:  realKuGouTopo,
			names:  nil,
			want:   0,
			wantOK: false,
		},
		{
			name: "反例_全空串切片不匹配空名进程",
			procs: []Proc{
				{PID: 10, Name: "", PPID: 1},
				{PID: 25940, Name: "HD_KuGou.exe", PPID: 3532},
			},
			names:  []string{"", ""},
			want:   0,
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := MainPIDAny(c.procs, c.names...)
			if got != c.want || ok != c.wantOK {
				t.Fatalf("MainPIDAny(%v) = (%d, %v), want (%d, %v)", c.names, got, ok, c.want, c.wantOK)
			}
		})
	}
}
