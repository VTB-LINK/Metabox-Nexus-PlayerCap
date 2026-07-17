package telemetry

// 本文件只测一件事：**Guard 不改变崩溃语义**。
// 栈裁剪在 stackframetrim_test.go。
//
// 这是整个 panic 捕获里唯一不可谈判的性质。官方的 `defer sentry.Recover()` 捕获后**不
// re-panic**，那会让 goroutine 带着未知的损坏状态继续跑——把错位歌词持续推上 OBS，且骗过
// 所有人（进程还活着，没有任何信号说该重启）。「假装没崩」比「崩掉」更坏。
//
// 所以下面每一条都在守同一句话：**该崩还是崩，只是顺便报一声。**

import (
	"errors"
	"testing"
)

// TestGuardRepanics 钉死 Guard 必须把 panic 抛回去。
//
// 变异自证：删掉 Guard 里的 `panic(r)` 即红——那正是官方 sentry.Recover() 的行为，
// 也正是我们拒绝它的理由。
func TestGuardRepanics(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = "" // 禁用上报：本测试只关心语义，不碰 sentry

	defer func() {
		r := recover()
		if r == nil {
			t.Error("Guard 把 panic 吞了 —— 进程本该死，现在它会带着损坏状态继续跑，" +
				"而且没有任何信号提示该重启")
			return
		}
		err, ok := r.(error)
		if !ok || err.Error() != "boom" {
			t.Errorf("re-panic 的值变了: %#v，want error(\"boom\")", r)
		}
	}()

	func() {
		defer Guard()
		panic(errors.New("boom"))
	}()

	t.Error("panic 没有穿过 Guard —— 走到这一行本身就是失败")
}

// TestGuardRepanicsOriginalValue 钉死 re-panic 抛的是**原始值**，不是 report 包装后的 error。
//
// report 会把非 error 的 panic 值包成 error（否则 sentry 生成不出 Exception，见 panic.go）。
// 那个包装**只能用于上报**，绝不能泄漏到 panic 语义里——上游若有 recover 按类型分支处理，
// 值被换成 *errors.errorString 会让它认不出来。
//
// 变异自证：把 Guard 里的 `panic(r)` 改成 `panic(fmt.Errorf("%v", r))` 即红。
func TestGuardRepanicsOriginalValue(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = ""

	defer func() {
		r := recover()
		s, ok := r.(string)
		if !ok {
			t.Errorf("re-panic 的值被改成了 %T(%#v) —— 必须原样抛回。"+
				"上报用的包装不许改变 panic 的类型", r, r)
			return
		}
		if s != "裸字符串 panic" {
			t.Errorf("re-panic 的值 = %q，want \"裸字符串 panic\"", s)
		}
	}()

	func() {
		defer Guard()
		panic("裸字符串 panic")
	}()

	t.Error("panic 没有穿过 Guard")
}

// TestGuardWithoutPanicIsNoop 没有 panic 时 Guard 必须什么都不做。
//
// 它挂在每个 goroutine 入口，正常退出时也会被执行——那条路径上不能有任何动作。
func TestGuardWithoutPanicIsNoop(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = ""

	done := func() (ok bool) {
		defer Guard()
		return true
	}()

	if !done {
		t.Error("正常返回的函数被 Guard 干扰了")
	}
}

// TestGuardIsUsableAsDirectDefer 钉死 Guard 能作为 `defer Guard()` 直接使用。
//
// 这看着像同义反复，其实不是：它锁死了 Guard 的签名——无参、无返回值。
// 一旦有人给它加参数或返回值，`defer telemetry.Guard()` 这个用法在全仓所有调用点同时失效，
// 而这个测试会先一步红。
//
// 注意反面：`defer func(){ Guard() }()` 多包一层后 recover() 返回 nil，上报静默失效而门禁
// 全绿——那条靠 guarddirect_test.go 的 AST 门禁守，测试守不到。
func TestGuardIsUsableAsDirectDefer(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = ""

	var f func() = Guard // 编译期钉死签名
	_ = f

	defer func() {
		if r := recover(); r == nil {
			t.Error("直接 defer Guard() 没能捕获并抛回 panic")
		}
	}()
	func() {
		defer Guard()
		panic("x")
	}()
}
