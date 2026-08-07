package lyric

// 本文件只测酷狗专属的 KRC 解密（krcDecrypt）：base64 → krc1 魔数 → XOR → zlib。
// KRC 的明文**解析**已抽到 player/krc 包（与汽水音乐共用），其测试也随之迁走；
// 解密是酷狗独有的（汽水平台直接给明文），故留在本包。

import "testing"

// 魔数不符必须报错，绝不把乱码当歌词往下传。
func TestKRCDecryptRejectsBadMagic(t *testing.T) {
	// "aGVsbG8gd29ybGQ=" = "hello world"，前 4 字节不是 krc1
	if _, err := krcDecrypt("aGVsbG8gd29ybGQ="); err == nil {
		t.Fatal("魔数不符时应报错")
	}
	if _, err := krcDecrypt("!!!not-base64!!!"); err == nil {
		t.Fatal("非 base64 时应报错")
	}
}
