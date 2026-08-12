package clientid

// 本文件守四件事，每一件都对应一种「错了但看不出来」的失败：
//
//	1. 派生必须稳定      —— 不稳定 = 同一台机器每次算出不同 ID = DAU 被灌成启动次数
//	2. 派生必须归一化    —— 大小写/空白不归一 = 同机不同读取路径算出两个 ID，去重静默失效
//	3. 原值绝不能泄漏    —— 发出去的必须是哈希，不是 MachineGuid 本身
//	4. 域前缀必须参与    —— 少了它，我们的 ID 就等于「MachineGuid 的裸 SHA256」，
//	                        任何人算一遍就能反查，跨用途关联重新成立
//
// 这些全部打在 derive 这个纯函数上，**它不是接线**。接线（谁调它、结果进不进请求头）
// 由 headers_test.go 与 telemetry/gatewaynotice_test.go 守——别把这四条当成接线有覆盖。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
)

// 一个真实形状的 MachineGuid（随机编的，不是任何真机的值）。
const sampleGUID = "f4c1a7e2-9b3d-4e58-8a10-6d2f7c93b0ea"

// resetID 清掉 ID() 的 sync.Once 缓存，让每个用例都能重新走一次派生。
// 没有它，第一个跑到的用例会把结果钉死给后面所有用例。
func resetID(t *testing.T, read func() (string, error)) {
	t.Helper()
	origRead := readMachineGUID
	readMachineGUID = read
	idOnce = sync.Once{}
	idCached = ""
	t.Cleanup(func() {
		readMachineGUID = origRead
		idOnce = sync.Once{}
		idCached = ""
	})
}

func TestDeriveIsStable(t *testing.T) {
	first := derive(sampleGUID)
	if first == "" {
		t.Fatal("derive 对合法 GUID 返回了空串")
	}
	for i := 0; i < 100; i++ {
		if got := derive(sampleGUID); got != first {
			t.Fatalf("第 %d 次派生结果不同：%q != %q —— 同一台机器必须永远算出同一个 ID", i, got, first)
		}
	}
}

func TestDeriveShape(t *testing.T) {
	got := derive(sampleGUID)
	if len(got) != 32 {
		t.Errorf("ID 长度为 %d，期望 32（128 位十六进制）：%q", len(got), got)
	}
	if got != strings.ToLower(got) {
		t.Errorf("ID 含大写字符：%q —— 网关侧按字符串去重，大小写漂移会把一台机器算成两台", got)
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Errorf("ID 不是合法十六进制：%q（%v）", got, err)
	}
}

// TestDeriveNormalizes 钉死大小写与空白归一。
//
// 变异自证：把 derive 里的 strings.ToLower 或 TrimSpace 去掉，本用例当场红。
func TestDeriveNormalizes(t *testing.T) {
	want := derive(sampleGUID)
	for _, variant := range []string{
		strings.ToUpper(sampleGUID),
		"  " + sampleGUID + "  ",
		"\t" + strings.ToUpper(sampleGUID) + "\r\n",
	} {
		if got := derive(variant); got != want {
			t.Errorf("变体 %q 派生出了不同的 ID —— 同一台机器会被算成两台", variant)
		}
	}
}

func TestDeriveEmptyStaysEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if got := derive(in); got != "" {
			t.Errorf("derive(%q) = %q，期望空串 —— 空 GUID 必须表现为「没采到」，不能编一个", in, got)
		}
	}
}

func TestDeriveDoesNotLeakRawGUID(t *testing.T) {
	got := derive(sampleGUID)
	if strings.Contains(strings.ToLower(got), strings.ToLower(sampleGUID)) {
		t.Fatalf("派生结果里含 MachineGuid 原值：%q", got)
	}
	// 去掉连字符再查一遍：hex 编码不会带 "-"，光查带连字符的原值挡不住「剥了连字符直接发」。
	bare := strings.ReplaceAll(strings.ToLower(sampleGUID), "-", "")
	if strings.Contains(got, bare) {
		t.Fatalf("派生结果里含剥掉连字符的 MachineGuid：%q", got)
	}
}

// TestDeriveIsDomainSeparated 钉死域前缀真的参与了哈希。
//
// 少了它，我们发出去的就等于「MachineGuid 的裸 SHA256」——任何拿到 GUID 的人算一遍
// 就能反查是哪台机器，包注释里承诺的「跨用途关联在机制上不成立」当场作废。
//
// 变异自证：把 derive 里的 idDomain 去掉，本用例当场红。
func TestDeriveIsDomainSeparated(t *testing.T) {
	plain := sha256.Sum256([]byte(strings.ToLower(sampleGUID)))
	naked := hex.EncodeToString(plain[:16])

	if got := derive(sampleGUID); got == naked {
		t.Fatal("派生结果等于 MachineGuid 的裸 SHA256 —— 域前缀没有参与哈希")
	}
}

func TestDeriveDistinguishesMachines(t *testing.T) {
	a := derive(sampleGUID)
	b := derive("0e93b2d1-5f47-4a06-9c88-1b3e5d72af64")
	if a == b {
		t.Fatal("两台不同机器派生出了同一个 ID")
	}
}

// TestIDReturnsEmptyWhenRegistryFails 钉死「读不到就空着，绝不编一个」。
//
// 退回随机值会让同一台机器每次启动都算成新设备——DAU 被灌成启动次数，
// 而且曲线只会变好看，没人会去查。
//
// 变异自证：把 ID() 里那条 return 改成 idCached = derive(randomSomething)，本用例当场红。
func TestIDReturnsEmptyWhenRegistryFails(t *testing.T) {
	resetID(t, func() (string, error) { return "", errors.New("注册表不可读") })

	if got := ID(); got != "" {
		t.Fatalf("读不到 MachineGuid 时 ID() 返回了 %q，期望空串", got)
	}
}

func TestIDUsesRegistryValue(t *testing.T) {
	resetID(t, func() (string, error) { return sampleGUID, nil })

	if got, want := ID(), derive(sampleGUID); got != want {
		t.Fatalf("ID() = %q，期望 %q", got, want)
	}
}

// TestIDCachesFirstRead 确认注册表只读一次——心跳路径会按天反复调 ID()。
func TestIDCachesFirstRead(t *testing.T) {
	calls := 0
	resetID(t, func() (string, error) {
		calls++
		return sampleGUID, nil
	})

	for i := 0; i < 5; i++ {
		ID()
	}
	if calls != 1 {
		t.Fatalf("readMachineGUID 被调用了 %d 次，期望 1 次", calls)
	}
}
