package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"golang.org/x/image/webp"
)

// return image.Config, format, clean base64 string, error
func DecodeBase64ImageData(base64String string) (image.Config, string, string, error) {
	// 去除base64数据的URL前缀（如果有）
	if idx := strings.Index(base64String, ","); idx != -1 {
		base64String = base64String[idx+1:]
	}

	if len(base64String) == 0 {
		return image.Config{}, "", "", errors.New("base64 string is empty")
	}

	// 将base64字符串解码为字节切片
	decodedData, err := base64.StdEncoding.DecodeString(base64String)
	if err != nil {
		fmt.Println("Error: Failed to decode base64 string")
		return image.Config{}, "", "", fmt.Errorf("failed to decode base64 string: %s", err.Error())
	}

	// 创建一个bytes.Buffer用于存储解码后的数据
	reader := bytes.NewReader(decodedData)
	config, format, err := getImageConfig(reader)
	return config, format, base64String, err
}

func DecodeBase64FileData(base64String string) (string, string, error) {
	var mimeType string
	var idx int
	idx = strings.Index(base64String, ",")
	if idx == -1 {
		_, file_type, base64, err := DecodeBase64ImageData(base64String)
		return "image/" + file_type, base64, err
	}
	mimeType = base64String[:idx]
	base64String = base64String[idx+1:]
	idx = strings.Index(mimeType, ";")
	if idx == -1 {
		_, file_type, base64, err := DecodeBase64ImageData(base64String)
		return "image/" + file_type, base64, err
	}
	mimeType = mimeType[:idx]
	idx = strings.Index(mimeType, ":")
	if idx == -1 {
		_, file_type, base64, err := DecodeBase64ImageData(base64String)
		return "image/" + file_type, base64, err
	}
	mimeType = mimeType[idx+1:]
	return mimeType, base64String, nil
}

type imageDownloadFunc func(string) (*http.Response, error)

func getImageFromURL(url string, download imageDownloadFunc) (mimeType string, data string, err error) {
	resp, err := download(url)
	if err != nil {
		return "", "", fmt.Errorf("failed to download image: %w", err)
	}
	defer resp.Body.Close()

	// Check HTTP status code
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("failed to download image: HTTP %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/octet-stream" && !strings.HasPrefix(contentType, "image/") {
		return "", "", fmt.Errorf("invalid content type: %s, required image/*", contentType)
	}
	maxImageSize := int64(constant.MaxFileDownloadMB * 1024 * 1024)

	// Check Content-Length if available
	if resp.ContentLength > maxImageSize {
		return "", "", fmt.Errorf("image size %d exceeds maximum allowed size of %d bytes", resp.ContentLength, maxImageSize)
	}

	// Use LimitReader to prevent reading oversized images
	limitReader := io.LimitReader(resp.Body, maxImageSize)
	buffer := &bytes.Buffer{}

	written, err := io.Copy(buffer, limitReader)
	if err != nil {
		return "", "", fmt.Errorf("failed to read image data: %w", err)
	}
	if written >= maxImageSize {
		return "", "", fmt.Errorf("image size exceeds maximum allowed size of %d bytes", maxImageSize)
	}

	data = base64.StdEncoding.EncodeToString(buffer.Bytes())
	mimeType = contentType

	// Handle application/octet-stream type
	if mimeType == "application/octet-stream" {
		_, format, _, err := DecodeBase64ImageData(data)
		if err != nil {
			return "", "", err
		}
		mimeType = "image/" + format
	}

	return mimeType, data, nil
}

// GetImageFromURLContext downloads an image using the caller context without
// imposing a relay-specific local deadline.
func GetImageFromURLContext(ctx context.Context, url string) (mimeType string, data string, err error) {
	return getImageFromURL(url, func(url string) (*http.Response, error) {
		return DoDownloadRequestContext(ctx, url, "get_image_data")
	})
}

// GetImageFromURLWithRelayInfo shares the relay's absolute request budget.
func GetImageFromURLWithRelayInfo(c *gin.Context, info *relaycommon.RelayInfo, url string) (mimeType string, data string, err error) {
	return getImageFromURL(url, func(url string) (*http.Response, error) {
		return DoDownloadRequestWithRelayInfo(c, info, url, "get_image_data")
	})
}

// GetImageFromUrl is retained as a Background-based compatibility wrapper.
func GetImageFromUrl(url string) (mimeType string, data string, err error) {
	return GetImageFromURLContext(context.Background(), url)
}

func decodeURLImageData(imageURL string, download imageDownloadFunc) (image.Config, string, error) {
	response, err := download(imageURL)
	if err != nil {
		common.SysLog(fmt.Sprintf("fail to get image from url: %s", err.Error()))
		return image.Config{}, "", err
	}
	defer response.Body.Close()

	if response.StatusCode != 200 {
		err = errors.New(fmt.Sprintf("fail to get image from url: %s", response.Status))
		return image.Config{}, "", err
	}

	mimeType := response.Header.Get("Content-Type")

	if mimeType != "application/octet-stream" && !strings.HasPrefix(mimeType, "image/") {
		return image.Config{}, "", fmt.Errorf("invalid content type: %s, required image/*", mimeType)
	}

	var readData []byte
	for _, limit := range []int64{1024 * 8, 1024 * 24, 1024 * 64} {
		common.SysLog(fmt.Sprintf("try to decode image config with limit: %d", limit))

		// 从response.Body读取更多的数据直到达到当前的限制
		additionalData := make([]byte, limit-int64(len(readData)))
		n, readErr := io.ReadFull(response.Body, additionalData)
		readData = append(readData, additionalData[:n]...)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return image.Config{}, "", fmt.Errorf("failed to read image data: %w", readErr)
		}

		// 使用io.MultiReader组合已经读取的数据和response.Body
		limitReader := io.MultiReader(bytes.NewReader(readData), response.Body)

		var config image.Config
		var format string
		config, format, err = getImageConfig(limitReader)
		if err == nil {
			return config, format, nil
		}
		var apiErr *types.NewAPIError
		if errors.As(err, &apiErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return image.Config{}, "", err
		}
	}

	return image.Config{}, "", err // 返回最后一个错误
}

