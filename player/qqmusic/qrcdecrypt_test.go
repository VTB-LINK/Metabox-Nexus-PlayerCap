package qqmusic

// 本文件只测 QRC 解密链路：qrcDecrypt 的两类失败必须可区分（「不是密文」按明文处理 vs
// 「是密文但解不开」报错），以及 parseLRC 对密文/明文的处理。不测取词请求本身。

import (
	"errors"
	"strings"
	"testing"
)

// TestParseLRCTurnsCiphertextIntoLyric 记录**危害机制本身**：parseLRC 拿到一段
// 无换行、无时间戳的 hex 密文时，会把它整段当成一行歌词。
//
// 这不是 parseLRC 的 bug —— 对真正的无时间戳纯文本歌词，这个「按时长均分」的兜底
// 是有意为之。问题在上游：decryptIfNeeded 若把解密失败的密文放行，就会命中这里。
// 本测试钉住这条因果链，防止有人在「修」上游时误以为 parseLRC 会自己挡住密文。
func TestParseLRCTurnsCiphertextIntoLyric(t *testing.T) {
	ciphertext := strings.Repeat("a1b2c3d4e5f6", 20) // 240 字符纯 hex，无换行

	lines, _, _ := parseLRC(ciphertext, 180000)

	if len(lines) != 1 {
		t.Fatalf("预期密文被当作 1 行，实得 %d 行", len(lines))
	}
	if lines[0].Text != ciphertext {
		t.Errorf("预期整段密文成为歌词文本")
	}
	t.Logf("已确认：%d 字符的 hex 密文 → 1 行歌词 @ %.1fs —— 若上游放行密文，这就是 OBS 上显示的东西",
		len(ciphertext), lines[0].Time)
}

// TestDecryptIfNeededRejectsBadCiphertext 钉死：**确实是密文但解不开**时必须返回
// error，绝不能把密文当明文返回。这是上一条测试所述危害的唯一防线。
//
// 注意用的样本是「合法 hex、长度为 8 的倍数」——即 qrcDecrypt 会走完 hex 与分组
// 校验、真正进入 3DES/zlib 才失败。这才是「是密文但坏了」。若样本连 hex 都解不开，
// 走的是 errNotCiphertext 放行分支，测不到这里要测的东西。
func TestDecryptIfNeededRejectsBadCiphertext(t *testing.T) {
	garbage := strings.Repeat("deadbeef", 40) // 320 字符合法 hex → 160 字节 → 8 的倍数
	if len(garbage)%16 != 0 {
		t.Fatalf("样本设计失误：hex 长度 %d 解出的字节数必须是 8 的倍数", len(garbage))
	}

	for _, c := range []struct {
		name  string
		crypt int
	}{
		{"crypt=1 显式声明加密", 1},
		{"crypt=0 但内容像 hex（启发式命中）", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, didDecrypt, err := decryptIfNeeded(garbage, c.crypt)
			if err == nil {
				t.Fatalf("是密文但解不开，却返回 nil error —— 密文会被当歌词推上 OBS（out 长度=%d, didDecrypt=%v）",
					len(out), didDecrypt)
			}
			if errors.Is(err, errNotCiphertext) {
				t.Errorf("合法 hex 且长度对齐，不该被判为 errNotCiphertext: %v", err)
			}
			if out != "" || didDecrypt {
				t.Errorf("失败时必须返回空串且 didDecrypt=false，实得 len=%d didDecrypt=%v", len(out), didDecrypt)
			}
		})
	}
}

// TestPlaintextWithCryptFlagStillWorks 钉死一条**我曾经引入过的回归**。
//
// b4d9fda 把 qrcDecrypt 的全部失败一律转成 error，于是「crypt=1 但服务端实际回
// 明文」这种情况从「正常显示歌词」退化成「0 行」——旧代码反而是对的（它失败时
// 保留 rawLyric 继续走）。回归实测：decryptIfNeeded(明文LRC, 1) 返回
// err=`hex decode: invalid byte: U+005B '['`，而同一串在旧路径下能解出 2 行歌词。
//
// 根因是没区分 qrcDecrypt 的两类失败。此测试覆盖「不是密文 → 放行」这一侧。
func TestPlaintextWithCryptFlagStillWorks(t *testing.T) {
	plain := "[00:00.00]作词 : 某人\n[00:12.34]第一行歌词\n[00:20.00]第二行歌词\n"

	// crypt=0（正常明文）与 crypt=1（标志误报）都必须原样放行
	for _, crypt := range []int{0, 1} {
		out, didDecrypt, err := decryptIfNeeded(plain, crypt)
		if err != nil {
			t.Fatalf("crypt=%d 的明文 LRC 不应报错（这正是回归的形状）: %v", crypt, err)
		}
		if didDecrypt {
			t.Errorf("crypt=%d: 明文 LRC 不应被判为已解密", crypt)
		}
		if out != plain {
			t.Errorf("crypt=%d: 明文 LRC 应原样返回", crypt)
		}
		// 端到端：放行后必须真能解出歌词，而不只是「没报错」
		if lines, _, _ := parseLRC(out, 180000); len(lines) != 3 {
			t.Errorf("crypt=%d: 放行后应解出 3 行歌词，实得 %d 行", crypt, len(lines))
		}
	}
}

// TestQrcDecryptFailureKindsAreDistinguishable 钉死两类失败可区分——这是
// decryptIfNeeded 能同时避免「密文上 OBS」和「明文被吞」的前提。
func TestQrcDecryptFailureKindsAreDistinguishable(t *testing.T) {
	// 不是密文：hex 解不开
	if _, err := qrcDecrypt("[00:00.00]歌词"); !errors.Is(err, errNotCiphertext) {
		t.Errorf("明文 LRC 应判为 errNotCiphertext，实得: %v", err)
	}
	// 不是密文：hex 能解开但长度不是 8 的倍数
	if _, err := qrcDecrypt("deadbe"); !errors.Is(err, errNotCiphertext) {
		t.Errorf("长度非 8 倍数应判为 errNotCiphertext，实得: %v", err)
	}
	// 是密文但坏了：hex 与长度都合法，死在 zlib
	_, err := qrcDecrypt(strings.Repeat("deadbeef", 40))
	if err == nil {
		t.Fatal("非法 QRC 应报错")
	}
	if errors.Is(err, errNotCiphertext) {
		t.Errorf("合法 hex+长度对齐者不该判为 errNotCiphertext，实得: %v", err)
	}
}
