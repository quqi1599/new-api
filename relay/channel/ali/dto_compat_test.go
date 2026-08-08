package ali

import (
	"github.com/QuantumNous/new-api/dto"

	"github.com/gin-gonic/gin"
)

var (
	_ func(*AliOutput, *gin.Context, string) []dto.ImageData = (*AliOutput).ChoicesToOpenAIImageDate
	_ func(*AliOutput, *gin.Context, string) []dto.ImageData = (*AliOutput).ResultToOpenAIImageDate
)
