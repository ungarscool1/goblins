package hello

import "testing"

func TestName(t *testing.T) {
	if Name() != "hello" {
		t.Fatal(Name())
	}
}
