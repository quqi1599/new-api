package openai

// Adapted from QuantumNous/new-api PR #4608 (d2576ddcd31ff752c30b54d1781e802e4021f824)
// and #7410 (e2bb05f70ed6b6725a83ff6f1c97dbd2c2f640d4), as present in
// d032204733db31208f10bac118b6e683aab3bf29. Local stream acceptance, failure
// and settlement rules remain authoritative; pricing/count policies are unchanged.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const imageCompletedCountKey = "openai_image_confirmed_count"
const imageReportedUsageKey = "openai_image_reported_usage"
const maxImageUsageBytes = 64 << 10

func ConfirmedImageCount(c *gin.Context) (int, bool) {
	value, exists := c.Get(imageCompletedCountKey)
	count, ok := value.(int)
	return count, exists && ok
}

func ImageUsageReported(c *gin.Context) bool { return c.GetBool(imageReportedUsageKey) }

func recordImagePayloads(c *gin.Context, data gjson.Result, seen map[[32]byte]struct{}, positional bool) error {
	for index, item := range data.Array() {
		value := item.Get("b64_json")
		if value.Type != gjson.String || value.String() == "" {
			value = item.Get("url")
		}
		if value.Type != gjson.String || value.String() == "" {
			continue
		}
		// Identical retransmitted completed events do not charge another image.
		// Provider indexes distinguish intentionally identical images when given.
		identity := "content:" + value.String()
		if imageIndex := item.Get("image_index"); imageIndex.Exists() {
			identity = "index:" + imageIndex.Raw
		} else if positional {
			identity = fmt.Sprintf("position:%d:%s", index, value.String())
		}
		seen[sha256.Sum256([]byte(identity))] = struct{}{}
		if len(seen) > dto.MaxImageN {
			return fmt.Errorf("image response exceeds maximum image count")
		}
	}
	c.Set(imageCompletedCountKey, len(seen))
	return nil
}

func OpenaiImageHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	body, err := readOpenAIImageJSON(resp.Body)
	if err != nil {
		return nil, imageResponseError(err)
	}
	usage, apiErr := parseOpenAIImageResponse(body)
	if apiErr != nil {
		return nil, apiErr
	}
	c.Set(imageReportedUsageKey, len(knownImageUsageFields([]byte(gjson.GetBytes(body, "usage").Raw))) > 0)
	if err := recordImagePayloads(c, gjson.GetBytes(body, "data"), map[[32]byte]struct{}{}, true); err != nil {
		return usage, imageResponseError(err)
	}
	applyUsagePostProcessing(info, usage, body)
	if err := writeOpenAIImageJSON(c, info, resp, body); err != nil {
		return usage, err
	}
	return usage, nil
}

func readOpenAIImageJSON(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, helper.DefaultMaxScannerBufferSize+1))
	if err == nil && len(data) > helper.DefaultMaxScannerBufferSize {
		return nil, fmt.Errorf("image response exceeds maximum buffered size")
	}
	return data, err
}

func imageResponseError(err error) *types.NewAPIError {
	// Headers have already arrived: another attempt could generate a second image.
	return types.NewErrorWithStatusCode(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway,
		types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty())
}

func parseOpenAIImageResponse(body []byte) (*dto.Usage, *types.NewAPIError) {
	var response dto.SimpleResponse
	if err := common.Unmarshal(body, &response); err != nil {
		return nil, imageResponseError(err)
	}
	if response.Error != nil {
		if upstreamErr := response.GetOpenAIError(); upstreamErr != nil {
			return nil, types.WithOpenAIError(*upstreamErr, http.StatusBadGateway,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty())
		}
		return nil, imageResponseError(fmt.Errorf("upstream image response contains an error"))
	}
	if !imagePayloadPresent(gjson.GetBytes(body, "data")) {
		return nil, imageResponseError(fmt.Errorf("image response contains no images"))
	}
	normalizeOpenAIImageUsage(&response.Usage, gjson.GetBytes(body, "usage").Raw)
	return &response.Usage, nil
}

