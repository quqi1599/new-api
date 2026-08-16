package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestGetChannelQueryForChannelsKeepsCapabilityFilterOnRetry(t *testing.T) {
	oldDB := DB
	oldGroupCol := commonGroupCol
	t.Cleanup(func() {
		DB = oldDB
		commonGroupCol = oldGroupCol
	})

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	DB = db
	commonGroupCol = "`group`"
	if err := DB.AutoMigrate(&Ability{}); err != nil {
		t.Fatal(err)
	}
	high := int64(100)
	low := int64(0)
	abilities := []Ability{
		{Group: "default", Model: "gpt-5.5", ChannelId: 1, Enabled: true, Priority: &high},
		{Group: "default", Model: "gpt-5.5", ChannelId: 2, Enabled: true, Priority: &low},
		{Group: "default", Model: "gpt-5.5", ChannelId: 9, Enabled: true, Priority: &low},
	}
	if err := DB.Create(&abilities).Error; err != nil {
		t.Fatal(err)
	}

	query, err := getChannelQueryForChannels("default", "gpt-5.5", 1, []int{9})
	if err != nil {
		t.Fatal(err)
	}
	var got []Ability
	if err := query.Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ChannelId != 9 {
		t.Fatalf("retry query escaped capability filter: %#v", got)
	}
}