func DecodeURLImageDataContext(ctx context.Context, imageURL string) (image.Config, string, error) {
	return decodeURLImageData(imageURL, func(url string) (*http.Response, error) {
		return DoDownloadRequestContext(ctx, url, "decode_image_data")
	})
}

func DecodeURLImageDataWithRelayInfo(c *gin.Context, info *relaycommon.RelayInfo, imageURL string) (image.Config, string, error) {
	return decodeURLImageData(imageURL, func(url string) (*http.Response, error) {
		return DoDownloadRequestWithRelayInfo(c, info, url, "decode_image_data")
	})
}

// DecodeUrlImageData is retained as a Background-based compatibility wrapper.
func DecodeUrlImageData(imageURL string) (image.Config, string, error) {
	return DecodeURLImageDataContext(context.Background(), imageURL)
}

func getImageConfig(reader io.Reader) (image.Config, string, error) {
	// Read all data so we can retry with different decoders
	data, readErr := io.ReadAll(reader)
	if readErr != nil {
		return image.Config{}, "", fmt.Errorf("failed to read image data: %w", readErr)
	}

	// 读取图片的头部信息来获取图片尺寸
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err == nil {
		return config, format, nil
	}
	common.SysLog(fmt.Sprintf("fail to decode image config(gif, jpg, png): %s", err.Error()))

	config, err = webp.DecodeConfig(bytes.NewReader(data))
	if err == nil {
		return config, "webp", nil
	}
	common.SysLog(fmt.Sprintf("fail to decode image config(webp): %s", err.Error()))

	// Try HEIF/HEIC: parse ISOBMFF ispe box for dimensions
	if heifMime := detectHEIF(data); heifMime != "" {
		formatName := "heif"
		if heifMime == "image/heic" {
			formatName = "heic"
		}
		if w, h, ok := parseHEIFDimensions(data); ok {
			return image.Config{Width: w, Height: h}, formatName, nil
		}
		return image.Config{}, "", fmt.Errorf("failed to decode HEIF/HEIC image dimensions")
	}

	return image.Config{}, "", err
}
