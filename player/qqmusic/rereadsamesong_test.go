package qqmusic

// 本文件只测 rereadSameSong —— 取词之后重读到的元数据能否当作本轮的读数。
//
// 轮询循环（runSession）没有离线夹具：这里只守判据本身，循环里采用之后替换了哪些量、位置取的是
// 哪个读数，靠读代码与真机；两处调用传的参照由 switchzero_test.go 的源码扫描守形状。
//
// 变异均在仓库的一份分离 worktree 副本里逐条注入、跑完即还原。

import (
	"errors"
	"testing"
)

func TestRereadSameSong(t *testing.T) {
	readErr := errors.New("process not attached")
	cases := []struct {
		name   string
		fresh  *SongMetadata
		err    error
		song   string
		prevMs uint32
		want   bool
	}{
		// 变异自证：判据改为恒假、或把相等改成不等，本例红（实测）。
		{"读成功且歌名仍是已认领的那首：采用", &SongMetadata{Name: "晴天", ProgressMs: 1500}, nil, "晴天", 0, true},
		{"歌名相同、歌手不同：循环只按歌名判换歌，这里同样采用", &SongMetadata{Name: "晴天", Singer: "翻唱者"}, nil, "晴天", 0, true},
		// 变异自证：去掉 `err == nil`，本例红（实测）。
		{"读出错但带回了同名的元数据：不采用", &SongMetadata{Name: "晴天"}, readErr, "晴天", 0, false},
		{"读出错、没有元数据：不采用", nil, readErr, "晴天", 0, false},
		// 变异自证：去掉 `fresh != nil`，本例因空指针解引用而崩（实测；上一例有 err 判断挡在前面，不崩）。
		{"没有元数据也没有报错：不采用", nil, nil, "晴天", 0, false},
		// 变异自证：去掉 `fresh.Name == name`，这三例与下一例红（实测）。
		{"取词期间又换了歌：不采用", &SongMetadata{Name: "七里香"}, nil, "晴天", 0, false},
		{"读到空闲态的占位串：不采用", &SongMetadata{Name: "QQ音乐 听我想听"}, nil, "晴天", 0, false},
		{"读到空歌名：不采用", &SongMetadata{Name: ""}, nil, "晴天", 0, false},
		// 变异自证：改用 strings.HasPrefix(fresh.Name, name) 比较，本例红（实测）。
		{"歌名只是前缀相同：按不同处理", &SongMetadata{Name: "晴天 (Live)"}, nil, "晴天", 0, false},
		// 变异自证：去掉 `name != ""`，本例红（实测）。
		{"尚未认领任何歌、读到的歌名也为空：不算同一首", &SongMetadata{Name: ""}, nil, "", 0, false},

		// 以下是 #202 的判据：相对 prevMs（调用方传 lastMemProgress）构成暂扣的触发即不采用
		// （doc/design/qqmusic-switch-zero-design.md §4.6、§7.2 U3），歌名都与已认领的相同、读成功。
		// 变异自证（均实测）：去掉 `!startsHold(...)`，不采用的四例红；判据改为一律不采用，采用的六例与上面采用的两例红；
		// 改为 `!seekBack(...)`（去掉带宽），「带宽边界」「带外回跳」两例红；startsHold 的带宽改为无穷大，同这两例红；
		// 带宽比较含等号，「带宽边界」红；startsHold 不要求构成回跳，「恰好不构成回跳」「回退不足 2 s」两例红；seekBack
		// 改为 `<=`，「恰好不构成回跳」红。
		{"设计审 R1 的反例：上一次读数 5000、重读 0（取词期间换歌归零）：不采用", &SongMetadata{Name: "晴天", ProgressMs: 0}, nil, "晴天", 5000, false},
		{"带内上沿 999：不采用", &SongMetadata{Name: "晴天", ProgressMs: 999}, nil, "晴天", 80000, false},
		{"带宽边界 1000：采用", &SongMetadata{Name: "晴天", ProgressMs: 1000}, nil, "晴天", 80000, true},
		{"带外回跳（取词期间拖到中段）：采用", &SongMetadata{Name: "晴天", ProgressMs: 30000}, nil, "晴天", 80000, true},
		{"恰好构成回跳 2001 → 0：不采用", &SongMetadata{Name: "晴天", ProgressMs: 0}, nil, "晴天", 2001, false},
		{"恰好不构成回跳 2000 → 0：采用", &SongMetadata{Name: "晴天", ProgressMs: 0}, nil, "晴天", 2000, true},
		{"回退不足 2 s：采用", &SongMetadata{Name: "晴天", ProgressMs: 200}, nil, "晴天", 1500, true},
		{"带内 300、上一次读数 80000：不采用", &SongMetadata{Name: "晴天", ProgressMs: 300}, nil, "晴天", 80000, false},
		{"正常前进：采用", &SongMetadata{Name: "晴天", ProgressMs: 5100}, nil, "晴天", 5000, true},
		{"接着走 80400、上一次读数 80000：采用", &SongMetadata{Name: "晴天", ProgressMs: 80400}, nil, "晴天", 80000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rereadSameSong(c.fresh, c.err, c.song, c.prevMs); got != c.want {
				t.Fatalf("rereadSameSong(%+v, %v, %q, %d) = %v，期望 %v", c.fresh, c.err, c.song, c.prevMs, got, c.want)
			}
		})
	}
}
