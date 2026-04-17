package model

import "gogent/internal/config"

// ModelTarget 最终调用哪个 provider/model/url/key
type ModelTarget struct {
	ID       string
	Provider string
	Model    string
	URL      string // full endpoint URL
	APIKey   string
}

// ResolveTarget()：把配置候选项解析成可调用目标。
func ResolveTarget(candidate config.ModelCandidate, provider config.ProviderConfig, endpointKey string) ModelTarget {
	url := provider.URL
	if ep, ok := provider.Endpoints[endpointKey]; ok {
		url = url + ep
	}
	return ModelTarget{
		ID:       candidate.ID,
		Provider: candidate.Provider,
		Model:    candidate.Model,
		URL:      url,
		APIKey:   provider.APIKey,
	}
}
