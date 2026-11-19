// Provider-aware list prices used by the performance dashboard.
//
// Sources, checked 19 November 2026:
//   - opencode-go: GET /inference/go/openai/v1/models, cost.text.*
//   - commandcode: https://commandcode.ai/docs/resources/pricing-limits#models
//
// Values are USD per one million tokens at the base/off-peak list price. Time
// of day multipliers and long-context tiers beyond the first band are not
// applied; the UI labels the column as a base rate.
package pricing

import "strings"

// ModelRate is the published price of one provider/model pair. CacheWrite is
// optional and may be zero when the provider does not charge for it.
type ModelRate struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// providerRates keeps providers with the same commercial terms together.
// opencode-go-anthropic shares the chat gateway's prices; commandcode-claude
// uses the same Command Code table as the chat entry.
var providerRates = map[string]map[string]ModelRate{
	"opencode-go":        openCodeGoRates,
	"opencode-systemone": openCodeSystemOneRates,
	"commandcode":        commandCodeRates,
}

// ModelRateFor looks up the list price for a provider/model pair. The provider
// may be an id or a catalog alias. Unknown providers or models report false,
// and the caller should show "unknown" rather than zero.
func ModelRateFor(provider, model string) (ModelRate, bool) {
	table, ok := providerRates[canonicalProvider(provider)]
	if !ok {
		return ModelRate{}, false
	}
	rate, ok := table[strings.TrimSpace(model)]
	return rate, ok
}

func canonicalProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ocg", "opencode-go", "ocga", "opencode-go-anthropic":
		return "opencode-go"
	case "ocs", "opencode-systemone":
		return "opencode-systemone"
	case "cmd", "commandcode", "cmdc", "commandcode-claude", "cmds", "commandcode-systemone":
		return "commandcode"
	}
	return strings.ToLower(strings.TrimSpace(provider))
}

