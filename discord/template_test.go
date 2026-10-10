package discord

import "testing"

func TestUnmarshalComponentsPrunesEmptyText(t *testing.T) {
	raw := `[{"type":10,"content":""},{"type":17,"components":[{"type":10,"content":"  "}]},{"type":17,"components":[{"type":10,"content":"hi"},{"type":10,"content":""}]}]`
	got, err := unmarshalComponents(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 component, got %d", len(got))
	}
}
