package rum

import (
	"bytes"
	"encoding/json"
	"math"
)

const remoteFlagKey = "playercap-rum-config"

// remoteConfig 为不可变运行期配置快照，经 atomic.Pointer 发布供热路径无锁读取。
// gen 与策略字段同处一份不可变结构体，消除策略与代次分步原子读产生的竞态撕裂。
type remoteConfig struct {
	gen                   uint64 // 0 表示默认内置配置；每次有效 /flags 更新递增
	enabled               bool
	personProfilesOnStart bool
	disabledCategories    map[string]struct{}
	disabledEvents        map[string]struct{}
	sampleRates           map[string]float64
}

func defaultRemoteConfig() *remoteConfig {
	return &remoteConfig{
		gen:                   0,
		enabled:               true,
		personProfilesOnStart: false,
	}
}

func (c *remoteConfig) cloneWithEnabled(enabled bool) *remoteConfig {
	if c == nil {
		cp := defaultRemoteConfig()
		cp.enabled = enabled
		return cp
	}
	cp := &remoteConfig{
		gen:                   c.gen,
		enabled:               enabled,
		personProfilesOnStart: c.personProfilesOnStart,
	}
	if len(c.disabledCategories) > 0 {
		cp.disabledCategories = make(map[string]struct{}, len(c.disabledCategories))
		for k := range c.disabledCategories {
			cp.disabledCategories[k] = struct{}{}
		}
	}
	if len(c.disabledEvents) > 0 {
		cp.disabledEvents = make(map[string]struct{}, len(c.disabledEvents))
		for k := range c.disabledEvents {
			cp.disabledEvents[k] = struct{}{}
		}
	}
	if len(c.sampleRates) > 0 {
		cp.sampleRates = make(map[string]float64, len(c.sampleRates))
		for k, v := range c.sampleRates {
			cp.sampleRates[k] = v
		}
	}
	return cp
}

// isDisabled 判定事件是否命中总开关或分类/事件显式关闭规则。
func (c *remoteConfig) isDisabled(cat Category, event string) bool {
	if c == nil {
		return false
	}
	if !c.enabled {
		return true
	}
	if _, off := c.disabledEvents[event]; off {
		return true
	}
	if _, off := c.disabledCategories[string(cat)]; off {
		return true
	}
	return false
}

// allowSample 按采样率判定事件是否放行；CatLifecycle 始终豁免 sample_rates 降采样。
func (c *remoteConfig) allowSample(cat Category, event string, randFloat func() float64) bool {
	if c == nil || cat == CatLifecycle {
		return true
	}
	rate := 1.0
	if r, ok := c.sampleRates[event]; ok {
		rate = r
	} else if r, ok := c.sampleRates[string(cat)]; ok {
		rate = r
	}
	if rate >= 1.0 {
		return true
	}
	if rate <= 0.0 {
		return false
	}
	return randFloat() < rate
}

// allow 按远端开关与采样率判定事件是否放行。
// CatLifecycle 豁免 sample_rates 降采样，仅受 enabled / disabled_categories / disabled_events 管控。
func (c *remoteConfig) allow(cat Category, event string, randFloat func() float64) bool {
	return !c.isDisabled(cat, event) && c.allowSample(cat, event, randFloat)
}

type flagsResponseEnvelope struct {
	ErrorsWhileComputingFlags bool                       `json:"errorsWhileComputingFlags"`
	QuotaLimited              []string                   `json:"quotaLimited"`
	Flags                     map[string]json.RawMessage `json:"flags"`
	FeatureFlags              map[string]json.RawMessage `json:"featureFlags"`
	FeatureFlagPayloads       map[string]json.RawMessage `json:"featureFlagPayloads"`
}

type flagV2Metadata struct {
	Payload json.RawMessage `json:"payload"`
}

type flagV2Entry struct {
	Enabled  *bool           `json:"enabled"`
	Failed   bool            `json:"failed"`
	Metadata json.RawMessage `json:"metadata"`
}

type remoteConfigPayload struct {
	Enabled               *bool              `json:"enabled"`
	PersonProfilesOnStart *bool              `json:"person_profiles_on_start"`
	DisabledCategories    []string           `json:"disabled_categories"`
	DisabledEvents        []string           `json:"disabled_events"`
	SampleRates           map[string]float64 `json:"sample_rates"`
}

