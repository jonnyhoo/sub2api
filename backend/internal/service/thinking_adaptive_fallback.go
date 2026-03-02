package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	gocache "github.com/patrickmn/go-cache"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// adaptiveProbeTTL 探测结果缓存时间，超时后重新探测（自动 fallback）
	adaptiveProbeTTL = 1 * time.Hour
	// adaptiveProbeCleanup 缓存清理间隔
	adaptiveProbeCleanup = 10 * time.Minute
	// adaptiveDefaultBudget 回退到 enabled 模式时的默认 budget
	adaptiveDefaultBudget = 10000
)

// newAdaptiveThinkingCache 创建 adaptive thinking 探测结果缓存
func newAdaptiveThinkingCache() *gocache.Cache {
	return gocache.New(adaptiveProbeTTL, adaptiveProbeCleanup)
}

// adaptiveCacheKey 生成缓存 key（按 account ID）
func adaptiveCacheKey(accountID int64) string {
	return fmt.Sprintf("adaptive_thinking_%d", accountID)
}

// isAdaptiveThinkingSupported 检查缓存，返回 (是否支持, 是否有缓存记录)
func (s *GatewayService) isAdaptiveThinkingSupported(accountID int64) (supported bool, cached bool) {
	key := adaptiveCacheKey(accountID)
	val, found := s.adaptiveThinkingCache.Get(key)
	if !found {
		return false, false
	}
	return val.(bool), true
}

// setAdaptiveThinkingSupported 写入缓存
func (s *GatewayService) setAdaptiveThinkingSupported(accountID int64, supported bool) {
	key := adaptiveCacheKey(accountID)
	s.adaptiveThinkingCache.Set(key, supported, adaptiveProbeTTL)
}

// probeAdaptiveThinking 向上游发送一个最小化非流式请求，检测是否支持 adaptive thinking。
// 返回 true 表示上游支持 adaptive 模式（响应中包含 thinking 块）。
func (s *GatewayService) probeAdaptiveThinking(
	ctx context.Context,
	account *Account,
	token string,
	proxyURL string,
) bool {
	probeBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"1+1"}]}`)

	baseURL := account.GetBaseURL()
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	targetURL := baseURL + "/v1/messages"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(probeBody))
	if err != nil {
		logger.LegacyPrintf("service.gateway", "[AdaptiveProbe] 构建探测请求失败: account=%d err=%v", account.ID, err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req = req.WithContext(probeCtx)

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, account.IsTLSFingerprintEnabled())
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		logger.LegacyPrintf("service.gateway", "[AdaptiveProbe] 探测请求失败: account=%d err=%v", account.ID, err)
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		logger.LegacyPrintf("service.gateway", "[AdaptiveProbe] 探测返回非200: account=%d status=%d", account.ID, resp.StatusCode)
		return false
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		logger.LegacyPrintf("service.gateway", "[AdaptiveProbe] 读取探测响应失败: account=%d err=%v", account.ID, err)
		return false
	}

	// 检查响应中是否包含 thinking 块
	hasThinking := false
	contentArr := gjson.GetBytes(respBody, "content")
	if contentArr.IsArray() {
		contentArr.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "thinking" {
				hasThinking = true
				return false // 找到即停
			}
			return true
		})
	}

	logger.LegacyPrintf("service.gateway", "[AdaptiveProbe] 探测完成: account=%d supported=%v", account.ID, hasThinking)
	return hasThinking
}

// convertAdaptiveToEnabled 将请求体中的 thinking.type 从 adaptive 改为 enabled，
// 并添加 budget_tokens。返回修改后的 body。
func convertAdaptiveToEnabled(body []byte, budgetTokens int) []byte {
	if budgetTokens <= 0 {
		budgetTokens = adaptiveDefaultBudget
	}

	// 用 sjson 替换 thinking 对象
	newThinking := map[string]any{
		"type":          "enabled",
		"budget_tokens": budgetTokens,
	}
	thinkingJSON, err := json.Marshal(newThinking)
	if err != nil {
		return body
	}

	result, err := sjson.SetRawBytes(body, "thinking", thinkingJSON)
	if err != nil {
		return body
	}
	return result
}

// maybeConvertAdaptiveThinking 检查请求是否使用 adaptive thinking，
// 如果上游不支持则自动转换为 enabled 模式。
// 该方法包含缓存和自动探测逻辑，缓存过期后会重新探测实现自动 fallback。
func (s *GatewayService) maybeConvertAdaptiveThinking(
	ctx context.Context,
	body []byte,
	account *Account,
	token string,
	proxyURL string,
) []byte {
	thinkingType := gjson.GetBytes(body, "thinking.type").String()
	if thinkingType != "adaptive" {
		return body
	}

	// 查缓存
	supported, cached := s.isAdaptiveThinkingSupported(account.ID)
	if cached {
		if supported {
			return body // 上游支持 adaptive，原样透传
		}
		// 上游不支持，转换为 enabled
		logger.LegacyPrintf("service.gateway", "[AdaptiveThinking] 账号 %d 上游不支持 adaptive（缓存命中），转换为 enabled", account.ID)
		return convertAdaptiveToEnabled(body, adaptiveDefaultBudget)
	}

	// 缓存未命中，发送探测请求
	logger.LegacyPrintf("service.gateway", "[AdaptiveThinking] 账号 %d 首次检测 adaptive 支持，开始探测...", account.ID)
	supported = s.probeAdaptiveThinking(ctx, account, token, proxyURL)
	s.setAdaptiveThinkingSupported(account.ID, supported)

	if !supported {
		logger.LegacyPrintf("service.gateway", "[AdaptiveThinking] 账号 %d 上游不支持 adaptive，转换为 enabled", account.ID)
		return convertAdaptiveToEnabled(body, adaptiveDefaultBudget)
	}

	return body
}
