package common

import "github.com/gin-gonic/gin"

const imageBillingOutcomeKey = "relay_image_billing_outcome"

// ImageBillingOutcome contains adaptor-confirmed complete images, not partial
// previews or requested images. It is scoped to one inbound relay request.
type ImageBillingOutcome struct {
	CompletedImages int
	Failed          bool
}

func SetImageBillingOutcome(c *gin.Context, completedImages int, failed bool) {
	if c == nil || completedImages < 0 {
		return
	}
	c.Set(imageBillingOutcomeKey, ImageBillingOutcome{CompletedImages: completedImages, Failed: failed})
}

func GetImageBillingOutcome(c *gin.Context) (ImageBillingOutcome, bool) {
	if c == nil {
		return ImageBillingOutcome{}, false
	}
	value, exists := c.Get(imageBillingOutcomeKey)
	outcome, valid := value.(ImageBillingOutcome)
	return outcome, exists && valid && outcome.CompletedImages >= 0
}
