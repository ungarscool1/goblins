package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

func TestCreateRequestIsConfined(t *testing.T) {
	r, err := New("d", Options{Defaults: core.Resources{MemoryBytes: 1 << 30, CPUs: 2, PIDs: 100}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := r.buildCreate(core.WorkloadSpec{
		Name: "w", Image: "img", Command: []string{"run"}, Network: core.NetworkNone,
		Mounts:   []core.Mount{{Volume: "ws", Target: "/workspace", ReadOnly: true}},
		Env:      map[string]string{"A": "1"},
		Secrets:  map[string]core.Secret{"S": core.NewSecret("v")},
		Security: core.SecurityProfile{ReadOnlyRoot: true},
	}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	hc := body.HostConfig
	if body.User != defaultUser || hc.Privileged || !hc.ReadonlyRootfs || !hc.Init {
		t.Fatalf("create = %+v", body)
	}
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" || hc.SecurityOpt[0] != "no-new-privileges:true" {
		t.Fatalf("hardening = %+v", hc)
	}
	if hc.NetworkMode != "none" || hc.Memory != 1<<30 || hc.MemorySwap != 1<<30 || hc.PidsLimit != 100 || hc.NanoCpus != 2e9 {
		t.Fatalf("limits = %+v", hc)
	}
	if len(hc.Mounts) != 1 || hc.Mounts[0].Type != "volume" || !hc.Mounts[0].ReadOnly {
		t.Fatalf("mounts = %+v", hc.Mounts)
	}
	if tmp := strings.Split(hc.Tmpfs["/tmp"], ","); !slices.Contains(tmp, "exec") || !slices.Contains(tmp, "nosuid") || !slices.Contains(tmp, "nodev") {
		t.Fatalf("tmpfs = %q", hc.Tmpfs["/tmp"])
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "Binds") || strings.Contains(string(raw), "docker.sock") {
		t.Fatalf("host path present: %s", raw)
	}
}

func TestHostPathsCannotBeMounted(t *testing.T) {
	r, _ := New("d", Options{})
	for _, v := range []string{"/var/run/docker.sock", "/", "../etc", "a/b", ""} {
		_, err := r.buildCreate(core.WorkloadSpec{Image: "i", Command: []string{"c"}, Network: core.NetworkNone, Mounts: []core.Mount{{Volume: v, Target: "/x"}}}, time.Time{})
		if !core.IsKind(err, core.KindInvalid) {
			t.Errorf("%q accepted: %v", v, err)
		}
		if v != "" {
			if err := r.EnsureVolume(context.Background(), v, nil); !core.IsKind(err, core.KindInvalid) {
				t.Errorf("volume %q accepted: %v", v, err)
			}
		}
	}
}

func TestRootUserIsRefused(t *testing.T) {
	for _, u := range []string{"0", "root", "0:0"} {
		if _, err := New("d", Options{User: u}); !core.IsKind(err, core.KindInvalid) {
			t.Errorf("user %q accepted: %v", u, err)
		}
	}
}

func TestUnsupportedFeaturesAreRefusedBeforeCallingTheEngine(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer srv.Close()
	r, err := New("d", Options{Host: strings.Replace(srv.URL, "http://", "tcp://", 1)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []core.WorkloadSpec{
		{Security: core.SecurityProfile{Privileged: true}, Network: core.NetworkNone},
		{Security: core.SecurityProfile{GPU: true}, Network: core.NetworkNone},
		{Security: core.SecurityProfile{KVM: true}, Network: core.NetworkNone},
		{Resources: core.Resources{DiskBytes: 1}, Network: core.NetworkNone},
	} {
		if _, err := r.Create(context.Background(), s); !core.IsKind(err, core.KindUnsupported) {
			t.Errorf("%+v: %v", s, err)
		}
	}
	if _, err := r.Create(context.Background(), core.WorkloadSpec{Image: "i", Command: []string{"c"}}); !core.IsKind(err, core.KindInvalid) {
		t.Errorf("no network policy: %v", err)
	}
	if calls != 0 {
		t.Fatalf("engine was called %d times", calls)
	}
}

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, "out1\n"))
	in.Write(frame(2, "err1\n"))
	in.Write(frame(1, "out2\n"))
	var out, errb bytes.Buffer
	if err := demux(&in, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if out.String() != "out1\nout2\n" || errb.String() != "err1\n" {
		t.Fatalf("out=%q err=%q", out.String(), errb.String())
	}
}

// engineStub scripts just enough of the API to exercise paths a real engine makes
// hard to reach on demand.
func engineStub(t *testing.T, h http.HandlerFunc) *Runtime {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	r, err := New("d", Options{Host: strings.Replace(srv.URL, "http://", "tcp://", 1), PullMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreatePullsMissingImageOnce(t *testing.T) {
	var creates, pulls int
	r := engineStub(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/containers/create"):
			creates++
			if creates == 1 {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `{"message":"No such image"}`)
			}
		case req.URL.Path == "/images/create":
			pulls++
			if req.URL.Query().Get("fromImage") != "registry:5000/img" || req.URL.Query().Get("tag") != "v1" {
				t.Errorf("pull query = %v", req.URL.Query())
			}
			_, _ = io.WriteString(w, `{"status":"Downloading"}`+"\n")
		case strings.HasSuffix(req.URL.Path, "/start"):
			w.WriteHeader(204)
		case strings.HasSuffix(req.URL.Path, "/json"):
			_, _ = io.WriteString(w, `{"State":{"Status":"running"},"Config":{"Labels":{}}}`)
		}
	})
	st, err := r.Create(context.Background(), core.WorkloadSpec{Name: "w", Image: "registry:5000/img:v1", Command: []string{"c"}, Network: core.NetworkNone})
	if err != nil || st.Phase != core.WorkloadRunning || pulls != 1 || creates != 2 {
		t.Fatalf("st=%+v err=%v pulls=%d creates=%d", st, err, pulls, creates)
	}
}

func TestRetriedCreateNeverRestartsAFinishedContainer(t *testing.T) {
	cases := []struct {
		state      string
		exit       int
		want       core.WorkloadPhase
		wantStarts int
	}{
		{"exited", 0, core.WorkloadSucceeded, 0},
		{"exited", 3, core.WorkloadFailed, 0},
		{"running", 0, core.WorkloadRunning, 0},
		{"created", 0, core.WorkloadPending, 1}, // created, but the first call died before starting it
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s-%d", tc.state, tc.exit), func(t *testing.T) {
			starts := 0
			r := engineStub(t, func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasPrefix(req.URL.Path, "/containers/create"):
					w.WriteHeader(409)
					_, _ = io.WriteString(w, `{"message":"name in use"}`)
				case strings.HasSuffix(req.URL.Path, "/start"):
					starts++
					w.WriteHeader(204)
				case strings.HasSuffix(req.URL.Path, "/json"):
					_, _ = fmt.Fprintf(w, `{"State":{"Status":%q,"ExitCode":%d},"Config":{"Labels":{}}}`, tc.state, tc.exit)
				}
			})
			st, err := r.Create(context.Background(), core.WorkloadSpec{Name: "w", Image: "i", Command: []string{"c"}, Network: core.NetworkNone})
			if err != nil || st.Phase != tc.want || starts != tc.wantStarts {
				t.Fatalf("st=%+v err=%v starts=%d", st, err, starts)
			}
		})
	}
}