var openCodeGoRates = map[string]ModelRate{
	"beam-free":                  {Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
	"claude-haiku-5-5":           {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125},
	"deepseek-v4-pro":            {Input: 0.66, Output: 1.98, CacheRead: 0.022, CacheWrite: 0},
	"deepseek-v4.1-flash":        {Input: 0.15, Output: 0.6, CacheRead: 0.003, CacheWrite: 0},
	"gemini-4-argon":             {Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 0},
	"glm-5.2":                    {Input: 1.4, Output: 4.4, CacheRead: 0.26, CacheWrite: 0},
	"glm-5.3":                    {Input: 1.4, Output: 4.4, CacheRead: 0.26, CacheWrite: 0},
	"glm-5.3-flash":              {Input: 0.15, Output: 0.5, CacheRead: 0.03, CacheWrite: 0},
	"gpt-5.6-luna":               {Input: 0.2, Output: 1.2, CacheRead: 0.02, CacheWrite: 0.25},
	"gpt-6-luna":                 {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125},
	"grok-4.7":                   {Input: 2, Output: 6, CacheRead: 0.5, CacheWrite: 0},
	"hy3":                        {Input: 0.14, Output: 0.58, CacheRead: 0.035, CacheWrite: 0},
	"hy4":                        {Input: 0.834, Output: 2.501, CacheRead: 0.042, CacheWrite: 0},
	"hy4-flash-free":             {Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
	"hy4-preview":                {Input: 0.834, Output: 2.501, CacheRead: 0.042, CacheWrite: 0},
	"iceman-contributor-free":    {Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
	"kimi-k2.7-code":             {Input: 0.95, Output: 4, CacheRead: 0.19, CacheWrite: 0},
	"kimi-k3":                    {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3},
	"kimi-k3.1-pro":              {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2},
	"longcat-2.0":                {Input: 0.3, Output: 1.2, CacheRead: 0.006, CacheWrite: 0},
	"longcat-2.5-free":           {Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
	"mimo-v2.6-flash":            {Input: 0.14, Output: 0.28, CacheRead: 0.0028, CacheWrite: 0},
	"mimo-v2.6-pro":              {Input: 0.435, Output: 0.87, CacheRead: 0.003625, CacheWrite: 0},
	"minimax-m3":                 {Input: 0.3, Output: 1.2, CacheRead: 0.06, CacheWrite: 0},
	"minimax-m3.1-flash":         {Input: 0.15, Output: 0.6, CacheRead: 0.015, CacheWrite: 0},
	"muse-spark-1.2-contributor": {Input: 0.1, Output: 0.2, CacheRead: 0.002, CacheWrite: 0},
	"muse-spark-1.3-contributor": {Input: 0.1, Output: 0.2, CacheRead: 0.002, CacheWrite: 0},
	"qwen3.7-plus":               {Input: 0.4, Output: 1.6, CacheRead: 0.04, CacheWrite: 0.5},
	"qwen3.8-flash":              {Input: 0.15, Output: 0.47, CacheRead: 0.016, CacheWrite: 0.2},
	"qwen3.8-max":                {Input: 2, Output: 6, CacheRead: 0.25, CacheWrite: 2.5},
	"qwen4-max":                  {Input: 1.5, Output: 4.5, CacheRead: 0.075, CacheWrite: 1.875},
	"step-5-preview":             {Input: 1, Output: 2.7, CacheRead: 0.1, CacheWrite: 0},
}

var openCodeSystemOneRates = map[string]ModelRate{
	"jev-1.13":            {Input: 0.042, Output: 0, CacheRead: 0, CacheWrite: 0},
	"jev-1.13-free":       {Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
	"gpt-6-luna-decision": {Input: 0.1, Output: 0, CacheRead: 0.01, CacheWrite: 0.125},
	"gemini-decision-1":   {Input: 0.1, Output: 0, CacheRead: 0, CacheWrite: 0},
}

var commandCodeRates = map[string]ModelRate{
	"typesafe/jev":                           {Input: 0.042, Output: 0, CacheRead: 0, CacheWrite: 0},
	"typesafe/jev-1.14.0":                    {Input: 0.042, Output: 0, CacheRead: 0, CacheWrite: 0},
	"MiniMaxAI/MiniMax-M2.5":                 {Input: 0.3, Output: 1.2, CacheRead: 0.03, CacheWrite: 0},
	"MiniMaxAI/MiniMax-M2.7":                 {Input: 0.3, Output: 1.2, CacheRead: 0.06, CacheWrite: 0},
	"MiniMaxAI/MiniMax-M3":                   {Input: 0.3, Output: 1.2, CacheRead: 0.06, CacheWrite: 0},
	"Qwen/Qwen3.6-Max-Preview":               {Input: 1.3, Output: 7.8, CacheRead: 0.26, CacheWrite: 1.63},
	"Qwen/Qwen3.6-Plus":                      {Input: 0.5, Output: 3, CacheRead: 0.1, CacheWrite: 0},
	"Qwen/Qwen3.7-Flash":                     {Input: 0.03, Output: 0.13, CacheRead: 0.006, CacheWrite: 0.038},
	"Qwen/Qwen3.7-Max":                       {Input: 2.5, Output: 7.5, CacheRead: 0.5, CacheWrite: 3.13},
	"Qwen/Qwen3.7-Plus":                      {Input: 0.4, Output: 1.6, CacheRead: 0.08, CacheWrite: 0.5},
	"Qwen/Qwen3.8-27B":                       {Input: 0.4, Output: 3, CacheRead: 0.04, CacheWrite: 0},
	"Qwen/Qwen3.8-Flash":                     {Input: 0.16, Output: 0.47, CacheRead: 0.016, CacheWrite: 0},
	"Qwen/Qwen3.8-Max":                       {Input: 2, Output: 6, CacheRead: 0.25, CacheWrite: 2.5},
	"Qwen/Qwen3.8-Max-0902":                  {Input: 2, Output: 6, CacheRead: 0.25, CacheWrite: 0},
	"Qwen/Qwen3.8-Max-Prime":                 {Input: 4, Output: 12, CacheRead: 0.5, CacheWrite: 5},
	"Qwen/Qwen3.8-Omni-Flash":                {Input: 0.15, Output: 0.47, CacheRead: 0.016, CacheWrite: 0},
	"Qwen/Qwen4-Max":                         {Input: 1.5, Output: 4.5, CacheRead: 0.075, CacheWrite: 1.875},
	"claude-fable-5":                         {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"claude-fable-5-1":                       {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-fable-5-2":                       {Input: 8, Output: 40, CacheRead: 0.2, CacheWrite: 10},
	"claude-haiku-4-5":                       {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	"claude-haiku-4-5-20251001":              {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	"claude-haiku-5-5":                       {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125},
	"claude-opus-4-7":                        {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-8":                        {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-5":                          {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-5-5":                        {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	"claude-sonnet-4-6":                      {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"claude-sonnet-5":                        {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	"claude-sonnet-5-5":                      {Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 2.5},
	"deepseek/deepseek-v4-flash":             {Input: 0.15, Output: 0.6, CacheRead: 0.003, CacheWrite: 0},
	"deepseek/deepseek-v4-flash-fast":        {Input: 0.28, Output: 0.56, CacheRead: 0.07, CacheWrite: 0},
	"deepseek/deepseek-v4-flash-vision-exp":  {Input: 0.15, Output: 0.6, CacheRead: 0.003, CacheWrite: 0},
	"deepseek/deepseek-v4-pro":               {Input: 0.66, Output: 1.98, CacheRead: 0.022, CacheWrite: 0},
	"deepseek/deepseek-v4.1-flash":           {Input: 0.15, Output: 0.6, CacheRead: 0.003, CacheWrite: 0},
	"deepseek/deepseek-v4.1-flash-fast":      {Input: 0.16, Output: 0.58, CacheRead: 0.016, CacheWrite: 0},
	"deepseek/deepseek-v4.1-flash-ultrafast": {Input: 2.4, Output: 9.6, CacheRead: 0.048, CacheWrite: 0},
	"google/gemini-3.1-flash-lite":           {Input: 0.25, Output: 1.5, CacheRead: 0.03, CacheWrite: 0},
	"google/gemini-3.5-flash":                {Input: 1.5, Output: 9, CacheRead: 0.15, CacheWrite: 0},
	"google/gemini-3.5-flash-lite":           {Input: 0.3, Output: 2.5, CacheRead: 0.03, CacheWrite: 0},
	"google/gemini-3.6-flash":                {Input: 1.5, Output: 7.5, CacheRead: 0.15, CacheWrite: 0},
	"google/gemini-3.7-flash":                {Input: 1.5, Output: 7.5, CacheRead: 0.15, CacheWrite: 0.08334},
	"google/gemini-3.8-flash":                {Input: 1.5, Output: 7.5, CacheRead: 0.15, CacheWrite: 0},
	"google/gemini-4-argon":                  {Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 0},
	"google/gemini-4-carbon":                 {Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 0},
	"google/gemini-4-neon":                   {Input: 3, Output: 15, CacheRead: 0.15, CacheWrite: 0},
	"gpt-5.3-codex":                          {Input: 2, Output: 8, CacheRead: 0.5, CacheWrite: 0},
	"gpt-5.4":                                {Input: 2.5, Output: 15, CacheRead: 0.25, CacheWrite: 0},
	"gpt-5.4-mini":                           {Input: 0.75, Output: 4.5, CacheRead: 0.075, CacheWrite: 0},
	"gpt-5.5":                                {Input: 5, Output: 30, CacheRead: 0.5, CacheWrite: 0},
	"gpt-5.6-luna":                           {Input: 0.2, Output: 1.2, CacheRead: 0.02, CacheWrite: 0.25},
	"gpt-5.6-sol":                            {Input: 5, Output: 30, CacheRead: 0.5, CacheWrite: 6.25},
	"gpt-5.6-terra":                          {Input: 2, Output: 12, CacheRead: 0.2, CacheWrite: 2.5},
	"gpt-6-astra":                            {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"gpt-6-luna":                             {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125},
	"gpt-6-sol":                              {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	"gpt-6.1-sol":                            {Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 2.5},
	"gpt-6.1-terra":                          {Input: 1, Output: 5, CacheRead: 0.05, CacheWrite: 1.25},
	"gpt-6.2-astra":                          {Input: 8, Output: 40, CacheRead: 0.4, CacheWrite: 10},
	"gpt-6.2-sol":                            {Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 2.5},
	"inclusionai/ling-3.1-flash":             {Input: 0.3, Output: 0.9, CacheRead: 0.06, CacheWrite: 0},
	"inclusionai/ling-3.1-flash-1019":        {Input: 0.3, Output: 0.9, CacheRead: 0.06, CacheWrite: 0},
	"meituan/LongCat-2.0":                    {Input: 0.3, Output: 1.2, CacheRead: 0.006, CacheWrite: 0},
	"meta/muse-ember":                        {Input: 0.1, Output: 0.5, CacheRead: 0.004, CacheWrite: 0},
	"meta/muse-spark-1.1":                    {Input: 1.25, Output: 4.25, CacheRead: 0.15, CacheWrite: 0},
	"meta/muse-spark-1.2":                    {Input: 1.25, Output: 4.25, CacheRead: 0.15, CacheWrite: 0},
	"meta/muse-spark-1.2-contributor":        {Input: 0.1, Output: 0.2, CacheRead: 0.002, CacheWrite: 0},
	"meta/muse-spark-1.3":                    {Input: 1.25, Output: 4.25, CacheRead: 0.15, CacheWrite: 0},
	"meta/muse-spark-1.3-contributor":        {Input: 0.1, Output: 0.2, CacheRead: 0.002, CacheWrite: 0},
	"meta/muse-spark-open":                   {Input: 1.25, Output: 4.25, CacheRead: 0.15, CacheWrite: 0},
	"meta/muse-wonder-1.0":                   {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 0},
	"minimax/minimax-m3.1-flash":             {Input: 0.15, Output: 0.6, CacheRead: 0.015, CacheWrite: 0},
	"mistral/mistral-large-4":                {Input: 1.36, Output: 4.18, CacheRead: 0.14, CacheWrite: 0},
	"moonshotai/Kimi-K2.5":                   {Input: 0.6, Output: 3, CacheRead: 0.1, CacheWrite: 0},
	"moonshotai/Kimi-K2.6":                   {Input: 0.95, Output: 4, CacheRead: 0.16, CacheWrite: 0},
	"moonshotai/Kimi-K2.7-Code":              {Input: 0.95, Output: 4, CacheRead: 0.19, CacheWrite: 0},
	"moonshotai/Kimi-K2.7-Code-Highspeed":    {Input: 1.9, Output: 8, CacheRead: 0.38, CacheWrite: 0},
	"moonshotai/Kimi-K3":                     {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 0},
	"moonshotai/Kimi-K3.1-Pro":               {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 0},
	"nvidia/nemotron-3-ultra-550b-a55b":      {Input: 0.6, Output: 2.4, CacheRead: 0.12, CacheWrite: 0},
	"nvidia/nemotron-3.5-super-vl":           {Input: 0.2, Output: 0.5, CacheRead: 0.04, CacheWrite: 0},
	"ornith/ornith-1.5-122b":                 {Input: 0.16, Output: 0.64, CacheRead: 0.02, CacheWrite: 0},
	"recursive/flint-v1.0:free":              {Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
	"sakana/fugu-ultra":                      {Input: 5, Output: 30, CacheRead: 0.5, CacheWrite: 0},
	"stealth/glyph-cluster:free":             {Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
	"stepfun/Step-3.5-Flash":                 {Input: 0.09, Output: 0.3, CacheRead: 0.02, CacheWrite: 0},
	"stepfun/Step-3.7-Flash":                 {Input: 0.2, Output: 1.15, CacheRead: 0.04, CacheWrite: 0},
	"stepfun/Step-5-Preview":                 {Input: 1, Output: 2.7, CacheRead: 0.05, CacheWrite: 0},
	"tencent/hy3-paid":                       {Input: 0.14, Output: 0.58, CacheRead: 0.035, CacheWrite: 0},
	"tencent/hy4":                            {Input: 0.834, Output: 2.501, CacheRead: 0.042, CacheWrite: 0},
	"tencent/hy4-flash":                      {Input: 0.07, Output: 0.281, CacheRead: 0.003, CacheWrite: 0},
	"tencent/hy4-preview":                    {Input: 0.834, Output: 2.501, CacheRead: 0.042, CacheWrite: 0},
	"thinkingmachines/inkling":               {Input: 1, Output: 4.05, CacheRead: 0.17, CacheWrite: 0},
	"thinkingmachines/inkling-2-small:free":  {Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
	"thinkingmachines/inkling-small":         {Input: 0.5, Output: 1.2, CacheRead: 0.1, CacheWrite: 0},
	"xai/grok-4.5":                           {Input: 2, Output: 6, CacheRead: 0.5, CacheWrite: 0},
	"xai/grok-4.6":                           {Input: 2, Output: 6, CacheRead: 0.5, CacheWrite: 0},
	"xai/grok-4.7":                           {Input: 2, Output: 6, CacheRead: 0.5, CacheWrite: 0},
	"xiaomi/mimo-v2.6-flash":                 {Input: 0.14, Output: 0.28, CacheRead: 0.0028, CacheWrite: 0},
	"xiaomi/mimo-v2.6-pro":                   {Input: 0.435, Output: 0.87, CacheRead: 0.0036, CacheWrite: 0},
	"xiaomi/mimo-v2.6-pro-ultraspeed":        {Input: 4.35, Output: 8.7, CacheRead: 0.036, CacheWrite: 0},
	"z-ai/glm-5.3-flash":                     {Input: 0.15, Output: 0.5, CacheRead: 0.03, CacheWrite: 0},
	"z-ai/glm-5.3-flashx":                    {Input: 0.37, Output: 1.25, CacheRead: 0.075, CacheWrite: 0},
	"zai-org/GLM-5":                          {Input: 1, Output: 3.2, CacheRead: 0.2, CacheWrite: 0},
	"zai-org/GLM-5.1":                        {Input: 1.4, Output: 4.4, CacheRead: 0.26, CacheWrite: 0},
	"zai-org/GLM-5.2":                        {Input: 1.4, Output: 4.4, CacheRead: 0.26, CacheWrite: 0},
	"zai-org/GLM-5.2-Fast":                   {Input: 3, Output: 10.25, CacheRead: 0.5, CacheWrite: 0},
	"zai-org/GLM-5.3":                        {Input: 1.4, Output: 4.4, CacheRead: 0.26, CacheWrite: 0},
}
