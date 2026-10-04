package controller

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/tidwall/gjson"
)

// Admin-only opt-in probe. Automatic channel checks keep their existing payload.
func addChannelVisionProbe(request dto.Request) error {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	const prompt = "What single color fills this image? Reply with one English color word only."
	limit := uint(128)
	switch req := request.(type) {
	case *dto.OpenAIResponsesRequest:
		input, err := common.Marshal([]any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + data}, map[string]any{"type": "input_text", "text": prompt}}}})
		if err != nil {
			return err
		}
		req.Input = input
		req.MaxOutputTokens = &limit
	case *dto.GeneralOpenAIRequest:
		var message dto.Message
		message.Role = "user"
		message.SetMediaContent([]dto.MediaContent{{Type: dto.ContentTypeImageURL, ImageUrl: &dto.MessageImageUrl{Url: "data:image/png;base64," + data}}, {Type: dto.ContentTypeText, Text: prompt}})
		req.Messages = []dto.Message{message}
		req.MaxTokens = &limit
	case *dto.ClaudeRequest:
		req.Messages = []dto.ClaudeMessage{{Role: "user", Content: []dto.ClaudeMediaMessage{{Type: "image", Source: &dto.ClaudeMessageSource{Type: "base64", MediaType: "image/png", Data: data}}, {Type: "text", Text: common.GetPointer(prompt)}}}}
		req.MaxTokens = &limit
	default:
		return errors.New("vision probe supports chat completions, Responses and Claude Messages endpoints")
	}
	return nil
}

func validateChannelVisionAnswer(body []byte, stream bool) error {
	var text strings.Builder
	if stream {
		for _, line := range bytes.Split(body, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			item := gjson.ParseBytes(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
			switch item.Get("type").String() {
			case "response.output_text.delta":
				text.WriteString(item.Get("delta").String())
			case "content_block_delta":
				if item.Get("delta.type").String() == "text_delta" {
					text.WriteString(item.Get("delta.text").String())
				}
			default:
				text.WriteString(item.Get("choices.0.delta.content").String())
			}
		}
	} else {
		item := gjson.ParseBytes(body)
		text.WriteString(item.Get("choices.0.message.content").String())
		for _, part := range item.Get("content").Array() {
			if part.Get("type").String() == "text" {
				text.WriteString(part.Get("text").String())
			}
		}
		for _, message := range item.Get("output").Array() {
			for _, part := range message.Get("content").Array() {
				if part.Get("type").String() == "output_text" {
					text.WriteString(part.Get("text").String())
				}
			}
		}
	}
	if strings.Trim(strings.ToLower(strings.TrimSpace(text.String())), ".\"'*! \n\r") != "red" {
		return errors.New("vision probe did not identify the synthetic image color")
	}
	return nil
}
