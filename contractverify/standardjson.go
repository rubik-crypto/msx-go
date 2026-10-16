package contractverify

import (
	"encoding/json"
	"errors"
	"strings"
)

// Source 是一个源码文件。Path 必须和当初编译时用的路径一模一样：
// 路径变了，编译出来的元数据哈希就变了，和链上那份对不上，验证会失败。
type Source struct {
	Path    string
	Content string
}

// defaultOutputSelection 让编译器把字节码和 ABI 都产出来——浏览器就靠这些比对。
var defaultOutputSelection = map[string]any{
	"*": map[string]any{
		"*": []string{
			"abi", "metadata",
			"evm.bytecode.object", "evm.bytecode.linkReferences",
			"evm.deployedBytecode.object",
		},
	},
}

// allowedSettings 是标准 JSON 输入认得的设置项。
// 编译元数据（metadata.json）里的 settings 多出 compilationTarget 这类「输出信息」，
// 原样塞回输入会被编译器当成不认识的键而报错，所以只留白名单里的。
var allowedSettings = map[string]bool{
	"remappings":      true,
	"optimizer":       true,
	"evmVersion":      true,
	"libraries":       true,
	"metadata":        true,
	"viaIR":           true,
	"debug":           true,
	"outputSelection": true,
}

// SanitizeSettings 只保留标准 JSON 输入认得的设置项，返回一份新的 map。
func SanitizeSettings(settings map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range settings {
		if allowedSettings[k] {
			out[k] = v
		}
	}
	return out
}

// BuildStandardJSONInput 把源码和编译设置拼成「标准 JSON 输入」——若干浏览器提交源码验证
// 时要的就是这一份。language 留空按 Solidity 处理。
//
// settings 一般来自合约的编译元数据（metadata.json 的 settings 字段），
// 也可能是区块浏览器接口给出的一份标准 JSON 输入里的 settings；
// 多余的非输入项会被丢掉（见 SanitizeSettings），缺 outputSelection 时会补上默认值。
func BuildStandardJSONInput(sources []Source, settings map[string]any, language string) (string, error) {
	srcs := map[string]any{}
	for _, f := range sources {
		path := strings.TrimSpace(f.Path)
		if path == "" {
			continue
		}
		srcs[path] = map[string]any{"content": f.Content}
	}
	if len(srcs) == 0 {
		return "", errors.New("contractverify: 没有可提交的源码文件")
	}
	clean := SanitizeSettings(settings)
	if _, ok := clean["outputSelection"]; !ok {
		clean["outputSelection"] = defaultOutputSelection
	}
	if strings.TrimSpace(language) == "" {
		language = "Solidity"
	}
	b, err := json.Marshal(map[string]any{
		"language": language,
		"sources":  srcs,
		"settings": clean,
	})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SourcesFromStandardJSON 从一份标准 JSON 输入里取出各个源码文件。
// 区块浏览器返回多文件合约的源码时，给的就是这个形状；有的还会在外面多包一层大括号。
func SourcesFromStandardJSON(raw string) ([]Source, bool) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "{") {
		return nil, false
	}
	if strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	var std struct {
		Sources map[string]struct {
			Content string `json:"content"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(s), &std); err != nil || len(std.Sources) == 0 {
		return nil, false
	}
	out := make([]Source, 0, len(std.Sources))
	for path, f := range std.Sources {
		out = append(out, Source{Path: path, Content: f.Content})
	}
	return out, true
}

// SettingsFromStandardJSON 从一份标准 JSON 输入里取出编译设置（已过白名单）。
func SettingsFromStandardJSON(raw string) map[string]any {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	if strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	var std struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal([]byte(s), &std); err != nil || len(std.Settings) == 0 {
		return nil
	}
	return SanitizeSettings(std.Settings)
}
