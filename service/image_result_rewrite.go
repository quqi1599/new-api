package service

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RewriteImageResultURLs 只替换图片结果字段，保留 usage、数量、模型、提示词及 Base64。
// 下载失败发生在独立 GET 上，不作为生成失败返回，避免渠道重试和重复扣费。
func RewriteImageResultURLs(c *gin.Context, body []byte) []byte {
	cache := imageResultCache.Load()
	if cache == nil || !gjson.ValidBytes(body) {
		return body
	}
	var paths []string
	requestPath := ""
	if c != nil && c.Request != nil {
		requestPath = c.Request.URL.Path
	}
	isImages := strings.HasSuffix(requestPath, "/images/generations") || strings.HasSuffix(requestPath, "/images/edits") || requestPath == "/v1/edits"
	eventType := gjson.GetBytes(body, "type").String()
	if strings.HasSuffix(eventType, ".partial_image") {
		return body
	}
	if isImages || eventType == "image_generation.completed" || eventType == "image_edit.completed" {
		paths = append(paths, "url")
		for index := range gjson.GetBytes(body, "data").Array() {
			paths = append(paths, fmt.Sprintf("data.%d.url", index))
		}
	}
	// Responses 只处理 image_generation_call；文本、引用和参考图 URL 保持原样。
	addOutput := func(prefix string, output gjson.Result) {
		if output.Get("type").String() != "image_generation_call" {
			return
		}
		status := output.Get("status").String()
		if status != "" && status != "completed" {
			return
		}
		paths = append(paths, prefix+"result", prefix+"url", prefix+"result.url")
	}
	for _, prefix := range []string{"", "response."} {
		for index, output := range gjson.GetBytes(body, prefix+"output").Array() {
			addOutput(fmt.Sprintf("%soutput.%d.", prefix, index), output)
		}
	}
	if eventType == "response.output_item.done" {
		addOutput("item.", gjson.GetBytes(body, "item"))
	}
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if value.Type != gjson.String || !validImageResultURL(value.String()) {
			continue
		}
		link, err := cache.link(value.String())
		if err != nil {
			common.SysError("image_result_cache link_creation_failed")
			continue
		}
		updated, err := sjson.SetBytes(body, path, link)
		if err == nil {
			body = updated
			if c != nil && link != value.String() {
				id := cache.id(value.String())
				common.SysLog(fmt.Sprintf("image_result_cache linked id=%s request_id=%s", id[:16], c.GetString(common.InternalRequestIdKey)))
			}
		}
	}
	return body
}
