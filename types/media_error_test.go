package types

import (
	"strings"
	"testing"
)

func TestCPAMediaErrorPreservesCustomerActionAndStopsRetryPenalty(t *testing.T) {
	const message = "单张图片文件过大（要求：不超过 20 MB）。请压缩图片后重新上传；如果图片在历史消息中，请新建对话。"
	for _, code := range []string{"request_too_large", "image_too_large", "invalid_image_input"} {
		for _, wire := range []string{"openai", "claude"} {
			var err *NewAPIError
			if wire == "openai" {
				err = WithOpenAIError(OpenAIError{Code: code, Type: "invalid_request_error", Message: message}, 502, ErrOptionWithChannelPenalty())
			} else {
				err = WithClaudeError(ClaudeError{Code: code, Type: "api_error", Message: message}, 502, ErrOptionWithChannelPenalty())
			}
			status := 413
			if code == "invalid_image_input" {
				status = 400
			}
			if err.StatusCode != status || !IsSkipRetryError(err) || IsChannelPenaltyAllowed(err) || err.ToOpenAIError().Message != message || err.ToClaudeError().Message != message {
				t.Fatalf("media error lost safe semantics: %s/%s/%+v", wire, code, err)
			}
			if !strings.Contains(err.Error(), "新建对话") || !strings.Contains(err.Error(), "压缩图片") {
				t.Fatal("client guidance lost")
			}
		}
	}
	unknown := WithOpenAIError(OpenAIError{Code: "invalid_request_error", Message: "image_too_large request_too_large"}, 502, ErrOptionWithChannelPenalty())
	if unknown.StatusCode != 502 || IsSkipRetryError(unknown) || !IsChannelPenaltyAllowed(unknown) {
		t.Fatal("message text incorrectly authorized classification")
	}
}
