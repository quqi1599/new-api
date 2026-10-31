package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/websearch"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
)

var managedWebSlots = make(chan struct{}, 8)

func managedProtocol(info *relaycommon.RelayInfo) string {
	switch info.RelayFormat {
	case types.RelayFormatClaude:
		return "claude"
	case types.RelayFormatOpenAIResponses:
		return "responses"
	case types.RelayFormatGemini:
		return "gemini"
	case types.RelayFormatOpenAI:
		return "chat"
	}
	return ""
}
func webError(err error, status int) *types.NewAPIError {
	var guard *websearch.GuardError
	if errors.As(err, &guard) {
		return types.NewErrorWithStatusCode(err, types.ErrorCode("cpa_content_audit_blocked"), guard.Status, types.ErrOptionWithSkipRetry())
	}
	return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, status, types.ErrOptionWithSkipRetry())
}

// ManagedWebHelper surrounds the existing per-channel relay. Every model round
// retains its normal provider, request conversions, audit and pricing. Only web
// tools are executed locally; they never reach the model provider as hosted tools.
func ManagedWebHelper(c *gin.Context, info *relaycommon.RelayInfo, run func() *types.NewAPIError) *types.NewAPIError {
	if !websearch.Enabled() {
		return run()
	}
	alpha := info.RelayMode == relayconstant.RelayModeAlphaSearch
	protocol := managedProtocol(info)
	if protocol == "" && !alpha {
		return run()
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return webError(err, 400)
	}
	body, err := storage.Bytes()
	if err != nil {
		return webError(err, 500)
	}
	var plan *websearch.Plan
	if !alpha {
		plan, err = websearch.NewPlan(body, protocol)
		if err != nil {
			return webError(err, 400)
		}
		if plan == nil {
			if !bytes.Contains(body, []byte("ws_na_")) {
				return run()
			}
			o, e := websearch.Obj(body)
			if e != nil {
				return webError(e, 400)
			}
			plan = &websearch.Plan{Protocol: protocol, Body: o, Limit: 8}
		}
	}
	if len(body) > 32<<20 {
		return webError(errors.New("managed web request exceeds 32 MiB"), 413)
	}
	select {
	case managedWebSlots <- struct{}{}:
		defer func() { <-managedWebSlots }()
	default:
		return webError(errors.New("managed web search capacity is temporarily exhausted"), 503)
	}
	client, err := websearch.ProductionClient()
	if err != nil {
		return webError(err, 503)
	}
	if common.RDB == nil {
		return webError(errors.New("managed web history storage is unavailable"), 503)
	}
	session := &websearch.Session{Backend: client, Store: websearch.RedisStore{Scope: fmt.Sprintf("%d:%d", info.UserId, info.TokenId)}}
	c.Request = c.Request.WithContext(websearch.WithRequestHook(c.Request.Context(), func(request *http.Request) error { return channel.ApplyManagedWebAudit(request, info) }))
	inputField := "input"
	if protocol == "claude" || protocol == "chat" {
		inputField = "messages"
	}
	if protocol == "gemini" {
		inputField = "contents"
	}
	if input := gjson.GetBytes(body, inputField); input.Exists() {
		c.Request = c.Request.WithContext(websearch.WithAuditInput(c.Request.Context(), json.RawMessage(input.Raw)))
	}
	return runManagedWeb(c, info, run, body, plan, session)
}

