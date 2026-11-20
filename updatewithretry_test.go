package main

// 本文件只测 updateWithRetry：自动更新失败后报错、询问、按选择重来或返回的编排（#216，设计 §4.9）。
// perform、report、ask 都是桩，不联网、不碰文件。

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// 变异自证（实测）：
//   - *placeError 也询问（去掉放置失败的判断）→「放置失败」「包了一层的放置失败」「重试后放置失败」多出一次 ask，红；
//   - 判放置失败改用类型断言 err.(*placeError) →「包了一层的放置失败」多出一次 ask，红；
//   - ask 返回 true 时不重来、直接返回错误 →「失败一次后重试成功」「重试后放置失败」「连续失败」少了后续的 perform，红；
//   - ask 返回 false 时仍重来 →「选择退出」「连续失败」多出一次 perform，桩报调用过多，红；
//   - 失败时不调 report → 每个失败的用例都少了 r，红；
//   - 先 ask 后 report → 调用序列为 p a r，红；
//   - report 总是拿第一次的 tried →「重试后放置失败」「连续失败」后几次的 tried 不符，红；
//   - 重试成功后仍返回上一次的错误 →「失败一次后重试成功」返回非 nil，红。
func TestUpdateWithRetry(t *testing.T) {
	a := updateSource{Name: "a.example", Prefix: "https://a.example/releases/download/"}
	b := updateSource{Name: "b.example", Prefix: "https://b.example/releases/download/"}
	e1, e2, e3 := errors.New("download 1"), errors.New("download 2"), errors.New("download 3")
	placed := &placeError{errors.New("place")}
	wrapped := fmt.Errorf("wrapped: %w", placed)

	type attempt struct {
		tried []updateSource
		err   error
	}
	describe := func(as []attempt) string {
		var s []string
		for _, at := range as {
			var names []string
			for _, src := range at.tried {
				names = append(names, src.Name)
			}
			s = append(s, fmt.Sprintf("%v: %v", names, at.err))
		}
		return "[" + strings.Join(s, "; ") + "]"
	}
	tests := []struct {
		name     string
		attempts []attempt // perform 第 i 次的返回
		answers  []bool    // ask 第 i 次的返回
		want     error
		calls    string // 调用序列：p 为 perform，r 为 report，a 为 ask
	}{
		{
			name:     "首次成功",
			attempts: []attempt{{[]updateSource{a}, nil}},
			want:     nil,
			calls:    "p",
		},
		{
			name:     "失败一次后重试成功",
			attempts: []attempt{{[]updateSource{a}, e1}, {[]updateSource{b}, nil}},
			answers:  []bool{true},
			want:     nil,
			calls:    "p r a p",
		},
		{
			name:     "选择退出",
			attempts: []attempt{{[]updateSource{a, b}, e1}},
			answers:  []bool{false},
			want:     e1,
			calls:    "p r a",
		},
		{
			name:     "放置失败",
			attempts: []attempt{{[]updateSource{a}, placed}},
			want:     placed,
			calls:    "p r",
		},
		{
			name:     "包了一层的放置失败",
			attempts: []attempt{{[]updateSource{a}, wrapped}},
			want:     wrapped,
			calls:    "p r",
		},
		{
			name:     "重试后放置失败",
			attempts: []attempt{{[]updateSource{a}, e1}, {[]updateSource{b}, placed}},
			answers:  []bool{true},
			want:     placed,
			calls:    "p r a p r",
		},
		{
			name:     "连续失败",
			attempts: []attempt{{[]updateSource{a}, e1}, {[]updateSource{a, b}, e2}, {nil, e3}},
			answers:  []bool{true, true, false},
			want:     e3,
			calls:    "p r a p r a p r a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			var reports []attempt
			attempts, answers := tt.attempts, tt.answers
			perform := func() ([]updateSource, error) {
				calls = append(calls, "p")
				if len(attempts) == 0 {
					t.Fatalf("perform 调用过多，调用序列 %v", calls)
				}
				at := attempts[0]
				attempts = attempts[1:]
				return at.tried, at.err
			}
			report := func(tried []updateSource, err error) {
				calls = append(calls, "r")
				reports = append(reports, attempt{tried, err})
			}
			ask := func() bool {
				calls = append(calls, "a")
				if len(answers) == 0 {
					t.Fatalf("ask 调用过多，调用序列 %v", calls)
				}
				ans := answers[0]
				answers = answers[1:]
				return ans
			}

			if err := updateWithRetry(perform, report, ask); err != tt.want {
				t.Errorf("返回 %v，应为 %v", err, tt.want)
			}
			if got := strings.Join(calls, " "); got != tt.calls {
				t.Errorf("调用序列 %q，应为 %q", got, tt.calls)
			}
			// 每次 report 拿到的是该次失败的 tried 与错误。
			var want []attempt
			for _, at := range tt.attempts {
				if at.err != nil {
					want = append(want, at)
				}
			}
			if !reflect.DeepEqual(reports, want) {
				t.Errorf("report 收到 %s，应为 %s", describe(reports), describe(want))
			}
		})
	}
}
