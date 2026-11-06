// Package pricing estimates the cost of a request from its token usage.
//
// The table is deliberately small and manual: provider prices change, and a
// wrong guess is worse than a missing number. Matching is by longest prefix, so
// "gpt-4o-mini-2024-07-18" finds the mini rate rather than the gpt-4o one. An
// unknown model reports known=false and must not be charged.
package pricing

import (
	"strings"
)

// Rate is USD per one million tokens.
type Rate struct {
	Input  float64
	Output float64
}

var table = map[string]Rate{
	// OpenAI
	"gpt-4o-mini":            {Input: 0.15, Output: 0.60},
	"gpt-4o":                 {Input: 2.50, Output: 10.00},
	"gpt-4.1-mini":           {Input: 0.40, Output: 1.60},
	"gpt-4.1-nano":           {Input: 0.10, Output: 0.40},
	"gpt-4.1":                {Input: 2.00, Output: 8.00},
	"o3-mini":                {Input: 1.10, Output: 4.40},
	"o3":                     {Input: 2.00, Output: 8.00},
	"o4-mini":                {Input: 1.10, Output: 4.40},
	"text-embedding-3-small": {Input: 0.02},
	"text-embedding-3-large": {Input: 0.13},

	// Anthropic
	"claude-opus-4":     {Input: 15.00, Output: 75.00},
	"claude-sonnet-4":   {Input: 3.00, Output: 15.00},
	"claude-haiku-4":    {Input: 1.00, Output: 5.00},
	"claude-3-7-sonnet": {Input: 3.00, Output: 15.00},
	"claude-3-5-sonnet": {Input: 3.00, Output: 15.00},
	"claude-3-5-haiku":  {Input: 0.80, Output: 4.00},

	// Google
	"gemini-2.5-pro":        {Input: 1.25, Output: 10.00},
	"gemini-2.5-flash-lite": {Input: 0.10, Output: 0.40},
	"gemini-2.5-flash":      {Input: 0.30, Output: 2.50},

	// DeepSeek
	"deepseek-reasoner": {Input: 0.55, Output: 2.19},
	"deepseek-chat":     {Input: 0.27, Output: 1.10},
	"deepseek-r1":       {Input: 0.55, Output: 2.19},
	"deepseek-v3":       {Input: 0.27, Output: 1.10},

	// Groq / Meta
	"llama-3.3-70b": {Input: 0.59, Output: 0.79},
	"llama-3.1-8b":  {Input: 0.05, Output: 0.08},

	// Mistral
	"mistral-large": {Input: 2.00, Output: 6.00},
	"mistral-small": {Input: 0.20, Output: 0.60},
}

// RateFor returns the table entry whose prefix matches model.
func RateFor(model string) (Rate, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:] // openrouter-style "vendor/model"
	}
	var (
		best       Rate
		bestPrefix string
	)
	for prefix, rate := range table {
		if strings.HasPrefix(name, prefix) && len(prefix) > len(bestPrefix) {
			best, bestPrefix = rate, prefix
		}
	}
	return best, bestPrefix != ""
}

// Estimate returns the request cost in USD. known is false when no prefix in the
// table matches; the caller should then report the cost as unknown rather than 0.
func Estimate(model string, promptTokens, completionTokens int) (usd float64, known bool) {
	rate, ok := RateFor(model)
	if !ok {
		return 0, false
	}
	usd = float64(promptTokens)*rate.Input/1e6 +
		float64(completionTokens)*rate.Output/1e6
	return usd, true
}