func runManagedWeb(c *gin.Context, info *relaycommon.RelayInfo, run func() *types.NewAPIError, body []byte, plan *websearch.Plan, session *websearch.Session) (resultErr *types.NewAPIError) {
	managed, err := service.NewManagedWebBilling(operation_setting.USDExchangeRate)
	if err != nil {
		return webError(err, 503)
	}
	info.ManagedWeb = managed
	originalWriter, originalRequest, originalStream := c.Writer, info.Request, info.IsStream
	stopHeartbeat := func() {}
	if originalStream {
		stopHeartbeat = managedHeartbeat(c.Request.Context(), originalWriter)
	}
	defer stopHeartbeat()
	originalStorage, _ := common.GetBodyStorage(c)
	var replacement common.BodyStorage
	var total dto.Usage
	settled := false
	modelHeadroom := max(info.PriceData.QuotaToPreConsume, 1)
	if info.Billing != nil {
		modelHeadroom = max(modelHeadroom, info.Billing.GetInitialPreConsumedQuota())
	}
	if plan == nil {
		modelHeadroom = 0
	}
	initialTools := info.ResponsesUsageInfo
	combinedTools := &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{}}
	defer func() {
		c.Writer = originalWriter
		info.Request = originalRequest
		info.IsStream = originalStream
		c.Set(common.KeyBodyStorage, originalStorage)
		if replacement != nil {
			replacement.Close()
		}
		info.DeferTextBilling = nil
	}()
	info.DeferTextBilling = func(u *dto.Usage, quota int) {
		managed.ModelRounds++
		managed.ModelQuota = common.QuotaFromDecimal(decimal.NewFromInt(int64(managed.ModelQuota)).Add(decimal.NewFromInt(int64(quota))))
		addManagedUsage(&total, u)
	}
	reserve := func(extra int) error {
		m := *managed
		m.SearchRequests = session.Meter.Search
		m.FetchRequests = session.Meter.Fetch + extra
		target := common.QuotaFromDecimal(decimal.NewFromInt(int64(managed.ModelQuota)).Add(service.ManagedWebQuota(&m)).Add(decimal.NewFromInt(int64(modelHeadroom))))
		if info.Billing == nil {
			if err := service.PreConsumeBilling(c, target, info); err != nil {
				return err
			}
			return nil
		}
		return info.Billing.Reserve(target)
	}
	session.BeforeCall = func() error { return reserve(1) }
	prepareBill := func() error {
		stopHeartbeat()
		managed.SearchRequests = session.Meter.Search
		managed.FetchRequests = session.Meter.Fetch
		info.ResponsesUsageInfo = combinedTools
		managed.Quota = common.QuotaFromDecimal(decimal.NewFromInt(int64(managed.ModelQuota)).Add(service.ManagedWebQuota(managed)))
		if info.Billing != nil {
			if err := info.Billing.Settle(managed.Quota); err != nil {
				managed.SettlementError = err
				return errors.New("managed web billing settlement failed")
			}
		}
		return nil
	}
	settle := func() {
		if settled {
			return
		}
		settled = true
		info.ResponsesUsageInfo = combinedTools
		stopHeartbeat()
		info.DeferTextBilling = nil
		managed.SearchRequests = session.Meter.Search
		managed.FetchRequests = session.Meter.Fetch
		c.Writer = originalWriter
		info.IsStream = originalStream
		service.PostTextConsumeQuota(c, info, &total, []string{"联网由 DeepSeek 执行，搜索与抓取按人民币官方价五倍计费，不叠加分组倍率"})
	}
	defer func() {
		if resultErr != nil && (managed.ModelRounds > 0 || session.Meter.Attempts > 0) {
			managed.Outcome = "failed"
			if c.Request.Context().Err() != nil || errors.Is(resultErr, io.ErrClosedPipe) {
				managed.Outcome = "cancelled"
			}
			settle()
		}
	}()
	if plan == nil {
		response, err := websearch.Alpha(c.Request.Context(), session, body)
		if err != nil {
			return webError(err, 502)
		}
		if err := prepareBill(); err != nil {
			return webError(err, 503)
		}
		response["usage"] = websearch.Raw(map[string]any{"server_tool_use": map[string]int{"web_search_requests": session.Meter.Search, "web_fetch_requests": session.Meter.Fetch}})
		c.Writer = originalWriter
		c.Header("Content-Type", "application/json")
		if _, err := c.Writer.Write(websearch.Raw(response)); err != nil {
			return webError(err, 502)
		}
		managed.Outcome = "completed"
		settle()
		return nil
	}
	if err := plan.Restore(c.Request.Context(), session.Store); err != nil {
		return webError(err, 409)
	}
	if err := plan.Tools(); err != nil {
		return webError(err, 400)
	}
	var collected []json.RawMessage
	collectedBytes := 0
	executedIDs := map[string]bool{}
	for round := 0; round < 16; round++ {
		if err := reserve(0); err != nil {
			return webError(err, 403)
		}
		encoded := websearch.Raw(plan.Body)
		if len(encoded) > 32<<20 {
			return webError(errors.New("managed web tool history exceeds 32 MiB"), 413)
		}
		if replacement != nil {
			replacement.Close()
		}
		replacement, err = common.CreateBodyStorage(encoded)
		if err != nil {
			return webError(err, 503)
		}
		c.Set(common.KeyBodyStorage, replacement)
		c.Request.Body = io.NopCloser(replacement)
		c.Request.ContentLength = int64(len(encoded))
		info.Request, err = helper.GetAndValidateRequest(c, info.RelayFormat)
		if err != nil {
			return webError(err, 400)
		}
		info.IsStream = originalStream
		info.ClaudeConvertInfo = &relaycommon.ClaudeConvertInfo{}
		info.StreamStatus = nil
		info.GenerationOutcome = nil
		info.ResponsesOutcome = nil
		info.PartialStreamError = nil
		info.ResponsesUsageInfo = &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{}}
		if initialTools != nil {
			for name, tool := range initialTools.BuiltInTools {
				if tool != nil {
					info.ResponsesUsageInfo.BuiltInTools[name] = &relaycommon.BuildInToolInfo{ToolName: tool.ToolName, SearchContextSize: tool.SearchContextSize}
				}
			}
		}
		capture := newWebCapture(c.Request.Context())
		c.Writer = capture
		managed.ModelFailure = true
		relayErr := run()
		capture.close()
		if info.ResponsesUsageInfo != nil {
			for name, tool := range info.ResponsesUsageInfo.BuiltInTools {
				if tool == nil {
					continue
				}
				if combinedTools.BuiltInTools[name] == nil {
					combinedTools.BuiltInTools[name] = &relaycommon.BuildInToolInfo{ToolName: name}
				}
				combinedTools.BuiltInTools[name].CallCount += tool.CallCount
			}
			combinedTools.ImageGenerationCalls = append(combinedTools.ImageGenerationCalls, info.ResponsesUsageInfo.ImageGenerationCalls...)
		}
		if relayErr != nil {
			if managed.ModelRounds == 0 && session.Meter.Attempts == 0 && capture.Size() == 0 {
				return relayErr
			}
			return webError(relayErr, relayErr.StatusCode)
		}
		if captureErr := capture.Err(); captureErr != nil {
			return webError(captureErr, 502)
		}
		if capture.Status() >= 400 {
			return webError(errors.New("model round returned an error status"), capture.Status())
		}
		if info.PartialStreamError != nil {
			return webError(info.PartialStreamError, 502)
		}
		if outcome := info.GenerationOutcome; outcome != nil && (outcome.State == "failed" || outcome.State == "cancelled") {
			return webError(errors.New("model round did not complete"), 502)
		}
		response, err := websearch.Snapshot(plan.Protocol, capture.bytes())
		if err != nil {
			return webError(err, 502)
		}
		calls, err := websearch.Calls(plan.Protocol, response)
		if err != nil {
			return webError(err, 502)
		}
		managed.ModelFailure = false
		var executed []websearch.Executed
		external := false
		for _, call := range calls {
			if !call.Managed() {
				external = true
				continue
			}
			if info.IsResponsesIncomplete() {
				return webError(errors.New("incomplete model tool call was not executed"), 502)
			}
			if executedIDs[call.ID] {
				return webError(errors.New("duplicate web tool call ID was not executed again"), 502)
			}
			executedIDs[call.ID] = true
			e, err := plan.Execute(c.Request.Context(), session, call)
			if err != nil {
				return webError(err, 502)
			}
			executed = append(executed, e)
		}
		if plan.Protocol == "responses" || plan.Protocol == "claude" {
			parts := websearch.PublicParts(plan.Protocol, response, executed)
			for _, part := range parts {
				collectedBytes += len(part)
			}
			if collectedBytes > 32<<20 {
				return webError(errors.New("managed web response history exceeds 32 MiB"), 502)
			}
			collected = append(collected, parts...)
		}
		if len(executed) > 0 && !external {
			if err := plan.Continue(response, executed); err != nil {
				return webError(err, 502)
			}
			continue
		}
		if plan.Protocol == "responses" {
			response["output"] = websearch.Raw(collected)
			if len(plan.OriginalTools) > 0 {
				response["tools"] = plan.OriginalTools
			} else {
				delete(response, "tools")
			}
			if len(plan.OriginalToolChoice) > 0 {
				response["tool_choice"] = plan.OriginalToolChoice
			}
		}
		if plan.Protocol == "claude" {
			response["content"] = websearch.Raw(collected)
		}
		if external && len(executed) > 0 {
			websearch.HideExecutedClientCalls(plan.Protocol, response, executed)
		}
		applyManagedUsage(response, plan.Protocol, &total, session.Meter)
		if err := prepareBill(); err != nil {
			return webError(err, 503)
		}
		c.Writer = originalWriter
		if originalStream {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			if err := websearch.Emit(c.Writer, plan.Protocol, response); err != nil {
				return webError(err, 502)
			}
			c.Writer.Flush()
		} else {
			c.Header("Content-Type", "application/json")
			if _, err := c.Writer.Write(websearch.Raw(response)); err != nil {
				return webError(err, 502)
			}
		}
		managed.Outcome = "completed"
		settle()
		return nil
	}
	return webError(errors.New("managed web model round limit exceeded"), 502)
}

