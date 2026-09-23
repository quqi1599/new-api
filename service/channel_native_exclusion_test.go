package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func TestNativePreferenceCircuitFallback(t *testing.T) {
	oldDB, oldCache, oldRedis := model.DB, common.MemoryCacheEnabled, common.RedisEnabled
	oldConfig, oldNow := defaultChannelCircuitConfig, channelCircuitNow
	oldMaster, oldSQLitePath := common.IsMasterNode, common.SQLitePath
	oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	t.Cleanup(func() {
		model.DB, common.MemoryCacheEnabled, common.RedisEnabled = oldDB, oldCache, oldRedis
		defaultChannelCircuitConfig, channelCircuitNow = oldConfig, oldNow
		common.IsMasterNode, common.SQLitePath = oldMaster, oldSQLitePath
		common.SetMainDatabaseType(oldMainType)
		common.SetLogDatabaseType(oldLogType)
		resetLocalChannelCircuitsForTest()
	})
	common.MemoryCacheEnabled, common.RedisEnabled = false, false
	defaultChannelCircuitConfig = channelCircuitConfig{FailureThreshold: 1, FailureWindow: time.Minute, OpenDuration: time.Minute, HalfOpenLease: time.Second}
	channelCircuitNow = func() time.Time { return time.Unix(1_700_000_000, 0) }
	resetLocalChannelCircuitsForTest()
	t.Setenv("SQL_DSN", "local")
	common.IsMasterNode = false
	common.SQLitePath = filepath.Join(t.TempDir(), "native-circuit.db")
	// Use the normal initialization path to establish portable column quoting.
	if err := model.InitDB(); err != nil {
		t.Fatal(err)
	}
	db := model.DB
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	model.DB = db
	if err := db.AutoMigrate(&model.Channel{}, &model.Ability{}); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []model.Channel{
		{Id: 9, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-5.6-sol"},
		{Id: 10, Type: constant.ChannelTypeAzure, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-5.6-sol"},
	} {
		if err := db.Create(&channel).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.Ability{Group: channel.Group, Model: channel.Models, ChannelId: channel.Id, Enabled: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	param := &RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol", PreferredChannelTypes: []int{constant.ChannelTypeOpenAI}}
	upstreamErr := types.NewErrorWithStatusCode(errors.New("upstream unavailable"), types.ErrorCodeBadResponse, http.StatusBadGateway)
	if !RecordChannelCircuitFailure(context.Background(), 9, param.ModelName, upstreamErr) {
		t.Fatal("native circuit must be open")
	}
	selected, _, err := CacheGetRandomSatisfiedChannel(param)
	if err != nil || selected == nil || selected.Id != 10 {
		t.Fatalf("circuit fallback=%v err=%v", selected, err)
	}
	if !slices.Equal(param.CircuitSkippedIds, []int{9}) || !slices.Contains(param.ExcludedChannelIds, 9) || param.CircuitRetryAfter <= 0 {
		t.Fatalf("native circuit diagnostics lost: %#v", param)
	}
	// Once every native route is skipped, fallback still cannot override a
	// request-lifetime/token exclusion or a hard endpoint capability gate.
	for _, exclusion := range []string{"persistent", "token", "endpoint"} {
		t.Run(exclusion, func(t *testing.T) {
			next := &RetryParam{Ctx: c, TokenGroup: "default", ModelName: param.ModelName, PreferredChannelTypes: param.PreferredChannelTypes}
			switch exclusion {
			case "persistent":
				next.AddPersistentExcludedChannel(10)
			case "token":
				common.SetContextKey(c, constant.ContextKeyTokenExcludedChannels, []int{10})
				defer c.Set(string(constant.ContextKeyTokenExcludedChannels), nil)
			case "endpoint":
				next.RequiredEndpointType = constant.EndpointTypeOpenAIAlphaSearch
			}
			selected, _, err := CacheGetRandomSatisfiedChannel(next)
			if err != nil || selected != nil {
				t.Fatalf("fallback bypassed %s gate: selected=%v err=%v", exclusion, selected, err)
			}
		})
	}
}