func imagePayloadPresent(data gjson.Result) bool {
	for _, item := range data.Array() {
		for _, field := range []string{"b64_json", "url"} {
			value := item.Get(field)
			if value.Type == gjson.String && value.String() != "" {
				return true
			}
		}
	}
	return false
}

// Images usage is an alias of canonical prompt/completion usage, not an
// additional charge. Presence checks preserve explicit zero values, and copying
// the full detail structs keeps image, text and cached input counts together.
func normalizeOpenAIImageUsage(usage *dto.Usage, raw string) {
	if gjson.Get(raw, "input_tokens").Exists() {
		usage.PromptTokens = usage.InputTokens
	}
	if gjson.Get(raw, "output_tokens").Exists() {
		usage.CompletionTokens = usage.OutputTokens
	}
	if usage.InputTokensDetails != nil {
		usage.PromptTokensDetails = *usage.InputTokensDetails
	}
	if usage.OutputTokensDetails != nil {
		usage.CompletionTokenDetails = *usage.OutputTokensDetails
	}
	if !gjson.Get(raw, "total_tokens").Exists() {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
}

// Keep cumulative accounting bounded to the numeric fields supported by the
// existing DTO. Unknown vendor metadata is forwarded but never accumulated.
func knownImageUsageFields(raw []byte) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage)
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "input_tokens", "output_tokens", "prompt_cache_hit_tokens"} {
		value := gjson.GetBytes(raw, key)
		if value.Type == gjson.Number {
			result[key] = json.RawMessage(value.Raw)
		}
	}
	for _, key := range []string{"prompt_tokens_details", "completion_tokens_details", "input_tokens_details", "output_tokens_details"} {
		details := make(map[string]json.RawMessage)
		for _, field := range []string{"cached_tokens", "cached_creation_tokens", "text_tokens", "audio_tokens", "image_tokens", "reasoning_tokens"} {
			value := gjson.GetBytes(raw, key+"."+field)
			if value.Type == gjson.Number {
				details[field] = json.RawMessage(value.Raw)
			}
		}
		if len(details) > 0 {
			result[key], _ = common.Marshal(details)
		}
	}
	return result
}

// This event-aware decoder preserves event names, multi-line data and a clean
// EOF marker. The latter permits native Images completed events without [DONE]
// while retaining failure for a stream that ends after partial images only.
type imageSSEDecoder struct {
	event string
	data  []string
	size  int
}

func (d *imageSSEDecoder) take() []helper.StreamFrame {
	if len(d.data) == 0 {
		d.event = ""
		return nil
	}
	frames := []helper.StreamFrame{{Event: d.event, Data: strings.Join(d.data, "\n")}}
	d.event, d.data, d.size = "", nil, 0
	return frames
}

func (d *imageSSEDecoder) Feed(line string) ([]helper.StreamFrame, error) {
	if line == "" {
		return d.take(), nil
	}
	if strings.HasPrefix(line, "event:") {
		d.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	}
	if strings.HasPrefix(line, "data:") {
		payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		// Preserve the local compatibility rule for gateways that duplicate data:.
		if strings.HasPrefix(payload, "data:") {
			payload, _ = helper.NormalizeSSEPayload(payload)
		}
		d.size += len(payload) + 1
		if len(d.data) >= 4096 {
			return nil, fmt.Errorf("image stream event exceeds maximum data line count")
		}
		if d.size > helper.DefaultMaxScannerBufferSize {
			return nil, fmt.Errorf("image stream event exceeds maximum size")
		}
		d.data = append(d.data, payload)
	}
	return nil, nil
}

func (d *imageSSEDecoder) Flush() ([]helper.StreamFrame, error) {
	return append(d.take(), helper.StreamFrame{Kind: "eof"}), nil
}

func OpenaiImageStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, imageResponseError(fmt.Errorf("invalid image response"))
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return openaiImageJSONAsStreamHandler(c, info, resp)
	}
	return openaiImageSSEHandler(c, info, resp, true)
}

func openaiImageSSEHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response, streamOutput bool) (*dto.Usage, *types.NewAPIError) {
	if !streamOutput {
		previousDisablePing := info.DisablePing
		info.DisablePing = true
		defer func() { info.DisablePing = previousDisablePing }()
	}
	usage := &dto.Usage{}
	var lastUsage []byte
	var completedData []json.RawMessage
	var completedBody []byte
	var aggregateSize int
	usageFields := map[string]json.RawMessage{}
	seenImages := map[[32]byte]struct{}{}
	c.Set(imageCompletedCountKey, 0)
	c.Set(imageReportedUsageKey, false)
	var failure *types.NewAPIError
	completed := false
	helper.StreamScannerHandlerWithDecoder(c, resp, info, &imageSSEDecoder{}, func(frame helper.StreamFrame, sr *helper.StreamResult) {
		if frame.Kind == "eof" || strings.TrimSpace(frame.Data) == "[DONE]" {
			if completed {
				sr.Done()
			} else if frame.Kind != "eof" {
				sr.Stop(fmt.Errorf("image stream ended without a completed image"))
			}
			return
		}
		raw := []byte(frame.Data)
		var event struct {
			Type  string          `json:"type"`
			Usage json.RawMessage `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if err := common.Unmarshal(raw, &event); err != nil {
			sr.Stop(fmt.Errorf("invalid image stream JSON: %w", err))
			return
		}
		name := event.Type
		if name == "" {
			name = frame.Event
		}
		if len(event.Usage) > 0 && !bytes.Equal(bytes.TrimSpace(event.Usage), []byte("null")) {
			if len(event.Usage) > maxImageUsageBytes {
				sr.Stop(fmt.Errorf("image stream usage exceeds maximum size"))
				return
			}
			var validated dto.Usage
			if err := common.Unmarshal(event.Usage, &validated); err != nil {
				sr.Stop(fmt.Errorf("invalid image stream usage: %w", err))
				return
			}
			fields := knownImageUsageFields(event.Usage)
			for key, value := range fields {
				if strings.HasSuffix(key, "_details") && len(usageFields[key]) > 0 {
					var previous, incoming map[string]json.RawMessage
					_ = common.Unmarshal(usageFields[key], &previous)
					_ = common.Unmarshal(value, &incoming)
					for field, count := range incoming {
						previous[field] = count
					}
					value, _ = common.Marshal(previous)
				}
				usageFields[key] = value
			}
			if len(fields) > 0 {
				c.Set(imageReportedUsageKey, true)
			}
			merged, err := common.Marshal(usageFields)
			if err != nil {
				sr.Stop(err)
				return
			}
			if len(merged) > maxImageUsageBytes {
				sr.Stop(fmt.Errorf("cumulative image usage exceeds maximum size"))
				return
			}
			var next dto.Usage
			if err := common.Unmarshal(merged, &next); err != nil {
				sr.Stop(fmt.Errorf("invalid image stream usage: %w", err))
				return
			}
			normalizeOpenAIImageUsage(&next, string(merged))
			usage = &next
			lastUsage = raw
		}
		hasError := len(event.Error) > 0 && !bytes.Equal(bytes.TrimSpace(event.Error), []byte("null"))
		if name == "error" || name == "upstream_error" || hasError {
			failure = imageResponseError(fmt.Errorf("upstream image stream returned an error event"))
			// Preserve upstream error data only once output has started; otherwise
			// the controller can return an ordinary non-retryable error response.
			if streamOutput && helper.StreamStarted(c) {
				if err := writeOpenAIImageEvent(c, name, frame.Data); err != nil {
					failure = helper.DownstreamStreamError(c, info, err)
				}
			}
			sr.Stop(fmt.Errorf("upstream image stream returned an error event"))
			return
		}
		isCompleted := name == "image_generation.completed" || name == "image_edit.completed"
		isPartial := name == "image_generation.partial_image" || name == "image_edit.partial_image"
		if (!isCompleted && !isPartial && (name != "" || len(event.Usage) == 0)) ||
			(isCompleted && !imagePayloadPresent(gjson.ParseBytes(raw)) && !imagePayloadPresent(gjson.GetBytes(raw, "data"))) {
			sr.Stop(fmt.Errorf("invalid image stream event"))
			return
		}
		if !sr.Accept() {
			return
		}
		if isCompleted {
			var countErr error
			if imagePayloadPresent(gjson.GetBytes(raw, "data")) {
				countErr = recordImagePayloads(c, gjson.GetBytes(raw, "data"), seenImages, true)
			} else {
				countErr = recordImagePayloads(c, gjson.ParseBytes(raw), seenImages, false)
			}
			if countErr != nil {
				sr.Stop(countErr)
				return
			}
		}
		if streamOutput {
			if err := writeOpenAIImageEvent(c, name, frame.Data); err != nil {
				failure = helper.DownstreamStreamError(c, info, err)
				sr.Stop(err)
				return
			}
		} else if isCompleted {
			aggregateSize += len(raw)
			if aggregateSize > helper.DefaultMaxScannerBufferSize || len(completedData) >= dto.MaxImageN {
				sr.Stop(fmt.Errorf("buffered image response exceeds maximum size or image count"))
				return
			}
			completedBody = raw
			if data := gjson.GetBytes(raw, "data"); data.IsArray() {
				if len(completedData)+len(data.Array()) > dto.MaxImageN {
					sr.Stop(fmt.Errorf("buffered image response exceeds maximum image count"))
					return
				}
				for _, item := range data.Array() {
					completedData = append(completedData, json.RawMessage(item.Raw))
				}
			} else if gjson.GetBytes(raw, "b64_json").String() != "" || gjson.GetBytes(raw, "url").String() != "" {
				item := raw
				for _, key := range []string{"type", "created_at", "usage", "background", "output_format", "quality", "size"} {
					item, _ = sjson.DeleteBytes(item, key)
				}
				completedData = append(completedData, json.RawMessage(item))
			}
		}
		completed = completed || isCompleted
	})
	applyUsagePostProcessing(info, usage, lastUsage)
	if failure != nil {
		if helper.StreamStarted(c) && info.ReceivedResponseCount > 0 {
			info.PartialStreamError = failure
		}
		return usage, failure
	}
	if err := helper.PreOutputStreamError(c, info); err != nil {
		return usage, err
	}
	if err := helper.PostOutputStreamError(c, info); err != nil {
		return usage, err
	}
	if !streamOutput {
		if len(completedData) == 0 {
			return usage, imageResponseError(fmt.Errorf("image stream contains no completed images"))
		}
		result := make(map[string]json.RawMessage)
		for _, field := range []string{"background", "output_format", "quality", "size"} {
			if value := gjson.GetBytes(completedBody, field); value.Exists() {
				result[field] = json.RawMessage(value.Raw)
			}
		}
		created := gjson.GetBytes(completedBody, "created_at")
		if !created.Exists() {
			created = gjson.GetBytes(completedBody, "created")
		}
		if created.Exists() {
			result["created"] = json.RawMessage(created.Raw)
		}
		result["data"], _ = common.Marshal(completedData)
		if len(usageFields) > 0 {
			result["usage"], _ = common.Marshal(usageFields)
		}
		body, err := common.Marshal(result)
		if err != nil {
			return usage, imageResponseError(err)
		}
		resp.Header.Set("Content-Type", "application/json")
		if err := writeOpenAIImageJSON(c, info, resp, body); err != nil {
			return usage, err
		}
	} else if helper.ShouldFinalizeStream(info) {
		if err := helper.StringData(c, "[DONE]"); err != nil {
			return usage, helper.DownstreamStreamError(c, info, err)
		}
	}
	return usage, nil
}

func writeOpenAIImageEvent(c *gin.Context, name, data string) error {
	if name == "image_generation.completed" || name == "image_edit.completed" {
		data = string(service.RewriteImageResultURLs(c, []byte(data)))
	}
	data = strings.ReplaceAll(data, "\n", "\ndata: ")
	if name == "" {
		return helper.StringData(c, data)
	}
	return helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: name}, data)
}

// Copy the JSON response with explicit delivery errors. The generic copy helper
// logs write failures but does not return them, which would mark this request as
// successful even after the downstream connection has gone away.
func writeOpenAIImageJSON(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response, body []byte) *types.NewAPIError {
	body = service.RewriteImageResultURLs(c, body)
	for key, values := range resp.Header {
		if len(values) > 0 && service.ShouldCopyUpstreamHeader(c, key, values) {
			c.Header(key, values[0])
		}
	}
	c.Header("Content-Length", fmt.Sprint(len(body)))
	c.Status(resp.StatusCode)
	info.SetFirstResponseTime()
	info.ReceivedResponseCount++
	var err error
	if err = c.Request.Context().Err(); err == nil {
		_, err = c.Writer.Write(body)
	}
	if err == nil {
		err = helper.FlushWriter(c)
	}
	if err == nil {
		return nil
	}
	status, reason, origin := 499, relaycommon.StreamEndReasonClientGone, constant.RelayCancelOriginDownstreamDisconnected
	if errors.Is(c.Request.Context().Err(), context.DeadlineExceeded) {
		status, reason, origin = http.StatusGatewayTimeout, relaycommon.StreamEndReasonRequestDeadline, constant.RelayCancelOriginGatewayDeadline
	}
	common.SetContextKey(c, constant.ContextKeyRelayCancelOrigin, origin)
	// The scanner, if used, has already joined every goroutine at this point.
	previous := info.StreamStatus
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.CopyErrorsFrom(previous)
	info.StreamStatus.SetEndReason(reason, err)
	apiErr := types.NewErrorWithStatusCode(fmt.Errorf("image response delivery failed: %w", err), types.ErrorCodeDoRequestFailed, status, types.ErrOptionWithSkipRetry())
	info.PartialStreamError = apiErr
	return apiErr
}

func openaiImageJSONAsStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	body, err := readOpenAIImageJSON(resp.Body)
	if err != nil {
		return nil, imageResponseError(err)
	}
	usage, apiErr := parseOpenAIImageResponse(body)
	if apiErr != nil {
		return nil, apiErr
	}
	c.Set(imageReportedUsageKey, len(knownImageUsageFields([]byte(gjson.GetBytes(body, "usage").Raw))) > 0)
	if err := recordImagePayloads(c, gjson.GetBytes(body, "data"), map[[32]byte]struct{}{}, true); err != nil {
		return usage, imageResponseError(err)
	}
	applyUsagePostProcessing(info, usage, body)
	name := "image_generation.completed"
	if info.RelayMode == relayconstant.RelayModeImagesEdits {
		name = "image_edit.completed"
	}
	created := gjson.GetBytes(body, "created").Int()
	if created == 0 {
		created = time.Now().Unix()
	}
	info.StreamStatus = relaycommon.NewStreamStatus()
	for _, item := range gjson.GetBytes(body, "data").Array() {
		if item.Get("b64_json").String() == "" && item.Get("url").String() == "" {
			continue
		}
		payload, err := common.Marshal(map[string]any{"type": name, "created_at": created})
		if err != nil {
			return usage, imageResponseError(err)
		}
		// Forward raw image and usage fields without decoding/re-encoding large
		// base64 strings or replacing provider metadata with canonical zero fields.
		for _, field := range []string{"background", "output_format", "quality", "size", "usage"} {
			if value := gjson.GetBytes(body, field); value.Exists() {
				payload, err = sjson.SetRawBytes(payload, field, []byte(value.Raw))
				if err != nil {
					return usage, imageResponseError(err)
				}
			}
		}
		for _, field := range []string{"url", "revised_prompt", "b64_json"} {
			if value := item.Get(field); value.Exists() {
				payload, err = sjson.SetRawBytes(payload, field, []byte(value.Raw))
				if err != nil {
					return usage, imageResponseError(err)
				}
			}
		}
		info.SetFirstResponseTime()
		info.ReceivedResponseCount++
		if err := writeOpenAIImageEvent(c, name, string(payload)); err != nil {
			return usage, helper.DownstreamStreamError(c, info, err)
		}
	}
	if info.ReceivedResponseCount == 0 {
		return usage, imageResponseError(fmt.Errorf("image response contains no images"))
	}
	if err := helper.StringData(c, "[DONE]"); err != nil {
		return usage, helper.DownstreamStreamError(c, info, err)
	}
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
	return usage, nil
}
