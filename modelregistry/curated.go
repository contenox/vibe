package modelregistry

var curatedModels = map[string]ModelDescriptor{
	// ── Qwen 2.5 ─────────────────────────────────────────────────────────────
	"qwen2.5-1.5b": {
		Name:      "qwen2.5-1.5b",
		SourceURL: "https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf",
		SizeBytes: 934_000_000,
		Curated:   true,
	},
	"qwen2.5-7b": {
		Name:      "qwen2.5-7b",
		SourceURL: "https://huggingface.co/Qwen/Qwen2.5-7B-Instruct-GGUF/resolve/main/qwen2.5-7b-instruct-q4_k_m.gguf",
		SizeBytes: 4_680_000_000,
		Curated:   true,
	},
	// ── Qwen 3 ───────────────────────────────────────────────────────────────
	"qwen3-4b": {
		Name:      "qwen3-4b",
		SourceURL: "https://huggingface.co/bartowski/Qwen_Qwen3-4B-GGUF/resolve/main/Qwen3-4B-Q4_K_M.gguf",
		SizeBytes: 2_500_000_000,
		Curated:   true,
	},
	"qwen3-14b": {
		Name:      "qwen3-14b",
		SourceURL: "https://huggingface.co/bartowski/Qwen_Qwen3-14B-GGUF/resolve/main/Qwen3-14B-Q4_K_M.gguf",
		SizeBytes: 9_000_000_000,
		Curated:   true,
	},
	"qwen3-30b": {
		Name:      "qwen3-30b",
		SourceURL: "https://huggingface.co/bartowski/Qwen_Qwen3-30B-A3B-GGUF/resolve/main/Qwen3-30B-A3B-Q4_K_M.gguf",
		SizeBytes: 18_630_000_000,
		Curated:   true,
	},
	// ── Llama ────────────────────────────────────────────────────────────────
	"llama3.2-1b": {
		Name:      "llama3.2-1b",
		SourceURL: "https://huggingface.co/bartowski/Llama-3.2-1B-Instruct-GGUF/resolve/main/Llama-3.2-1B-Instruct-Q4_K_M.gguf",
		SizeBytes: 800_000_000,
		Curated:   true,
	},
	"llama4-scout": {
		Name:      "llama4-scout",
		SourceURL: "https://huggingface.co/bartowski/meta-llama_Llama-4-Scout-17B-16E-Instruct-GGUF/resolve/main/Llama-4-Scout-17B-16E-Instruct-Q4_K_M.gguf",
		SizeBytes: 67_550_000_000,
		Curated:   true,
	},
	// ── Google Gemma 4 ───────────────────────────────────────────────────────
	"gemma4-e2b": {
		Name:      "gemma4-e2b",
		SourceURL: "https://huggingface.co/unsloth/gemma-4-E2B-it-GGUF/resolve/main/Q4_K_M.gguf",
		SizeBytes: 3_340_169_216,
		Curated:   true,
	},
	"gemma4-e4b": {
		Name:      "gemma4-e4b",
		SourceURL: "https://huggingface.co/bartowski/google_gemma-4-E4B-it-GGUF/resolve/main/google_gemma-4-E4B-it-Q4_K_M.gguf",
		SizeBytes: 5_811_128_320,
		Curated:   true,
	},
	// ── Microsoft Phi 4 ──────────────────────────────────────────────────────
	"phi-4-mini": {
		Name:      "phi-4-mini",
		SourceURL: "https://huggingface.co/bartowski/microsoft_Phi-4-mini-instruct-GGUF/resolve/main/microsoft_Phi-4-mini-instruct-Q4_K_M.gguf",
		SizeBytes: 2_674_000_000,
		Curated:   true,
	},
	// ── IBM Granite 3.2 ──────────────────────────────────────────────────────
	"granite-3.2-2b": {
		Name:      "granite-3.2-2b",
		SourceURL: "https://huggingface.co/bartowski/ibm-granite_granite-3.2-2b-instruct-GGUF/resolve/main/granite-3.2-2b-instruct-Q4_K_M.gguf",
		SizeBytes: 1_665_000_000,
		Curated:   true,
	},
	"granite-3.2-8b": {
		Name:      "granite-3.2-8b",
		SourceURL: "https://huggingface.co/bartowski/ibm-granite_granite-3.2-8b-instruct-GGUF/resolve/main/granite-3.2-8b-instruct-Q4_K_M.gguf",
		SizeBytes: 5_303_304_806,
		Curated:   true,
	},
	// ── Moonshot Kimi ─────────────────────────────────────────────────────────
	"kimi-linear": {
		Name:      "kimi-linear",
		SourceURL: "https://huggingface.co/bartowski/moonshotai_Kimi-Linear-48B-A3B-Instruct-GGUF/resolve/main/Kimi-Linear-48B-A3B-Instruct-Q4_K_M.gguf",
		SizeBytes: 30_060_000_000,
		Curated:   true,
	},
	// ── Tiny (testing) ───────────────────────────────────────────────────────
	"tiny": {
		Name:      "tiny",
		SourceURL: "https://huggingface.co/Hjgugugjhuhjggg/FastThink-0.5B-Tiny-Q2_K-GGUF/resolve/main/fastthink-0.5b-tiny-q2_k.gguf",
		SizeBytes: 200_000_000,
		Curated:   true,
	},
}

type familyMapping struct {
	CanonicalName string
	Substrings    []string
}

var defaultFamilies = []familyMapping{
	// Qwen 3 (checked before 2.5 to avoid substring collision)
	{CanonicalName: "qwen3-4b", Substrings: []string{"qwen3-4", "qwen3:4"}},
	{CanonicalName: "qwen3-14b", Substrings: []string{"qwen3-14", "qwen3:14", "qwen3"}},
	{CanonicalName: "qwen3-30b", Substrings: []string{"qwen3-30", "qwen3:30"}},
	// Qwen 2.5
	{CanonicalName: "qwen2.5-1.5b", Substrings: []string{"qwen2.5-1.5", "qwen2.5:1.5"}},
	{CanonicalName: "qwen2.5-7b", Substrings: []string{"qwen2.5-7", "qwen2.5:7"}},
	// Llama
	{CanonicalName: "llama4-scout", Substrings: []string{"llama-4", "llama4"}},
	{CanonicalName: "llama3.2-1b", Substrings: []string{"llama-3.2-1", "llama3.2-1", "llama3.2:1"}},
	// Gemma 4
	{CanonicalName: "gemma4-e4b", Substrings: []string{"gemma-4", "gemma4", "gemma:4"}},
	// Phi 4
	{CanonicalName: "phi-4-mini", Substrings: []string{"phi-4-mini", "phi4-mini", "phi4mini", "phi-4", "phi4"}},
	// Granite
	{CanonicalName: "granite-3.2-8b", Substrings: []string{"granite-3.2-8", "granite-3.1", "granite3.", "granite"}},
	// Kimi
	{CanonicalName: "kimi-linear", Substrings: []string{"kimi"}},
}

const defaultFallback = "tiny"