func normalizeSampleRates(in map[string]float64) map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if v < 0.0 {
			v = 0.0
		} else if v > 1.0 {
			v = 1.0
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildRemoteConfig(p remoteConfigPayload) *remoteConfig {
	cfg := defaultRemoteConfig()
	if p.Enabled != nil {
		cfg.enabled = *p.Enabled
	}
	if p.PersonProfilesOnStart != nil {
		cfg.personProfilesOnStart = *p.PersonProfilesOnStart
	}
	if len(p.DisabledCategories) > 0 {
		cfg.disabledCategories = make(map[string]struct{}, len(p.DisabledCategories))
		for _, cat := range p.DisabledCategories {
			if cat != "" {
				cfg.disabledCategories[cat] = struct{}{}
			}
		}
	}
	if len(p.DisabledEvents) > 0 {
		cfg.disabledEvents = make(map[string]struct{}, len(p.DisabledEvents))
		for _, ev := range p.DisabledEvents {
			if ev != "" {
				cfg.disabledEvents[ev] = struct{}{}
			}
		}
	}
	cfg.sampleRates = normalizeSampleRates(p.SampleRates)
	return cfg
}

func parseRemoteConfigPayload(raw json.RawMessage) *remoteConfig {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var innerStr string
		if err := json.Unmarshal(trimmed, &innerStr); err != nil {
			return nil
		}
		trimmed = bytes.TrimSpace([]byte(innerStr))
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var p remoteConfigPayload
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return nil
	}
	return buildRemoteConfig(p)
}

// parseFlagsResponse 解析 PostHog /flags/?v=2 响应。
// 优先按顶层 flags 解析 v2 原生结构，仅当顶层 flags 为 nil 时向后兼容旧版 featureFlags/featureFlagPayloads。
// 返回值 ok 为 true 表示产生有效配置更新；否则原样返回 prev 不改变现行配置。
func parseFlagsResponse(body []byte, prev *remoteConfig) (*remoteConfig, bool) {
	if prev == nil {
		prev = defaultRemoteConfig()
	}
	var env flagsResponseEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return prev, false
	}
	if env.ErrorsWhileComputingFlags {
		return prev, false
	}
	for _, q := range env.QuotaLimited {
		if q == "feature_flags" {
			return prev, false
		}
	}

	if env.Flags != nil {
		rawEntry, ok := env.Flags[remoteFlagKey]
		if !ok {
			return prev, false
		}
		trimmedEntry := bytes.TrimSpace(rawEntry)
		if len(trimmedEntry) == 0 || trimmedEntry[0] != '{' {
			return prev, false
		}
		var entry flagV2Entry
		if err := json.Unmarshal(trimmedEntry, &entry); err != nil {
			return prev, false
		}
		if entry.Failed || entry.Enabled == nil {
			return prev, false
		}
		if !*entry.Enabled {
			return prev.cloneWithEnabled(false), true
		}
		trimmedMeta := bytes.TrimSpace(entry.Metadata)
		if len(trimmedMeta) == 0 || trimmedMeta[0] != '{' {
			return prev, false
		}
		var meta flagV2Metadata
		if err := json.Unmarshal(trimmedMeta, &meta); err != nil {
			return prev, false
		}
		parsed := parseRemoteConfigPayload(meta.Payload)
		if parsed == nil {
			return prev, false
		}
		return parsed, true
	}

	explicitFalse := false
	if rawFlag, ok := env.FeatureFlags[remoteFlagKey]; ok {
		var flagBool *bool
		if err := json.Unmarshal(rawFlag, &flagBool); err == nil && flagBool != nil && !*flagBool {
			explicitFalse = true
		}
	}

	var parsedPayload *remoteConfig
	if rawPayload, ok := env.FeatureFlagPayloads[remoteFlagKey]; ok {
		parsedPayload = parseRemoteConfigPayload(rawPayload)
	}

	if explicitFalse {
		if parsedPayload != nil {
			parsedPayload.enabled = false
			return parsedPayload, true
		}
		return prev.cloneWithEnabled(false), true
	}

	if parsedPayload != nil {
		return parsedPayload, true
	}
	return prev, false
}