func TestPullQueryForImageReferences(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct{ image, name, tag string }{
		{"img", "img", "latest"},
		{"img:v1", "img", "v1"},
		{"registry:5000/img", "registry:5000/img", "latest"},
		{"registry:5000/img:v1", "registry:5000/img", "v1"},
		{"registry/img@" + digest, "registry/img", digest},
		{"registry:5000/img@" + digest, "registry:5000/img", digest},
		{"registry:5000/img:v1@" + digest, "registry:5000/img", digest},
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			pulled := false
			creates := 0
			r := engineStub(t, func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasPrefix(req.URL.Path, "/containers/create"):
					creates++
					if creates == 1 {
						w.WriteHeader(404)
						_, _ = io.WriteString(w, `{"message":"No such image"}`)
					}
				case req.URL.Path == "/images/create":
					pulled = true
					if q := req.URL.Query(); q.Get("fromImage") != tc.name || q.Get("tag") != tc.tag {
						t.Errorf("pull query = %v", q)
					}
				case strings.HasSuffix(req.URL.Path, "/json"):
					_, _ = io.WriteString(w, `{"State":{"Status":"running"},"Config":{"Labels":{}}}`)
				default:
					w.WriteHeader(204)
				}
			})
			if _, err := r.Create(context.Background(), core.WorkloadSpec{Name: "w", Image: tc.image, Command: []string{"c"}, Network: core.NetworkNone}); err != nil || !pulled {
				t.Fatalf("err=%v pulled=%v", err, pulled)
			}
		})
	}
}