func addManagedUsage(total *dto.Usage, part *dto.Usage) {
	if part == nil {
		return
	}
	addUsageFields(reflect.ValueOf(total).Elem(), reflect.ValueOf(part).Elem())
	total.ServerToolUse = nil // Only the platform executor may report web usage.
}
func addUsageFields(dst, src reflect.Value) {
	for i := 0; i < src.NumField(); i++ {
		d, s := dst.Field(i), src.Field(i)
		if src.Type().Field(i).Name == "ServerToolUse" {
			continue
		}
		switch s.Kind() {
		case reflect.Int:
			if s.Int() > 0 && d.Int() <= math.MaxInt64-s.Int() {
				d.SetInt(d.Int() + s.Int())
			}
		case reflect.String:
			if s.String() != "" {
				d.SetString(s.String())
			}
		case reflect.Struct:
			addUsageFields(d, s)
		case reflect.Pointer:
			if s.IsNil() || s.Type().Elem().Kind() != reflect.Struct {
				continue
			}
			if d.IsNil() {
				d.Set(reflect.New(s.Type().Elem()))
			}
			addUsageFields(d.Elem(), s.Elem())
		}
	}
}

func applyManagedUsage(response websearch.Object, protocol string, u *dto.Usage, m websearch.Meter) {
	usage, _ := websearch.Obj(response["usage"])
	if usage == nil {
		usage = websearch.Object{}
	}
	switch protocol {
	case "responses":
		usage["input_tokens"] = websearch.Raw(u.PromptTokens)
		usage["output_tokens"] = websearch.Raw(u.CompletionTokens)
		usage["total_tokens"] = websearch.Raw(u.TotalTokens)
		usage["input_tokens_details"] = websearch.Raw(map[string]int{"cached_tokens": u.PromptTokensDetails.CachedTokens})
		usage["output_tokens_details"] = websearch.Raw(u.CompletionTokenDetails)
	case "claude":
		usage["input_tokens"] = websearch.Raw(u.PromptTokens)
		usage["output_tokens"] = websearch.Raw(u.CompletionTokens)
		usage["cache_read_input_tokens"] = websearch.Raw(u.PromptTokensDetails.CachedTokens)
		usage["cache_creation_input_tokens"] = websearch.Raw(u.PromptTokensDetails.CachedCreationTokens)
	case "gemini":
		response["usageMetadata"] = websearch.Raw(map[string]int{"promptTokenCount": u.PromptTokens, "candidatesTokenCount": u.CompletionTokens, "totalTokenCount": u.TotalTokens})
	default:
		usage, _ = websearch.Obj(websearch.Raw(u))
	}
	usage["server_tool_use"] = websearch.Raw(map[string]int{"web_search_requests": m.Search, "web_fetch_requests": m.Fetch})
	response["usage"] = websearch.Raw(usage)
}

