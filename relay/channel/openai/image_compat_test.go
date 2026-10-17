package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const imageUsageFixture = `{"input_tokens":15,"prompt_tokens":15,"output_tokens":1352,"completion_tokens":1352,"input_tokens_details":{"text_tokens":10,"image_tokens":5,"cached_tokens":3},"output_tokens_details":{"image_tokens":1120,"text_tokens":232}}`

func imageCompatContext(t *testing.T, body, contentType string, stream *bool, edit bool) (*gin.Context, *httptest.ResponseRecorder, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	mode := relayconstant.RelayModeImagesGenerations
	if edit {
		mode = relayconstant.RelayModeImagesEdits
		c.Request.URL.Path = "/v1/images/edits"
	}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, RelayMode: mode,
		IsStream:    (stream != nil && *stream) || strings.Contains(contentType, "event-stream"),
		RelayFormat: types.RelayFormatOpenAI, Request: &dto.ImageRequest{Model: "gpt-image-2", Stream: stream}}
	return c, w, resp, info
}

func TestImagesUsageNormalizationAndClientFormat(t *testing.T) {
	for _, edit := range []bool{false, true} {
		name := "image_generation"
		if edit {
			name = "image_edit"
		}
		for _, sse := range []bool{false, true} {
			for _, requestStream := range []*bool{nil, common.GetPointer(false), common.GetPointer(true)} {
				label := "absent"
				if requestStream != nil {
					label = fmt.Sprint(*requestStream)
				}
				t.Run(fmt.Sprintf("%s/upstreamSSE=%t/stream=%s", name, sse, label), func(t *testing.T) {
					body := `{"created":123,"data":[{"b64_json":"complete"}],"quality":"high","size":"1024x1024","output_format":"png","usage":` + imageUsageFixture + `}`
					contentType := "application/json"
					if sse {
						body = "event: " + name + ".partial_image\ndata: {\"type\":\"" + name + ".partial_image\",\"b64_json\":\"partial\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20}}\n\n" +
							"event: " + name + ".completed\ndata: {\"type\":\"" + name + ".completed\",\"created_at\":123,\"b64_json\":\"complete\",\"quality\":\"high\",\"output_format\":\"png\",\"size\":\"1024x1024\",\"usage\":" + imageUsageFixture + "}\n\n"
						contentType = "text/event-stream"
					}
					c, w, resp, info := imageCompatContext(t, body, contentType, requestStream, edit)
					info.PriceData.UsePrice = true
					info.PriceData.AddOtherRatio("n", 3)
					result, err := (&Adaptor{}).DoResponse(c, resp, info)
					require.Nil(t, err)
					usage := result.(*dto.Usage)
					require.Equal(t, 15, usage.PromptTokens, "aliases and repeated snapshots must not be added")
					require.Equal(t, 1352, usage.CompletionTokens)
					require.Equal(t, 1367, usage.TotalTokens)
					require.Equal(t, 3, usage.PromptTokensDetails.CachedTokens)
					require.Equal(t, 1120, usage.CompletionTokenDetails.ImageTokens)
					require.Equal(t, 3.0, info.PriceData.OtherRatios()["n"], "stream frames must not multiply the image count")
					params := service.BuildTieredTokenParams(usage, false, map[string]bool{"img_o": true})
					require.Equal(t, 1120.0, params.ImgO)
					require.Equal(t, 232.0, params.C, "separately priced image output must leave only text output in c")
					require.Equal(t, 1352.0, service.BuildTieredTokenParams(usage, false, nil).C)
					if requestStream != nil && *requestStream {
						require.Contains(t, w.Header().Get("Content-Type"), "text/event-stream")
						require.Contains(t, w.Body.String(), "event: "+name+".completed")
						require.Contains(t, w.Body.String(), "data: [DONE]")
					} else {
						require.Contains(t, w.Header().Get("Content-Type"), "application/json")
						require.NotContains(t, w.Body.String(), "partial")
						require.NotContains(t, w.Body.String(), "[DONE]")
					}
					require.Contains(t, w.Body.String(), `"b64_json":"complete"`)
					require.Contains(t, w.Body.String(), `"quality":"high"`)
				})
			}
		}
	}
}

