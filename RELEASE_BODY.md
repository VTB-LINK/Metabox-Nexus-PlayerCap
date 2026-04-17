### 修复

- **组曲/混音歌曲歌词误判为纯音乐**：CDP 搜索 ID 匹配到原曲纯音乐版时，使用 Go HTTP API 请求 Redux ID 二次确认歌词；Redux ID 为空时不立即标记纯音乐，交由 Redux fallback 补救。
- **CDP 失败后纯音乐歌词残留**：CDP 获取失败且 Redux fallback 超时 3 秒仍无歌词时，自动清空前端歌词显示。

