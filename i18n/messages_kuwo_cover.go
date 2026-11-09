package i18n

// 酷我封面查询的错误文案，含包级 sentinel ErrNoCover（i18n.Sentinel，Error() 时才本地化）。
func init() {
	for en, zh := range kuwoCoverMessages {
		if _, ok := chinese[en]; !ok {
			chinese[en] = zh
		}
	}
}

var kuwoCoverMessages = map[string]string{
	"kuwo: cover timeout must be positive":    "酷我：封面超时必须大于零",
	"kuwo: cover download failed":             "酷我：封面下载失败",
	"kuwo: invalid cover RID":                 "酷我：封面歌曲 RID 无效",
	"kuwo: invalid cover endpoint: %w":        "酷我：封面端点无效：%w",
	"kuwo: failed to build cover request: %w": "酷我：构造封面请求失败：%w",
	"kuwo: cover lookup failed: %w":           "酷我：查询封面失败：%w",
	"kuwo: cover lookup HTTP %d":              "酷我：封面查询返回 HTTP %d",
	"kuwo: failed to read cover response: %w": "酷我：读取封面响应失败：%w",
	"kuwo: cover lookup response too large":   "酷我：封面查询响应过大",
	"kuwo: invalid cover URL":                 "酷我：封面 URL 无效",
	"kuwo: unexpected cover URL":              "酷我：封面 URL 不符合预期",
	"kuwo: no cover":                          "酷我：没有封面",
}