func TestImagesStreamFailureNeverSynthesizesSuccess(t *testing.T) {
	partial := "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"partial\",\"usage\":" + imageUsageFixture + "}\n\n"
	for _, suffix := range []string{"", "data: [DONE]\n\n", "data: not-json\n\n", "data: {\"type\":\"unknown\"}\n\n", "event: error\ndata: {\"message\":\"failed\"}\n\ndata: [DONE]\n\n"} {
		t.Run(suffix, func(t *testing.T) {
			c, w, resp, info := imageCompatContext(t, partial+suffix, "text/event-stream", common.GetPointer(true), false)
			result, err := (&Adaptor{}).DoResponse(c, resp, info)
			require.NotNil(t, err)
			require.True(t, types.IsSkipRetryError(err))
			require.Same(t, err, info.PartialStreamError)
			require.Equal(t, 1352, result.(*dto.Usage).CompletionTokens)
			require.NotContains(t, w.Body.String(), "[DONE]")
			require.NotContains(t, w.Body.String(), "image_generation.completed")
		})
	}
	c, w, resp, info := imageCompatContext(t, "data: {\"type\":\"unknown\"}\n\ndata: [DONE]\n\n", "text/event-stream", common.GetPointer(true), false)
	_, err := (&Adaptor{}).DoResponse(c, resp, info)
	require.NotNil(t, err)
	require.True(t, types.IsSkipRetryError(err))
	require.Empty(t, w.Body.String())
	require.Nil(t, info.PartialStreamError)
}

func TestImageEditMultipartReplayPreservesFilesAndFields(t *testing.T) {
	for _, stream := range []string{"true", "false", "invalid"} {
		t.Run(stream, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			for key, value := range map[string]string{"model": "gpt-image-2", "prompt": "edit", "stream": stream, "n": "2", "partial_images": "0", "output_compression": "0", "quality": "high"} {
				require.NoError(t, mw.WriteField(key, value))
			}
			for key, value := range map[string]string{"image[]": "image bytes", "mask": "mask bytes"} {
				part, err := mw.CreateFormFile(key, key+".png")
				require.NoError(t, err)
				_, err = io.WriteString(part, value)
				require.NoError(t, err)
			}
			require.NoError(t, mw.Close())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
			c.Request.Header.Set("Content-Type", mw.FormDataContentType())
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			request, err := helper.GetAndValidOpenAIImageRequest(c, relayconstant.RelayModeImagesEdits)
			if stream == "invalid" {
				require.ErrorContains(t, err, "invalid stream value")
				return
			}
			require.NoError(t, err)
			require.Equal(t, stream == "true", request.IsStream(c))
			// Simulate another parser consuming the request. Repeated conversion
			// also replaces Content-Type with a new multipart boundary each time.
			_, err = io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeImagesEdits}
			for i := 0; i < 2; i++ {
				converted, err := (&Adaptor{}).ConvertImageRequest(c, info, *request)
				require.NoError(t, err)
				out := httptest.NewRequest(http.MethodPost, "/v1/images/edits", converted.(io.Reader))
				out.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
				require.NoError(t, out.ParseMultipartForm(1<<20))
				require.Equal(t, stream, out.FormValue("stream"))
				require.Equal(t, "0", out.FormValue("partial_images"))
				require.Equal(t, "0", out.FormValue("output_compression"))
				require.Equal(t, "gpt-image-2", out.FormValue("model"))
				for key, expected := range map[string]string{"image": "image bytes", "mask": "mask bytes"} {
					file, _, err := out.FormFile(key)
					require.NoError(t, err)
					data, err := io.ReadAll(file)
					require.NoError(t, err)
					require.NoError(t, file.Close())
					require.Equal(t, expected, string(data))
				}
				require.NoError(t, out.MultipartForm.RemoveAll())
			}
		})
	}
}

func TestImagesDownstreamFailureKeepsKnownUsageAndNoDone(t *testing.T) {
	for _, sse := range []bool{false, true} {
		for _, allowed := range []int{0, 1} {
			t.Run(fmt.Sprintf("upstreamSSE=%t/allowWrites=%d", sse, allowed), func(t *testing.T) {
				body := `{"data":[{"b64_json":"complete"}],"usage":` + imageUsageFixture + `}`
				contentType := "application/json"
				if sse {
					body = "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"complete\",\"usage\":" + imageUsageFixture + "}\n\ndata: [DONE]\n\n"
					contentType = "text/event-stream"
				}
				_, _, resp, info := imageCompatContext(t, body, contentType, common.GetPointer(true), false)
				w := &postOutputDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), allowWrites: allowed}
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
				result, err := (&Adaptor{}).DoResponse(c, resp, info)
				require.NotNil(t, err)
				require.Equal(t, 499, err.StatusCode)
				require.True(t, types.IsSkipRetryError(err))
				require.False(t, types.IsChannelPenaltyAllowed(err))
				require.Same(t, err, info.PartialStreamError)
				require.Equal(t, 1352, result.(*dto.Usage).CompletionTokens)
				require.Equal(t, relaycommon.StreamEndReasonClientGone, info.StreamStatus.EndReason)
				require.NotContains(t, w.Body.String(), "[DONE]")
			})
		}
	}
}

