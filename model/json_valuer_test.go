package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONColumnValuersUseStringsForPostgreSQLSimpleProtocol(t *testing.T) {
	channelValue, err := (ChannelInfo{IsMultiKey: true}).Value()
	require.NoError(t, err)
	require.IsType(t, "", channelValue)

	propertiesValue, err := (Properties{Input: "prompt"}).Value()
	require.NoError(t, err)
	require.IsType(t, "", propertiesValue)

	privateValue, err := (TaskPrivateData{UpstreamTaskID: "upstream-task"}).Value()
	require.NoError(t, err)
	require.IsType(t, "", privateValue)

	jsonValue, err := JSONValue(`{"model":"test"}`).Value()
	require.NoError(t, err)
	require.IsType(t, "", jsonValue)
}

func TestJSONColumnScannersAcceptStringsAndBytes(t *testing.T) {
	var channel ChannelInfo
	require.NoError(t, channel.Scan(`{"is_multi_key":true}`))
	require.True(t, channel.IsMultiKey)

	var properties Properties
	require.NoError(t, properties.Scan([]byte(`{"input":"prompt"}`)))
	require.Equal(t, "prompt", properties.Input)
	require.NoError(t, properties.Scan(`{"input":"updated"}`))
	require.Equal(t, "updated", properties.Input)

	var privateData TaskPrivateData
	require.NoError(t, privateData.Scan([]byte(`{"upstream_task_id":"first"}`)))
	require.Equal(t, "first", privateData.UpstreamTaskID)
	require.NoError(t, privateData.Scan(`{"upstream_task_id":"second"}`))
	require.Equal(t, "second", privateData.UpstreamTaskID)
}
