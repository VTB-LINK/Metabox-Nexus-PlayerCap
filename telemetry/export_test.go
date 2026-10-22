package telemetry

// 供外部测试包 telemetry_test 核对隐私提示（audionotice_test.go）。那条门禁要导入 audiocap/manager，而
// manager 的 Windows 文件导入本包，放进包内测试会成环；外部测试包不受影响。
var (
	NoticeTelemetrySection   = noticeTelemetrySection
	NoticeTelemetrySectionEN = noticeTelemetrySectionEN
)