func TestImagesExplicitZeroUsageRemainsZero(t *testing.T) {
	usage, err := parseOpenAIImageResponse([]byte(`{"data":[{"b64_json":"image"}],"usage":{"input_tokens":0,"prompt_tokens":12,"output_tokens":0,"completion_tokens":30,"total_tokens":0}}`))
	require.Nil(t, err)
	require.Zero(t, usage.PromptTokens)
	require.Zero(t, usage.CompletionTokens)
	require.Zero(t, usage.TotalTokens)
}

func TestImagesRejectEmptySuccessAndInvalidCompleted(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":[]}`, `{"data":[{}]}`, `{"data":[{"url":""}]}`} {
		for _, stream := range []bool{false, true} {
			c, w, resp, info := imageCompatContext(t, body, "application/json", common.GetPointer(stream), false)
			_, err := (&Adaptor{}).DoResponse(c, resp, info)
			require.NotNil(t, err, body)
			require.True(t, types.IsSkipRetryError(err))
			require.Empty(t, w.Body.String())
		}
	}
	for _, body := range []string{
		`{"type":"image_generation.completed","usage":{"input_tokens":1}}`,
		`{"type":"image_generation.completed","data":[{}]}`,
		`{"type":"unknown","usage":{"input_tokens":1}}`,
	} {
		c, w, resp, info := imageCompatContext(t, "data: "+body+"\n\ndata: [DONE]\n\n", "text/event-stream", common.GetPointer(true), false)
		_, err := (&Adaptor{}).DoResponse(c, resp, info)
		require.NotNil(t, err, body)
		require.True(t, types.IsSkipRetryError(err))
		require.Empty(t, w.Body.String())
	}
}

func TestImagesJSONWriteFailureKeepsConfirmedUsage(t *testing.T) {
	for _, sse := range []bool{false, true} {
		body := `{"data":[{"b64_json":"complete"}],"usage":` + imageUsageFixture + `}`
		contentType := "application/json"
		if sse {
			body = "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"complete\",\"usage\":" + imageUsageFixture + "}\n\n"
			contentType = "text/event-stream"
		}
		_, _, resp, info := imageCompatContext(t, body, contentType, common.GetPointer(false), false)
		w := &postOutputDisconnectWriter{ResponseRecorder: httptest.NewRecorder()}
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		result, err := (&Adaptor{}).DoResponse(c, resp, info)
		require.NotNil(t, err)
		require.Equal(t, 499, err.StatusCode)
		require.False(t, types.IsChannelPenaltyAllowed(err))
		require.Same(t, err, info.PartialStreamError)
		require.Equal(t, 1352, result.(*dto.Usage).CompletionTokens)
		count, known := ConfirmedImageCount(c)
		require.True(t, known)
		require.Equal(t, 1, count)
	}
}

func TestImagesCompletedRetransmissionsAreNotCountedTwice(t *testing.T) {
	completed := "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"same\",\"usage\":" + imageUsageFixture + "}\n\n"
	c, _, resp, info := imageCompatContext(t, completed+completed+"data: broken\n\n", "text/event-stream", common.GetPointer(true), false)
	_, err := (&Adaptor{}).DoResponse(c, resp, info)
	require.NotNil(t, err)
	count, known := ConfirmedImageCount(c)
	require.True(t, known)
	require.Equal(t, 1, count)
}

func TestImagesUsageSnapshotRetainsKnownFields(t *testing.T) {
	body := "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"partial\",\"usage\":{\"input_tokens\":15,\"output_tokens\":100}}\n\n" +
		"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"complete\",\"usage\":{\"output_tokens\":200}}\n\n"
	c, _, resp, info := imageCompatContext(t, body, "text/event-stream", common.GetPointer(true), false)
	result, err := (&Adaptor{}).DoResponse(c, resp, info)
	require.Nil(t, err)
	require.Equal(t, 15, result.(*dto.Usage).PromptTokens)
	require.Equal(t, 200, result.(*dto.Usage).CompletionTokens)
}

func TestImageSSEDecoderBoundsEmptyDataLines(t *testing.T) {
	decoder := &imageSSEDecoder{}
	for i := 0; i < 4096; i++ {
		_, err := decoder.Feed("data:")
		require.NoError(t, err)
	}
	_, err := decoder.Feed("data:")
	require.Error(t, err)
}

type imageProgressWriter struct {
	*httptest.ResponseRecorder
	progress chan struct{}
	once     sync.Once
}

