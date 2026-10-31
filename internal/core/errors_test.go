package core

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorClassification(t *testing.T) {
	base := errors.New("token=abc123 leaked by library")
	err := fmt.Errorf("outer: %w", Wrap(KindTransient, "fetch", base))

	if got := KindOf(err); got != KindTransient {
		t.Fatalf("KindOf = %q", got)
	}
	if !errors.Is(err, base) {
		t.Fatal("cause lost")
	}
	if !errors.Is(err, &Error{Kind: KindTransient}) {
		t.Fatal("kind sentinel did not match")
	}
	if errors.Is(err, &Error{Kind: KindInvalid}) {
		t.Fatal("wrong kind matched")
	}
	if got := UserMessage(err); got != "internal error" {
		t.Fatalf("UserMessage exposed cause: %q", got)
	}
	if got := UserMessage(Errorf(KindInvalid, "op", "bad %s", "value")); got != "bad value" {
		t.Fatalf("UserMessage = %q", got)
	}
	if KindOf(nil) != "" || KindOf(base) != KindInternal {
		t.Fatal("default kinds wrong")
	}
	if Wrap(KindInternal, "op", nil) != nil {
		t.Fatal("Wrap(nil) must be nil")
	}
}