func TestPullFailureInBandIsReported(t *testing.T) {
	r := engineStub(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/containers/create"):
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"message":"No such image"}`)
		case req.URL.Path == "/images/create":
			_, _ = io.WriteString(w, `{"error":"pull access denied"}`+"\n")
		}
	})
	_, err := r.Create(context.Background(), core.WorkloadSpec{Name: "w", Image: "private/img", Command: []string{"c"}, Network: core.NetworkNone})
	if !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusMapping(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	label := func(tm time.Time) string {
		b, _ := json.Marshal(map[string]string{labelDeadline: itoa(tm.Unix())})
		return string(b)
	}
	stopped := false
	cases := []struct {
		name, body string
		want       core.WorkloadPhase
	}{
		{"created", `{"State":{"Status":"created"},"Config":{"Labels":{}}}`, core.WorkloadPending},
		{"running", `{"State":{"Status":"running"},"Config":{"Labels":{}}}`, core.WorkloadRunning},
		{"ok", `{"State":{"Status":"exited","ExitCode":0},"Config":{"Labels":{}}}`, core.WorkloadSucceeded},
		{"failed", `{"State":{"Status":"exited","ExitCode":2},"Config":{"Labels":{}}}`, core.WorkloadFailed},
		{"timed out", `{"State":{"Status":"exited","ExitCode":137,"FinishedAt":"` + time.Now().Format(time.RFC3339) + `"},"Config":{"Labels":` + label(past) + `}}`, core.WorkloadTimedOut},
		{"overdue is stopped", `{"State":{"Status":"running"},"Config":{"Labels":` + label(past) + `}}`, core.WorkloadRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := engineStub(t, func(w http.ResponseWriter, req *http.Request) {
				if strings.HasSuffix(req.URL.Path, "/stop") {
					stopped = true
					w.WriteHeader(204)
					return
				}
				_, _ = io.WriteString(w, tc.body)
			})
			st, err := r.Status(context.Background(), "w")
			if err != nil || st.Phase != tc.want {
				t.Fatalf("status = %+v, %v", st, err)
			}
		})
	}
	if !stopped {
		t.Fatal("a workload past its deadline was not stopped")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestErrorKinds(t *testing.T) {
	for status, kind := range map[int]core.ErrorKind{404: core.KindNotFound, 409: core.KindConflict, 400: core.KindInvalid, 403: core.KindForbidden, 500: core.KindTransient} {
		r := engineStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		if _, err := r.Status(context.Background(), "w"); !core.IsKind(err, kind) {
			t.Errorf("%d: %v", status, err)
		}
	}
}

func TestFactory(t *testing.T) {
	reg := providers.NewRegistry()
	Register(reg)
	rt, err := providers.As[core.ExecutionRuntime](reg, providers.KindRuntime, "docker", "d", providers.Deps{}, providers.Options{"host": "tcp://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Info().Has(core.CapRuntimePrivileged) || rt.Info().Has(core.CapRuntimeGPU) || rt.Info().Has(core.CapRuntimeKVM) {
		t.Fatal("privileged features offered")
	}
	if _, err := providers.As[core.ExecutionRuntime](reg, providers.KindRuntime, "docker", "d", providers.Deps{}, providers.Options{"user": "root"}); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("root: %v", err)
	}
	if _, err := New("d", Options{Host: "ftp://x"}); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("scheme: %v", err)
	}
}
