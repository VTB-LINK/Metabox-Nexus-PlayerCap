package player

import (
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// DecodeLyricFileText 把本地歌词文件的字节解码为 UTF-8 文本：带 UTF-8、UTF-16LE、UTF-16BE 的 BOM 时按 BOM
// 解码并去掉 BOM；无 BOM 时合法 UTF-8 原样使用，否则按 GB18030（GBK 的超集）解码。无 BOM 的
// UTF-16 不猜。
func DecodeLyricFileText(b []byte) (string, error) {
	var fallback transform.Transformer = transform.Nop
	if !utf8.Valid(b) {
		fallback = simplifiedchinese.GB18030.NewDecoder()
	}
	out, _, err := transform.Bytes(unicode.BOMOverride(fallback), b)
	return string(out), err
}
