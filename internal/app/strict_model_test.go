package app

import (
	"strings"
	"testing"
)

// strict_model（#41）的判定不变量：只有"服务端自报的模型确实不是请求的那个"才算降级；
// 判不了（没自报 / 表里没有该模型）一律放行 —— 这是刻意的，把"看不见"当成"没降级"
// 会把这道防线变成摆设，而误报会让用户开始无视这个开关。
func TestModelDowngraded(t *testing.T) {
	cases := []struct {
		name            string
		model, upstream string
		want            bool
	}{
		{"4 Argon 正常", "gemini-4-argon", "4 Argon", false},
		{"4 Argon 开思考时服务端只报 4 Argon High", "gemini-4-argon-thinking", "4 Argon High", false},
		{"4 Argon 被降级成 Flash-Lite", "gemini-4-argon", "3.5 Flash-Lite", true},
		{"3.5 Flash-Lite 正常（大小写/连字符差异）", "gemini-3.5-flash-lite", "3.5 flash lite", false},
		{"3.8 Flash 被降级成 Flash-Lite", "gemini-3.8-flash", "3.5 Flash-Lite", true},
		{"3.1 Pro 被降级成 Flash-Lite", "gemini-3.1-pro", "3.5 Flash-Lite", true},
		{"3.7 的 id 遇到 3.8 的显示名（改名容错）", "gemini-3.7-flash", "3.8 Flash", false},
		{"没自报显示名 → 不判定", "gemini-4-argon", "", false},
		{"只有空白也算没自报", "gemini-4-argon", "   ", false},
		{"表里没有的模型（生图）→ 不判定", "gemini-image", "3.5 Flash-Lite", false},
	}
	for _, c := range cases {
		got, _ := ModelDowngraded(c.model, c.upstream)
		if got != c.want {
			t.Errorf("%s：ModelDowngraded(%q, %q) = %v，期望 %v", c.name, c.model, c.upstream, got, c.want)
		}
	}
}

// 判据表要覆盖到每个"付费/登录态专属"的模型：漏了就等于静默放行。
// 反向也查一次——表里的每个 id 都必须真实存在于 Models，防止改名后留死条目。
func TestStrictModelExpectTable(t *testing.T) {
	for _, model := range []string{
		"gemini-4-argon", "gemini-4-argon-thinking",
		"gemini-3.1-pro", "gemini-3.1-pro-thinking",
		"gemini-3.8-flash", "gemini-3.5-flash-lite",
	} {
		if len(strictModelExpect[model]) == 0 {
			t.Errorf("判据表缺少 %s —— 它被静默降级时将无法发现", model)
		}
	}
	for model := range strictModelExpect {
		if _, ok := Models[model]; !ok {
			t.Errorf("判据表里的 %s 不在 Models 里（改名后忘了同步？）", model)
		}
	}
}

// 开关默认必须关（不改变既有行为），打开后必须拦；文案要能自证「请求了什么、实际什么」。
func TestStrictModelViolation(t *testing.T) {
	old := rtCfg()
	defer func() {
		rtMu.Lock()
		rtVal = old
		rtMu.Unlock()
	}()

	res := &StreamResult{UpstreamModel: "3.5 Flash-Lite"}

	rtMu.Lock()
	rtVal.StrictModel = false
	rtMu.Unlock()
	if got := strictModelViolation("gemini-4-argon", res); got != "" {
		t.Errorf("开关关闭时不该拦截，实际 %q", got)
	}

	rtMu.Lock()
	rtVal.StrictModel = true
	rtMu.Unlock()
	got := strictModelViolation("gemini-4-argon", res)
	if got == "" {
		t.Fatal("开关打开时应拦截，实际放行")
	}
	for _, want := range []string{"gemini-4-argon", "3.5 Flash-Lite"} {
		if !strings.Contains(got, want) {
			t.Errorf("文案里应提到 %q，实际 %q", want, got)
		}
	}
	// 上游失败时 res 就是 nil，不能 panic
	if got := strictModelViolation("gemini-4-argon", nil); got != "" {
		t.Errorf("res 为 nil 时应放行，实际 %q", got)
	}
}
