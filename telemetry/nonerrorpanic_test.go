package telemetry

// 本文件只测一件事：**panic 的值不是 error 时，上报里仍然有 Exception 和栈**。
// re-panic 语义在 panicrepanic_test.go，栈裁剪在 stackframetrim_test.go。
//
// # 这条守的是一个实测出来的、违反直觉的坑
//
// Go 允许 panic 任何值。而 sentry-go v0.48.0 对**非 error 的 panic 值不生成 Exception**：
//
//	panic(errors.New("x"))  → Exception{Type, Value, Stacktrace}  ✓
//	panic("x")              → Exception 整个为空，只剩一行 Message，**没有栈**
//	panic(42)               → 同上
//
// 「没有栈」等于没报：Sentry 里会出现一个孤零零的 fatal 事件，说不出是哪一行崩的。
// Guard 因此在上报前把非 error 的值包一层 fmt.Errorf。
//
// 删掉那个包装，四道门禁与其余所有测试**全绿**——因为它们用的都是 error panic。
// 这个文件就是那三行的唯一守卫。

import (
	"strings"
	"testing"
)

func panicWithString() { panic("裸字符串 panic") }
func panicWithInt()    { panic(42) }

// panicWithNilMap 触发一个**真实的运行时 panic**（不是我们手写的 panic）。
//
// 它同时是一条重要的反证：运行时 panic 的值是 runtime.Error，**本身就实现了 error**，
// 所以这一类天然安全（实测得到 runtime.plainError "assignment to entry in nil map"，栈完整）。
// nil 解引用、数组越界同理。包装兜的是代码里裸写 panic("...") 的情况，不是这一类。
func panicWithNilMap() {
	var m map[string]int
	m["boom"] = 1
}

// TestNonErrorPanicStillHasExceptionAndStack 钉死非 error 的 panic 值也能报出可用的 event。
//
// 变异自证：删掉 Guard 里的 `err = fmt.Errorf("%v", r)`（连同它的 if），
// 裸字符串与 int 两条子用例立刻红 —— Exception 会变成空的。
func TestNonErrorPanicStillHasExceptionAndStack(t *testing.T) {
	cases := []struct {
		name      string
		fn        func()
		wantValue string // Exception.Value 里应当出现的内容
		wantFrame string // 栈最内层应当是的函数
	}{
		{"裸字符串", panicWithString, "裸字符串 panic", "panicWithString"},
		{"int", panicWithInt, "42", "panicWithInt"},
		{"真实运行时 panic（nil map 写入）", panicWithNilMap, "nil map", "panicWithNilMap"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := captureGuardEvent(t, c.fn)
			if ev == nil {
				t.Fatal("panic 没有产生上报事件")
			}
			if len(ev.Exception) == 0 {
				t.Fatalf("Exception 为空 —— 没有 type/value/栈，等于没报。"+
					"sentry 对非 error 的 panic 值不生成 Exception，Guard 必须先包成 error。"+
					"（事件只剩 Message=%q）", ev.Message)
			}

			ex := ev.Exception[0]
			if !strings.Contains(ex.Value, c.wantValue) {
				t.Errorf("Exception.Value = %q，应当含 %q", ex.Value, c.wantValue)
			}
			if ex.Stacktrace == nil || len(ex.Stacktrace.Frames) == 0 {
				t.Fatal("Exception 有了但没有栈 —— 定位不到任何东西")
			}
			inner := ex.Stacktrace.Frames[len(ex.Stacktrace.Frames)-1]
			if inner.Function != c.wantFrame {
				t.Errorf("栈最内层 = %q，want %q（真正的 panic 现场）", inner.Function, c.wantFrame)
			}
		})
	}
}
