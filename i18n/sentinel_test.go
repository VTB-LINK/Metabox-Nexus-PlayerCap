package i18n

import (
	"errors"
	"fmt"
	"testing"
)

// Sentinel 的文案随 Error() 被调用时的语言变化，按身份比较。
//
// 变异自证：Error() 改为返回构造时的 T(msg)（即冻结），英文一支红；Sentinel 改为返回值类型
// sentinel（同文案即相等），「同文案的另一个」一例红。
func TestSentinel(t *testing.T) {
	defer SetLanguage(Language())
	SetLanguage(Chinese)
	err := Sentinel("Kuwo debug port is not ready yet")

	SetLanguage(English)
	if got := err.Error(); got != "Kuwo debug port is not ready yet" {
		t.Errorf("英文下 Error() = %q", got)
	}
	if got := fmt.Errorf("wrap: %w", err).Error(); got != "wrap: Kuwo debug port is not ready yet" {
		t.Errorf("英文下包一层 = %q", got)
	}
	SetLanguage(Chinese)
	if got := err.Error(); got != "酷我调试端口尚未就绪" {
		t.Errorf("中文下 Error() = %q", got)
	}
	if !errors.Is(fmt.Errorf("wrap: %w", err), err) {
		t.Error("包一层之后 errors.Is 认不出")
	}
	if errors.Is(err, Sentinel("Kuwo debug port is not ready yet")) {
		t.Error("同文案的另一个 sentinel 被认作同一个")
	}
}
