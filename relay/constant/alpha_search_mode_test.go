package constant

import "testing"

func TestPath2RelayModeRecognizesStandaloneAlphaSearch(t *testing.T) {
	if got := Path2RelayMode("/v1/alpha/search"); got != RelayModeAlphaSearch {
		t.Fatalf("relay mode = %d, want %d", got, RelayModeAlphaSearch)
	}
	if RelayModeAlphaSearch == RelayModeResponses || RelayModeAlphaSearch == RelayModeResponsesCompact {
		t.Fatal("alpha search must use a distinct relay mode")
	}
}
