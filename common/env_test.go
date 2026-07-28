package common

import (
	"reflect"
	"testing"
)

func TestParseTokenRPMRateLimits(t *testing.T) {
	got, err := ParseTokenRPMRateLimits("15777:100, 42:20")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int{15777: 100, 42: 20}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseTokenRPMRateLimitsRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"15777", "0:100", "15777:0", "15777:100,15777:50"} {
		if _, err := ParseTokenRPMRateLimits(value); err == nil {
			t.Fatalf("expected %q to fail", value)
		}
	}
}
