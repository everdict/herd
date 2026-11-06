package deploy

import "testing"

// A job passes -egress-allow-prefix from a variable that may be empty; that must start, meaning no allowance.
func TestListIgnoresAnEmptyValue(t *testing.T) {
	var l list
	for _, v := range []string{"", "100.77.85.27/32", ""} {
		if err := l.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(l) != 1 || l[0] != "100.77.85.27/32" {
		t.Fatalf("list = %v", l)
	}
}
