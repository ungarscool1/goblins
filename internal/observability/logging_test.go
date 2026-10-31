package observability

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLoggerRedacts(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLogger(&buf, LogOptions{Level: "debug", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("event",
		"webhook_secret", "hunter2",
		"private_key", "abc",
		"url", "https://user:s3cr3tpass@example.com/x",
		"msg2", "Authorization: Bearer abcdefghijklmnop",
		"err", errors.New("failed with token abcdefghijklmnop"),
		"harmless", "hello",
	)
	out := buf.String()
	for _, leak := range []string{"hunter2", "s3cr3tpass", "abcdefghijklmnop"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("harmless value dropped: %s", out)
	}
}

func TestRedactText(t *testing.T) {
	cases := map[string]string{
		"no secrets here":                         "no secrets here",
		"x eyJhbGciOiJI.eyJzdWIiOiIx.sigsigsig y": "x [REDACTED] y",
		"-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----": "[REDACTED]",
		"sk-ant-oat01-abcdefghijklmnopqrstuvwxyz0123456789":                   "[REDACTED]",
	}
	for in, want := range cases {
		if got := RedactText(in); got != want {
			t.Errorf("RedactText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoggerOptionsAndContext(t *testing.T) {
	if _, err := NewLogger(&bytes.Buffer{}, LogOptions{Level: "loud"}); err == nil {
		t.Error("invalid level accepted")
	}
	if _, err := NewLogger(&bytes.Buffer{}, LogOptions{Format: "xml"}); err == nil {
		t.Error("invalid format accepted")
	}
	var buf bytes.Buffer
	l, _ := NewLogger(&buf, LogOptions{Format: "text"})
	if Logger(WithLogger(context.Background(), l)) != l {
		t.Error("logger not retrieved from context")
	}
}
