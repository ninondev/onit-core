package main

// 多模型编排（原版「Manage AI Assistants on your behalf」的本地等价）：
// Go 后端代理调用多家 OpenAI 兼容模型，汇总结果返回。
// POST /v1/orchestrate 接收 {models:[{endpoint,key,model}], prompt, mode}
// mode: "first"（第一个成功的）、"consensus"（多数一致）、"all"（全部结果）。
import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OrchestrateRequest 多模型编排请求。
type OrchestrateRequest struct {
	Models []ModelConfig  `json:"models"`
	Prompt string         `json:"prompt"`
	System string         `json:"system"`
	Mode   string         `json:"mode"` // first, consensus, all
}

// ModelConfig 一家模型的配置。
type ModelConfig struct {
	Endpoint string `json:"endpoint"` // OpenAI 兼容端点
	Key      string `json:"key"`      // API 密钥
	Model    string `json:"model"`    // 模型名
	Label    string `json:"label"`    // 显示名
}

// OrchestrateResult 编排结果。
type OrchestrateResult struct {
	Mode     string            `json:"mode"`
	Answer   string            `json:"answer,omitempty"`   // first/consensus 的答案
	Answers  []ModelAnswer     `json:"answers,omitempty"`  // all 模式的全部答案
	Error    string            `json:"error,omitempty"`
	TookMs   int64             `json:"tookMs"`
}

// ModelAnswer 单家模型的回答。
type ModelAnswer struct {
	Label  string `json:"label"`
	Model  string `json:"model"`
	Text   string `json:"text,omitempty"`
	Error  string `json:"error,omitempty"`
	TookMs int64  `json:"tookMs"`
}

// Orchestrate 调用多家模型并按 mode 汇总。
func Orchestrate(req OrchestrateRequest) (*OrchestrateResult, error) {
	if len(req.Models) == 0 || req.Prompt == "" {
		return nil, ErrInvalid
	}
	if req.Mode == "" {
		req.Mode = "first"
	}

	start := time.Now()
	ch := make(chan ModelAnswer, len(req.Models))
	for _, mc := range req.Models {
		go func(mc ModelConfig) {
			ansStart := time.Now()
			text, err := callModel(mc, req.System, req.Prompt)
			ans := ModelAnswer{
				Label:  mc.Label,
				Model:  mc.Model,
				TookMs: time.Since(ansStart).Milliseconds(),
			}
			if err != nil {
				ans.Error = err.Error()
			} else {
				ans.Text = text
			}
			ch <- ans
		}(mc)
	}

	result := &OrchestrateResult{Mode: req.Mode}
	var answers []ModelAnswer
	for range req.Models {
		answers = append(answers, <-ch)
	}
	result.TookMs = time.Since(start).Milliseconds()

	switch req.Mode {
	case "first":
		// 取第一个成功的
		for _, a := range answers {
			if a.Text != "" {
				result.Answer = a.Text
				break
			}
		}
		if result.Answer == "" {
			result.Error = "所有模型都失败了"
		}
	case "all":
		result.Answers = answers
	case "consensus":
		// 取最长的非空回答作为「多数意见」的代理
		// 真正的共识需要语义比较，这里用长度作为简单代理
		var best string
		for _, a := range answers {
			if len(a.Text) > len(best) {
				best = a.Text
			}
		}
		result.Answer = best
		if best == "" {
			result.Error = "所有模型都失败了"
		}
	default:
		return nil, ErrInvalid
	}

	return result, nil
}

// callModel 调用一家 OpenAI 兼容模型。
func callModel(mc ModelConfig, system, prompt string) (string, error) {
	body := map[string]interface{}{
		"model": mc.Model,
		"messages": []map[string]string{},
		"stream":  false,
	}
	msgs := body["messages"].([]map[string]string)
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": prompt})
	body["messages"] = msgs

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequest("POST", mc.Endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if mc.Key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+mc.Key)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("模型没有返回任何结果")
	}
	return result.Choices[0].Message.Content, nil
}