type webCapture struct {
	mu         sync.Mutex
	buffer     bytes.Buffer
	header     http.Header
	status     int
	written    bool
	err        error
	ctx        context.Context
	notified   chan bool
	stopNotify func() bool
}

func newWebCapture(ctx context.Context) *webCapture {
	w := &webCapture{header: make(http.Header), status: 200, ctx: ctx, notified: make(chan bool, 1)}
	w.stopNotify = context.AfterFunc(ctx, func() { w.notified <- true })
	return w
}
func (w *webCapture) Header() http.Header { return w.header }
func (w *webCapture) WriteHeader(n int)   { w.mu.Lock(); defer w.mu.Unlock(); w.status = n }
func (w *webCapture) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buffer.Len()+len(b) > 32<<20 {
		w.err = errors.New("managed web model response exceeds 32 MiB")
		return 0, w.err
	}
	w.written = true
	return w.buffer.Write(b)
}
func (w *webCapture) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *webCapture) Status() int                       { w.mu.Lock(); defer w.mu.Unlock(); return w.status }
func (w *webCapture) Size() int                         { w.mu.Lock(); defer w.mu.Unlock(); return w.buffer.Len() }
func (w *webCapture) Written() bool                     { w.mu.Lock(); defer w.mu.Unlock(); return w.written }
func (w *webCapture) WriteHeaderNow()                   { w.mu.Lock(); w.written = true; w.mu.Unlock() }
func (w *webCapture) Flush()                            { w.WriteHeaderNow() }
func (w *webCapture) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("managed web search does not hijack connections")
}
func (w *webCapture) CloseNotify() <-chan bool {
	return w.notified
}
func (w *webCapture) close() {
	if w.stopNotify != nil {
		w.stopNotify()
	}
}
func (w *webCapture) Pusher() http.Pusher { return nil }
func (w *webCapture) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
}

func managedHeartbeat(ctx context.Context, w gin.ResponseWriter) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				if _, err := io.WriteString(w, ": searching\n\n"); err != nil {
					return
				}
				w.Flush()
			}
		}
	}()
	return func() { once.Do(func() { close(stop); <-done }) }
}

func (w *webCapture) Err() error { w.mu.Lock(); defer w.mu.Unlock(); return w.err }
