package assoc

// 本文件只测 Key：由设计 §2.1 记下的 7 个 dword 按 QQMusicCommon.dll 的算法重算，须与常量逐字节相同。

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestKeyMatchesDerivation(t *testing.T) {
	d := [7]uint32{0x6a10d376, 0x0c75f63c, 0x437fb91e, 0x1339c1c9, 0x073cfd1a, 0x5e748518, 0x792912cf}
	seed := d[0]
	skip := 1 + int(seed%10%4) // 跳过的是 d[1..5] 中的第 (seed%10)%4 项
	var k []byte
	for i := 1; i <= 5; i++ {
		if i == skip {
			continue
		}
		k = binary.LittleEndian.AppendUint32(k, d[i]^seed)
	}
	if !bytes.Equal(k, Key[:]) {
		t.Fatalf("Key = % x，按 7 个 dword 重算为 % x", Key[:], k)
	}
}
