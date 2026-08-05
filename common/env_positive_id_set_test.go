package common

import "testing"

func TestParsePositiveIDSet(t *testing.T) {
	ids, err := ParsePositiveIDSet("9, 12")
	if err != nil {
		t.Fatalf("expected valid id set: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected two ids, got %d", len(ids))
	}
	if _, ok := ids[9]; !ok {
		t.Fatal("expected id 9")
	}
	if _, ok := ids[12]; !ok {
		t.Fatal("expected id 12")
	}
}

func TestParsePositiveIDSetRejectsInvalidValues(t *testing.T) {
	for _, input := range []string{"0", "-1", "abc", "9,9"} {
		if _, err := ParsePositiveIDSet(input); err == nil {
			t.Fatalf("expected %q to fail", input)
		}
	}
}
