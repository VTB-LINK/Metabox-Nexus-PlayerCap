package cloudmusic

// 本文件只测封面 URL 请求的尺寸档（issue #109）：normalizeNeteaseCoverURL 对 *.music.126.net 的 http(s) URL
// 把 thumbnail 的值改成 800y800、没有 thumbnail 也没有 param 时补 param=800y800，其余原样。

import "testing"

const neteaseCover = "https://p3.music.126.net/pMyAJ-EJF4Mz_pbJRXzwSQ==/109951165967125218.jpg"

// 变异自证（实测）：thumbnail 改成删除 → 查询串里有 thumbnail 键的十一例红；有 thumbnail 仍补 param → 九例红
// （有 thumbnail 无 param 的各例）；只改第一个 thumbnail →「多个 thumbnail」一例红；有 param 也照补 → 十五例红
// （幂等检查）；不切片段 → 带片段的三例红；去掉主机判断 →「其他主机」三例红。
func TestNormalizeNeteaseCoverURL(t *testing.T) {
	cases := []struct{ name, in, want string }{
		// Redux 与 /api/song/detail 给的无参原图
		{"无参数（https）", neteaseCover, neteaseCover + "?param=800y800"},
		{"无参数（http）", "http://p1.music.126.net/kRIAcze06ZeX-8ybvQbvEw==/5983542278885386.jpg", "http://p1.music.126.net/kRIAcze06ZeX-8ybvQbvEw==/5983542278885386.jpg?param=800y800"},
		{"带端口", "https://p1.music.126.net:443/a.jpg", "https://p1.music.126.net:443/a.jpg?param=800y800"},
		{"scheme 大写", "HTTPS://p1.music.126.net/a.jpg", "HTTPS://p1.music.126.net/a.jpg?param=800y800"},
		{"主机全大写", "https://P3.MUSIC.126.NET/a.jpg", "https://P3.MUSIC.126.NET/a.jpg?param=800y800"},
		{"主机混合大小写，带 thumbnail", "https://p3.Music.126.Net/a.jpg?imageView&thumbnail=300y300", "https://p3.Music.126.Net/a.jpg?imageView&thumbnail=800y800"},
		{"主机大写，带端口与 param", "https://P3.MUSIC.126.NET:443/a.jpg?param=130y130", "https://P3.MUSIC.126.NET:443/a.jpg?param=130y130"},
		{"大写的伪装后缀仍不算", "https://P1.MUSIC.126.NET.EXAMPLE.COM/a.jpg", "https://P1.MUSIC.126.NET.EXAMPLE.COM/a.jpg"},
		{"只有一个问号", neteaseCover + "?", neteaseCover + "?param=800y800"},

		// 已有 param：不动
		{"已有 param=800y800", neteaseCover + "?param=800y800", neteaseCover + "?param=800y800"},
		{"已有其他尺寸的 param", neteaseCover + "?param=130y130", neteaseCover + "?param=130y130"},
		{"已有 param 且有其他参数", neteaseCover + "?a=1&param=130y130&b=2", neteaseCover + "?a=1&param=130y130&b=2"},

		// DOM 底栏图带的 thumbnail：只改它的值，不补 param
		{"只有 thumbnail（页面脚本改过的 300y300）", neteaseCover + "?thumbnail=300y300", neteaseCover + "?thumbnail=800y800"},
		{"thumbnail 值已是 800y800", neteaseCover + "?thumbnail=800y800", neteaseCover + "?thumbnail=800y800"},
		{"imageView、thumbnail、type", neteaseCover + "?imageView&thumbnail=48y48&type=webp", neteaseCover + "?imageView&thumbnail=800y800&type=webp"},
		{"thumbnail 在中间", neteaseCover + "?imageView&thumbnail=300y300&quality=75", neteaseCover + "?imageView&thumbnail=800y800&quality=75"},
		{"thumbnail 在末尾", neteaseCover + "?imageView&thumbnail=60y60", neteaseCover + "?imageView&thumbnail=800y800"},
		{"thumbnail 没有值", neteaseCover + "?imageView&thumbnail", neteaseCover + "?imageView&thumbnail=800y800"},
		{"多个 thumbnail 全改", neteaseCover + "?thumbnail=48y48&a=1&thumbnail=300y300", neteaseCover + "?thumbnail=800y800&a=1&thumbnail=800y800"},
		{"thumbnail 在 param 之后：只改 thumbnail", neteaseCover + "?param=200y200&thumbnail=300y300", neteaseCover + "?param=200y200&thumbnail=800y800"},
		{"thumbnail 在 param 之前：只改 thumbnail", neteaseCover + "?thumbnail=300y300&param=200y200", neteaseCover + "?thumbnail=800y800&param=200y200"},
		{"thumbnail 与空段：空段原样", neteaseCover + "?&thumbnail=300y300&&a=1", neteaseCover + "?&thumbnail=800y800&&a=1"},
		{"键名只是以 thumbnail 开头的不算", neteaseCover + "?thumbnail2=1", neteaseCover + "?thumbnail2=1&param=800y800"},
		{"thumbnail 只在值里不算", neteaseCover + "?from=thumbnail=1", neteaseCover + "?from=thumbnail=1&param=800y800"},

		// param 字样不在查询串的键上
		{"param 在路径里", "https://p1.music.126.net/param=1/a.jpg", "https://p1.music.126.net/param=1/a.jpg?param=800y800"},
		{"键名是 params", neteaseCover + "?params=1", neteaseCover + "?params=1&param=800y800"},
		{"param 只在值里", neteaseCover + "?from=param=1", neteaseCover + "?from=param=1&param=800y800"},

		// 片段
		{"带片段、无查询串", neteaseCover + "#frag", neteaseCover + "?param=800y800#frag"},
		{"片段里的 param 与 thumbnail 不算", neteaseCover + "?thumbnail=300y300#a?param=1&thumbnail=2", neteaseCover + "?thumbnail=800y800#a?param=1&thumbnail=2"},
		{"片段里的 param 不算（无查询串）", neteaseCover + "#param=1", neteaseCover + "?param=800y800#param=1"},

		// 原样返回
		{"空串", "", ""},
		{"其他主机（汽水）", "https://p3-luna.douyinpic.com/img/tos-cn-v-2774c002/a~tplv-b829550vbb-crop-center:800:800.jpg", "https://p3-luna.douyinpic.com/img/tos-cn-v-2774c002/a~tplv-b829550vbb-crop-center:800:800.jpg"},
		{"其他主机（后缀伪装）", "https://p1.music.126.net.example.com/a.jpg?thumbnail=300y300", "https://p1.music.126.net.example.com/a.jpg?thumbnail=300y300"},
		{"其他主机（没有子域）", "https://music.126.net/a.jpg", "https://music.126.net/a.jpg"},
		{"非 http(s)：orpheus", "orpheus://cache/?https://p1.music.126.net/a.jpg?thumbnail=300y300", "orpheus://cache/?https://p1.music.126.net/a.jpg?thumbnail=300y300"},
		{"非 http(s)：data", "data:image/jpeg;base64,/9j/2Q==", "data:image/jpeg;base64,/9j/2Q=="},
		{"非 http(s)：ftp", "ftp://p1.music.126.net/a.jpg", "ftp://p1.music.126.net/a.jpg"},
		{"没有 scheme", "//p1.music.126.net/a.jpg", "//p1.music.126.net/a.jpg"},
		{"解析失败：路径转义非法", "https://p1.music.126.net/%zz.jpg?thumbnail=300y300", "https://p1.music.126.net/%zz.jpg?thumbnail=300y300"},
		{"解析失败：主机不完整", "https://[p1.music.126.net/a.jpg", "https://[p1.music.126.net/a.jpg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeNeteaseCoverURL(c.in)
			if got != c.want {
				t.Errorf("normalizeNeteaseCoverURL(%q) = %q，应为 %q", c.in, got, c.want)
			}
			if again := normalizeNeteaseCoverURL(got); again != got {
				t.Errorf("再调用一次应不变：%q → %q", got, again)
			}
		})
	}
}
