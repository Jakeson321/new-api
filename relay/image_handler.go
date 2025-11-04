package relay

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func ImageHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	startTime := time.Now()
	logger.LogInfo(c, "#ImageHelper#start, tokenId:"+string(info.TokenId)+", userId:"+string(info.UserId))

	info.InitChannelMeta(c)

	imageReq, ok := info.Request.(*dto.ImageRequest)
	if !ok {
		return types.NewErrorWithStatusCode(fmt.Errorf("invalid request type, expected dto.ImageRequest, got %T", info.Request), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	request, err := common.DeepCopy(imageReq)
	if err != nil {
		return types.NewError(fmt.Errorf("failed to copy request to ImageRequest: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	deepCopyTime := time.Now()
	logger.LogInfo(c, "#ImageHelper#deep copy, tokenId:"+string(info.TokenId)+", userId:"+string(info.UserId)+", timeCost:"+(deepCopyTime.Sub(startTime)/1000).String())

	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)

	var requestBody io.Reader

	if model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled {
		body, err := common.GetRequestBody(c)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		requestBody = bytes.NewBuffer(body)
	} else {
		convertedRequest, err := adaptor.ConvertImageRequest(c, info, *request)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed)
		}

		switch convertedRequest.(type) {
		case *bytes.Buffer:
			requestBody = convertedRequest.(io.Reader)
		default:
			jsonData, err := common.Marshal(convertedRequest)
			if err != nil {
				return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
			}

			// apply param override
			if len(info.ParamOverride) > 0 {
				jsonData, err = relaycommon.ApplyParamOverride(jsonData, info.ParamOverride)
				if err != nil {
					return types.NewError(err, types.ErrorCodeChannelParamOverrideInvalid, types.ErrOptionWithSkipRetry())
				}
			}

			if common.DebugEnabled {
				logger.LogDebug(c, fmt.Sprintf("image request body: %s", string(jsonData)))
			}
			requestBody = bytes.NewBuffer(jsonData)
		}
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")

	requestStartTime := time.Now()
	logger.LogInfo(c, "#ImageHelper#start request, tokenId:"+string(info.TokenId)+", userId:"+string(info.UserId)+", timeCost:"+(requestStartTime.Sub(deepCopyTime)/1000).String())
	resp, err := adaptor.DoRequest(c, info, requestBody)
	requestEndTime := time.Now()
	logger.LogInfo(c, "#ImageHelper#end request, tokenId:"+string(info.TokenId)+", userId:"+string(info.UserId)+", timeCost:"+(requestEndTime.Sub(requestStartTime)/1000).String())

	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		if httpResp.StatusCode != http.StatusOK {
			newAPIError = service.RelayErrorHandler(c.Request.Context(), httpResp, false)
			// reset status code 重置状态码
			service.ResetStatusCode(newAPIError, statusCodeMappingStr)
			return newAPIError
		}
	}

	usage, newAPIError := adaptor.DoResponse(c, httpResp, info)
	if newAPIError != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return newAPIError
	}

	if usage.(*dto.Usage).TotalTokens == 0 {
		usage.(*dto.Usage).TotalTokens = int(request.N)
	}
	if usage.(*dto.Usage).PromptTokens == 0 {
		usage.(*dto.Usage).PromptTokens = int(request.N)
	}

	quality := "standard"
	if request.Quality == "hd" {
		quality = "hd"
	}

	dealRespTime := time.Now()
	logger.LogInfo(c, "#ImageHelper#deal resp, tokenId:"+string(info.TokenId)+", userId:"+string(info.UserId)+", timeCost:"+(dealRespTime.Sub(requestEndTime)/1000).String())

	var logContent string
	if len(request.Size) > 0 {
		logContent = fmt.Sprintf("大小 %s, 品质 %s, 张数 %d", request.Size, quality, request.N)

		// 添加图片张数和大小信息
		imageCount, imageSizeInfo := getImageCountAndSizeInfo(c)
		if imageCount > 0 {
			logContent += fmt.Sprintf(", 输入图片 %d 张", imageCount)
			if imageSizeInfo != "" {
				logContent += fmt.Sprintf(" (%s)", imageSizeInfo)
			}
		}
	}

	postConsumeQuota(c, info, usage.(*dto.Usage), logContent)
	return nil
}

// getImageCountAndSizeInfo 获取图片张数和大小信息
func getImageCountAndSizeInfo(c *gin.Context) (int, string) {
	mf := c.Request.MultipartForm
	if mf == nil {
		if _, err := c.MultipartForm(); err != nil {
			return 0, ""
		}
		mf = c.Request.MultipartForm
	}

	var imageFiles []*multipart.FileHeader
	var exists bool

	// First check for standard "image" field
	if imageFiles, exists = mf.File["image"]; !exists || len(imageFiles) == 0 {
		// If not found, check for "image[]" field
		if imageFiles, exists = mf.File["image[]"]; !exists || len(imageFiles) == 0 {
			// If still not found, iterate through all fields to find any that start with "image["
			foundArrayImages := false
			for fieldName, files := range mf.File {
				if strings.HasPrefix(fieldName, "image[") && len(files) > 0 {
					foundArrayImages = true
					imageFiles = append(imageFiles, files...)
				}
			}

			// If no image fields found at all
			if !foundArrayImages && (len(imageFiles) == 0) {
				return 0, ""
			}
		}
	}

	if len(imageFiles) == 0 {
		return 0, ""
	}

	// 计算图片大小信息
	var totalSize int64
	var sizeInfo string

	for _, file := range imageFiles {
		totalSize += file.Size
	}

	// 格式化大小信息
	if totalSize > 0 {
		if totalSize < 1024 {
			sizeInfo = fmt.Sprintf("%d B", totalSize)
		} else if totalSize < 1024*1024 {
			sizeInfo = fmt.Sprintf("%.1f KB", float64(totalSize)/1024)
		} else {
			sizeInfo = fmt.Sprintf("%.1f MB", float64(totalSize)/(1024*1024))
		}
	}

	return len(imageFiles), sizeInfo
}
