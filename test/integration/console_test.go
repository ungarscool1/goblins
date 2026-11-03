//go:build integration

package integration

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

const consoleToken = "integration-console-token-0123456789abcdef"

type browser struct {
	t      *testing.T
	base   string
	cookie string
}

func (b *browser) do(method, path string, form url.Values) (int, string, http.Header) {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, b.base+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if b.cookie != "" {
		req.Header.Set("Cookie", b.cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if c.Name == "goblins_console" {
			b.cookie = c.Name + "=" + c.Value
		}
	}
	return resp.StatusCode, string(data), resp.Header
}

func (b *browser) page(path string) string {
	b.t.Helper()
	code, body, _ := b.do(http.MethodGet, path, nil)
	if code != http.StatusOK {
		b.t.Fatalf("%s: %d", path, code)
	}
	return body
}

func field(t *testing.T, page, name string) string {
	t.Helper()
	m := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no %s field on the page:\n%s", name, page)
	}
	return m[1]
}

// A fake task is operated entirely from the console, and its conversation is still there
// after a restart. Only the ticket itself comes from outside, as it would from a tracker.
func TestMockTaskOperatedFromTheConsole(t *testing.T) {
	consoleAddr := freeAddr(t)
	s := &service{t: t, bin: buildBinary(t), addr: freeAddr(t), data: t.TempDir(),
		extra: []string{"GOBLINS_CONSOLE_LISTEN=" + consoleAddr,
			"GOBLINS_CONSOLE_TOKEN=" + consoleToken}}
	s.start()
	s.post("/mock/demo/issues", `{"title":"Try the console","body":"Do the thing"}`)
	s.eventually(func() bool { return strings.Contains(s.status(), "design-proposed") })

	b := &browser{t: t, base: "http://" + consoleAddr}
	if code, _, h := b.do(http.MethodGet, "/", nil); code != http.StatusSeeOther ||
		h.Get("Location") != "/login" {
		t.Fatalf("the console let someone in without a login: %d", code)
	}
	b.do(http.MethodPost, "/login", url.Values{"token": {consoleToken}})
	index := b.page("/")
	link := regexp.MustCompile(`href="(/tasks/[^"]+)"`).FindStringSubmatch(index)
	if link == nil || !strings.Contains(index, "design-proposed") {
		t.Fatalf("the queue does not offer the task:\n%s", index)
	}
	task := b.page(link[1])
	if !strings.Contains(task, "Approve revision 1") {
		t.Fatalf("no approval offered:\n%s", task)
	}
	code, _, _ := b.do(http.MethodPost, link[1]+"/actions", url.Values{
		"csrf": {field(t, task, "csrf")}, "action": {"approve"},
		"revision": {field(t, task, "revision")}, "hash": {field(t, task, "hash")}})
	if code != http.StatusSeeOther {
		t.Fatalf("approve: %d", code)
	}
	s.eventually(func() bool { return strings.Contains(s.status(), "completed") })

	s.stop()
	s.start()
	defer s.stop()
	b.cookie = ""
	if code, _, _ := b.do(http.MethodGet, link[1], nil); code != http.StatusSeeOther {
		t.Fatalf("a session outlived the restart: %d", code)
	}
	b.do(http.MethodPost, "/login", url.Values{"token": {consoleToken}})
	task = b.page(link[1])
	for _, want := range []string{"Proposed design, revision 1", "/approve-design 1 ",
		"operator (console)", "approved by operator", "completed"} {
		if !strings.Contains(task, want) {
			t.Errorf("%q is missing after a restart", want)
		}
	}
	if strings.Contains(task, `value="approve"`) {
		t.Error("a finished task still offers actions")
	}
}
