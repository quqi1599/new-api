package ali

import (
	"github.com/QuantumNous/new-api/dto"
	"github.com/samber/lo"
)

// https://help.aliyun.com/document_detail/613695.html?spm=a2c4g.2399480.0.0.1adb778fAdzP9w#341800c0f8w0r

const EnableSearchModelSuffix = "-internet"

func requestOpenAI2Ali(request dto.GeneralOpenAIRequest, upstreamModelName string) *dto.GeneralOpenAIRequest {
	modelName := upstreamModelName
	if modelName == "" {
		modelName = request.Model
	}
	if !dto.IsQwenThinkingBudgetModel(modelName) {
		request.ThinkingBudget = nil
	}

	// DashScope rejects the 0 and 1 boundaries. Clamp only explicit values;
	// omitted top_p must keep the model's own default behavior.
	if request.TopP != nil {
		if *request.TopP >= 1 {
			request.TopP = lo.ToPtr(0.99)
		} else if *request.TopP <= 0 {
			request.TopP = lo.ToPtr(0.01)
		}
	}
	return &request
}