func (w *imageProgressWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if bytes.Contains(p, []byte("partial_image")) {
		w.once.Do(func() { close(w.progress) })
	}
	return n, err
}

func TestImageHTTPStreamsPartialBeforeUpstreamCompletes(t *testing.T) {
	progress := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"partial\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-progress:
			_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"complete\"}\n\n")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	writer := &imageProgressWriter{ResponseRecorder: httptest.NewRecorder(), progress: progress}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil).WithContext(ctx)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, IsStream: true, RelayFormat: types.RelayFormatOpenAI}
	_, apiErr := OpenaiImageStreamHandler(c, info, response)
	require.Nil(t, apiErr, "upstream waits for downstream partial delivery before completing")
	require.Contains(t, writer.Body.String(), `"b64_json":"complete"`)
	require.Contains(t, writer.Body.String(), "[DONE]")
}

func TestImageStreamBoundsCompletedCountAndUsageMetadata(t *testing.T) {
	for _, streamOutput := range []bool{false, true} {
		var body strings.Builder
		for i := 0; i <= dto.MaxImageN; i++ {
			fmt.Fprintf(&body, "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"image%d\"}\n\n", i)
		}
		c, w, resp, info := imageCompatContext(t, body.String(), "text/event-stream", common.GetPointer(streamOutput), false)
		_, err := (&Adaptor{}).DoResponse(c, resp, info)
		require.NotNil(t, err)
		require.True(t, types.IsSkipRetryError(err))
		require.NotContains(t, w.Body.String(), "[DONE]")
		count, known := ConfirmedImageCount(c)
		require.True(t, known)
		require.Equal(t, dto.MaxImageN, count)
	}
	body := "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"preview\"}\n\n" +
		"data: {\"usage\":{\"vendor\":\"" + strings.Repeat("x", maxImageUsageBytes) + "\"}}\n\n"
	c, w, resp, info := imageCompatContext(t, body, "text/event-stream", common.GetPointer(true), false)
	_, err := (&Adaptor{}).DoResponse(c, resp, info)
	require.NotNil(t, err)
	require.True(t, types.IsSkipRetryError(err))
	require.NotContains(t, w.Body.String(), "[DONE]")
}

func TestImagesStableIndexDeduplicatesAndUnknownUsageIsNotReported(t *testing.T) {
	body := "data: {\"type\":\"image_generation.completed\",\"image_index\":0,\"b64_json\":\"one\",\"usage\":{\"vendor\":123}}\n\n" +
		"data: {\"type\":\"image_generation.completed\",\"image_index\":0,\"b64_json\":\"updated\",\"usage\":{}}\n\n"
	c, _, resp, info := imageCompatContext(t, body, "text/event-stream", common.GetPointer(true), false)
	_, err := (&Adaptor{}).DoResponse(c, resp, info)
	require.Nil(t, err)
	count, known := ConfirmedImageCount(c)
	require.True(t, known)
	require.Equal(t, 1, count)
	require.False(t, ImageUsageReported(c))
	for _, raw := range []string{`{}`, `{"vendor":123}`, `{"input_tokens_details":{"vendor":1}}`} {
		require.Empty(t, knownImageUsageFields([]byte(raw)))
	}
	require.NotEmpty(t, knownImageUsageFields([]byte(`{"input_tokens":0}`)))
}

func TestImageUsageDetailsMergeMissingAndExplicitZero(t *testing.T) {
	body := "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"partial\",\"usage\":{\"input_tokens\":15,\"output_tokens\":20,\"input_tokens_details\":{\"text_tokens\":10,\"image_tokens\":5,\"cached_tokens\":3},\"output_tokens_details\":{\"text_tokens\":2,\"image_tokens\":18}}}\n\n" +
		"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"complete\",\"usage\":{\"input_tokens_details\":{\"cached_tokens\":4,\"text_tokens\":0},\"output_tokens_details\":{\"text_tokens\":0}}}\n\n"
	c, _, resp, info := imageCompatContext(t, body, "text/event-stream", common.GetPointer(true), false)
	result, err := (&Adaptor{}).DoResponse(c, resp, info)
	require.Nil(t, err)
	usage := result.(*dto.Usage)
	require.Equal(t, 15, usage.PromptTokens)
	require.Equal(t, 5, usage.PromptTokensDetails.ImageTokens)
	require.Equal(t, 4, usage.PromptTokensDetails.CachedTokens)
	require.Zero(t, usage.PromptTokensDetails.TextTokens)
	require.Equal(t, 18, usage.CompletionTokenDetails.ImageTokens)
	require.Zero(t, usage.CompletionTokenDetails.TextTokens)
}
