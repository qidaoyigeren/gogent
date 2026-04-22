package prompt

import (
	"embed"
	"strings"
	"sync"
)

var embeddedTemplates embed.FS

// templateLoader 模板加载器（带缓存）
type templateLoader struct {
	mu    sync.RWMutex
	cache map[string]string // 缓存：模板名称 → 模板内容
}

func newTemplateLoader() *templateLoader {
	return &templateLoader{cache: make(map[string]string)}
}

// load 加载模板（带缓存和降级）
func (l *templateLoader) load(name, fallback string) string {
	l.mu.Lock()
	if v, ok := l.cache[name]; ok {
		l.mu.Unlock()
		return v
	}
	l.mu.Unlock()
	v := fallback
	if data, err := embeddedTemplates.ReadFile("prompts/" + name); err == nil {
		v = string(data)
	}
	v = cleanupPrompt(v)

	l.mu.Lock()
	l.cache[name] = v
	l.mu.Unlock()
	return v
}

// cleanupPrompt 清理提示词格式
func cleanupPrompt(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
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
