package constant

const (
	APITypeOpenAI = iota
	APITypeAnthropic
	APITypePaLM
	APITypeBaidu
	APITypeZhipu
	APITypeAli
	APITypeXunfei
	APITypeAIProxyLibrary
	APITypeTencent
	APITypeGemini
	APITypeZhipuV4
	APITypeOllama
	APITypePerplexity
	APITypeAws
	APITypeCohere
	APITypeDify
	APITypeJina
	APITypeCloudflare
	APITypeSiliconFlow
	APITypeVertexAi
	APITypeMistral
	APITypeDeepSeek
	APITypeMokaAI
	APITypeVolcEngine
	APITypeBaiduV2
	APITypeOpenRouter
	APITypeXinference
	APITypeXai
	APITypeCoze
	APITypeJimeng
	APITypeMoonshot
	APITypeSubmodel
	APITypeMiniMax
	APITypeReplicate
	APITypeCodex
	APITypeAdvancedCustom
	// APITypeApiNebula is the ApiNebula image-task provider. Unlike the
	// sync-only OpenAI adapter, its generation/edit operations are async_task:
	// submit returns a task id and the processor polls for completion. The
	// image adapter registry uses this to declare the hard execution boundary.
	APITypeApiNebula
	APITypeDummy // this one is only for count, do not add any channel after this
)
