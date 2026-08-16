package relay

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
)

func TestBuildAlphaSearchRequestBodyPreservesRawBytesWithoutModelMapping(t *testing.T) {
	raw := []byte("{\n  \"model\": \"gpt-5.5\", \"query\": \"weather\", \"future\": {\"n\": 1e3}\n}")

	got, err := buildAlphaSearchRequestBody(raw, "gpt-5.5", "gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("unmapped body changed:\n got: %s\nwant: %s", got, raw)
	}
}

func TestBuildAlphaSearchRequestBodyChangesOnlyTopLevelModel(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","query":"weather","future":{"n":1e3},"items":[true,null,"x"]}`)

	got, err := buildAlphaSearchRequestBody(raw, "gpt-5.5", "gpt-5.6")
	if err != nil {
		t.Fatal(err)
	}

	var original map[string]json.RawMessage
	var mapped map[string]json.RawMessage
	if err := common.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	if err := common.Unmarshal(got, &mapped); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := common.Unmarshal(mapped["model"], &model); err != nil {
		t.Fatal(err)
	}
	if model != "gpt-5.6" {
		t.Fatalf("mapped model = %q", model)
	}
	delete(original, "model")
	delete(mapped, "model")
	if len(original) != len(mapped) {
		t.Fatalf("field count changed: got %d want %d", len(mapped), len(original))
	}
	for key, want := range original {
		if gotField := mapped[key]; !bytes.Equal(gotField, want) {
			t.Fatalf("field %q changed: got %s want %s", key, gotField, want)
		}
	}
}

func TestBuildAlphaSearchRequestBodyRejectsEmptyBody(t *testing.T) {
	if _, err := buildAlphaSearchRequestBody(nil, "gpt-5.5", "gpt-5.6"); err == nil {
		t.Fatal("expected empty body error")
	}
}
