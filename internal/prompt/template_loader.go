package prompt

import (
	"embed"
	"strings"
	"sync"
)

//go:embed prompts/*.txt
var embeddedTemplates embed.FS // 嵌入模板文件系统（编译时打包）

// templateLoader 模板加载器（带缓存）
// 核心职责：
// 1. 从嵌入文件系统加载模板（prompts/*.txt）
// 2. 缓存已加载的模板（避免重复读取）
// 3. 支持降级（如果文件不存在，使用 fallback）
type templateLoader struct {
	mu    sync.RWMutex
	cache map[string]string // 缓存：模板名称 → 模板内容
}

func newTemplateLoader() *templateLoader {
	return &templateLoader{cache: make(map[string]string)}
}

// load 加载模板（带缓存和降级）
// 工作流程：
// 1. 检查缓存（读锁）
// 2. 如果缓存命中，直接返回
// 3. 如果未命中，从嵌入文件系统读取
// 4. 如果文件不存在，使用 fallback
// 5. 清理模板格式（cleanupPrompt）
// 6. 写入缓存（写锁）
func (l *templateLoader) load(name, fallback string) string {
	l.mu.RLock()
	if v, ok := l.cache[name]; ok {
		l.mu.RUnlock()
		return v
	}
	l.mu.RUnlock()

	v := fallback
	if data, err := embeddedTemplates.ReadFile("prompts/" + name); err == nil && len(data) > 0 {
		v = string(data) // 文件存在，使用文件内容
	}
	v = cleanupPrompt(v) // 清理格式

	l.mu.Lock()
	l.cache[name] = v
	l.mu.Unlock()
	return v
}

// fillSlots 填充模板槽位（替换 {{key}} 为 value）
// 示例：fillSlots("Hello {{name}}", {"name": "World"}) → "Hello World"
func fillSlots(tpl string, slots map[string]string) string {
	out := tpl
	for k, v := range slots {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return cleanupPrompt(out)
}

// cleanupPrompt 清理提示词格式
// 核心职责：
// 1. 统一换行符（\r\n → \n）
// 2. 压缩多余空行（3个以上 \n → 2个 \n）
// 3. 去除首尾空白
func cleanupPrompt(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}
