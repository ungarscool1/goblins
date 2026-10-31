//go:build docker

// Tests against a real Docker engine. They need no credentials and consume no
// tokens. Run with `make docker-test`; they skip if no engine is reachable.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	rtpkg "github.com/ungarscool1/goblins/internal/runtime"
	"github.com/ungarscool1/goblins/internal/runtime/docker"
	"github.com/ungarscool1/goblins/internal/runtime/workspace"
)

const testImage = "alpine:3.22"

func engine(t *testing.T) *docker.Runtime {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		if home, _ := os.UserHomeDir(); home != "" {
			if _, err := os.Stat(home + "/.docker/run/docker.sock"); err == nil {
				host = "unix://" + home + "/.docker/run/docker.sock"
			}
		}
	}
	r, err := docker.New("docker", docker.Options{Host: host, PullMissing: true, Network: "goblins-test-workers"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.List(ctx, map[string]string{"goblins.none": "x"}); err != nil {
		t.Skipf("no Docker engine: %v", err)
	}
	return r
}

func translate(cmd ...string) []string {
	switch cmd[0] {
	case "ok":
		return []string{"sh", "-c", "echo " + strings.Join(cmd[1:], " ")}
	case "hang":
		return []string{"sleep", "300"}
	}
	return []string{"sh", "-c", "exit 3"}
}

func TestDockerContract(t *testing.T) {
	contract.ExecutionRuntime(t, func(t *testing.T) contract.RuntimeHarness {
		return contract.RuntimeHarness{Runtime: engine(t), Image: testImage, Translate: translate}
	})
}

func run(t *testing.T, r *docker.Runtime, spec core.WorkloadSpec) (core.WorkloadStatus, string, string) {
	t.Helper()
	ctx := context.Background()
	spec.Name = "goblins-itest-" + spec.Name + "-" + time.Now().Format("150405.000")
	spec.Name = strings.ReplaceAll(spec.Name, ".", "-")
	spec.Image = testImage
	t.Cleanup(func() { _ = r.Cleanup(ctx, spec.Name) })
	if _, err := r.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if err := r.Logs(ctx, spec.Name, true, &out, &errb); err != nil {
		t.Fatal(err)
	}
	var st core.WorkloadStatus
	for range 200 {
		var err error
		st, err = r.Status(ctx, spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		if st.Phase.Terminal() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return st, out.String(), errb.String()
}

func TestWorkerConfinement(t *testing.T) {
	r := engine(t)
	script := `
id -u
test -e /var/run/docker.sock && echo SOCKET-PRESENT
touch /should-fail 2>/dev/null && echo ROOT-WRITABLE
touch /tmp/ok && echo TMP-OK
printf '#!/bin/sh\necho TMP-EXEC\n' > /tmp/run && chmod +x /tmp/run && /tmp/run
touch /workspace/ok && echo WS-WRITABLE
grep CapEff /proc/self/status
grep NoNewPrivs /proc/self/status
wget -q -T 2 -O- http://1.1.1.1 >/dev/null 2>&1 && echo NETWORK-OPEN
echo done
`
	ctx := context.Background()
	vol := "goblins-itest-vol-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "-")
	if err := r.EnsureVolume(ctx, vol, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.RemoveVolume(ctx, vol) })
	st, out, errb := run(t, r, core.WorkloadSpec{
		Name: "confine", Command: []string{"sh", "-c", script}, Network: core.NetworkNone,
		Mounts:   []core.Mount{{Volume: vol, Target: "/workspace", ReadOnly: true}},
		Security: core.SecurityProfile{ReadOnlyRoot: true},
	})
	if st.Phase != core.WorkloadSucceeded {
		t.Fatalf("status = %+v, stderr: %s", st, errb)
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "10001" {
		t.Errorf("uid = %s", lines[0])
	}
	for _, bad := range []string{"SOCKET-PRESENT", "ROOT-WRITABLE", "WS-WRITABLE", "NETWORK-OPEN"} {
		if strings.Contains(out, bad) {
			t.Errorf("confinement broken, %s:\n%s", bad, out)
		}
	}
	for _, want := range []string{"TMP-OK", "TMP-EXEC", "CapEff:\t0000000000000000", "NoNewPrivs:\t1", "done"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestWorkloadLimitAndTimeout(t *testing.T) {
	r := engine(t)
	start := time.Now()
	st, _, _ := run(t, r, core.WorkloadSpec{Name: "timeout", Command: []string{"sleep", "300"}, Network: core.NetworkNone, Timeout: 2 * time.Second})
	if st.Phase != core.WorkloadTimedOut {
		t.Fatalf("status = %+v", st)
	}
	if time.Since(start) > 60*time.Second {
		t.Fatal("timeout took too long")
	}
	st, out, _ := run(t, r, core.WorkloadSpec{Name: "pids", Command: []string{"sh", "-c", "cat /sys/fs/cgroup/pids.max /sys/fs/cgroup/memory.max"}, Network: core.NetworkNone,
		Resources: core.Resources{PIDs: 64, MemoryBytes: 64 << 20}})
	if st.Phase != core.WorkloadSucceeded || !strings.Contains(out, "64\n") || !strings.Contains(out, "67108864") {
		t.Fatalf("limits not applied: %+v %q", st, out)
	}
}

func TestVolumeOutlivesContainer(t *testing.T) {
	r := engine(t)
	ctx := context.Background()
	vol := "goblins-itest-keep-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "-")
	if err := r.EnsureVolume(ctx, vol, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.RemoveVolume(ctx, vol) })
	// Worker images pre-create their mount points so volumes get usable ownership;
	// TestWorkspaceCycleInContainers covers that. Here the mount is read back.
	st, out, _ := run(t, r, core.WorkloadSpec{Name: "vol", Command: []string{"sh", "-c", "ls -ld /workspace"}, Network: core.NetworkNone, Mounts: []core.Mount{{Volume: vol, Target: "/workspace"}}})
	if st.Phase != core.WorkloadSucceeded || !strings.Contains(out, "/workspace") {
		t.Fatalf("status = %+v, %q", st, out)
	}
}

func TestOrphanListingAcrossRestart(t *testing.T) {
	first := engine(t)
	ctx := context.Background()
	name := "goblins-itest-orphan-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "-")
	label := map[string]string{"goblins.itest": name}
	if _, err := first.Create(ctx, core.WorkloadSpec{Name: name, Image: testImage, Command: []string{"sleep", "300"}, Network: core.NetworkNone, Labels: label}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Cleanup(ctx, name) })

	// A new process has no memory of it, yet can find, stop and remove it.
	second := engine(t)
	list, err := second.List(ctx, label)
	if err != nil || len(list) != 1 || list[0].Phase != core.WorkloadRunning {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := second.Cancel(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := second.Cleanup(ctx, name); err != nil {
		t.Fatal(err)
	}
	list, _ = second.List(ctx, label)
	if len(list) != 0 {
		t.Fatalf("after cleanup: %+v", list)
	}
}

// TestWorkspaceCycleInContainers runs the real scripts in the real helper image,
// confined the way production workloads are: non-root, read-only root, no capabilities.
func TestWorkspaceCycleInContainers(t *testing.T) {
	r := engine(t)
	ctx := context.Background()
	stamp := strings.ReplaceAll(time.Now().Format("150405.000"), ".", "-")
	remote, ws, scratch := "goblins-itest-remote-"+stamp, "goblins-itest-ws-"+stamp, "goblins-itest-scratch-"+stamp
	for _, v := range []string{remote, ws, scratch} {
		if err := r.EnsureVolume(ctx, v, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.RemoveVolume(ctx, v) })
	}
	step := 0
	exec := func(spec core.WorkloadSpec) string {
		t.Helper()
		step++
		spec.Name = fmt.Sprintf("goblins-itest-%s-%d", stamp, step)
		res, err := rtpkg.RunToCompletion(ctx, r, spec)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status.Phase != core.WorkloadSucceeded {
			t.Fatalf("step %d failed: %+v\nstdout: %s\nstderr: %s", step, res.Status, res.Stdout, res.Stderr)
		}
		return res.Stdout
	}
	const image = "goblins-git:dev"
	// A bare remote on a volume, mounted into each networked step.
	seed := "set -eu; export HOME=/tmp GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@e GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@e; cd /workspace; git init -q --bare -b main r.git; d=$(mktemp -d); cd $d; git init -q -b main; echo hi > a; git add a; git commit -qm init; git push -q /workspace/r.git main; git rev-parse HEAD"
	out := exec(core.WorkloadSpec{Image: image, Command: []string{"sh", "-c", seed}, Network: core.NetworkNone, Mounts: []core.Mount{{Volume: remote, Target: "/workspace"}}})
	base := strings.TrimSpace(out)
	access := core.GitAccess{RemoteURL: "file:///remote/r.git", Username: "x", Password: core.NewSecret("tok")}
	withRemote := func(s core.WorkloadSpec, err error) core.WorkloadSpec {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		s.Mounts = append(s.Mounts, core.Mount{Volume: remote, Target: "/remote"})
		return s
	}

	const branch = "goblins/t1"
	exec(withRemote(workspace.PrepareSpec("p", image, ws, nil, access, "main", base, branch)))
	// The agent's edit, as the agent user, on the same volume.
	exec(core.WorkloadSpec{Image: image, Command: []string{"sh", "-c", "echo change > /workspace/repo/new.txt"}, Network: core.NetworkNone, Mounts: []core.Mount{{Volume: ws, Target: "/workspace"}}})
	spec, err := workspace.CommitSpec("c", image, ws, nil, workspace.Identity{Name: "G", Email: "g@e"}, branch, "Add file")
	if err != nil {
		t.Fatal(err)
	}
	got, err := workspace.ParseResult(exec(spec))
	if err != nil || !got.Changed || got.HeadSHA == base {
		t.Fatalf("commit = %+v, %v", got, err)
	}
	cp, err := workspace.CopySpec("cp", image, ws, scratch, nil, got.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	exec(cp)
	check := exec(core.WorkloadSpec{Image: image, Command: []string{"sh", "-c", "cat /scratch/repo/new.txt; touch /scratch/repo/test-output"}, Network: core.NetworkNone, Mounts: []core.Mount{{Volume: scratch, Target: "/scratch"}}})
	if !strings.Contains(check, "change") {
		t.Fatalf("scratch copy = %q", check)
	}
	exec(withRemote(workspace.PublishSpec("pub", image, ws, nil, access, "main", branch, got.HeadSHA)))
	pushed := exec(core.WorkloadSpec{Image: image, Command: []string{"sh", "-c", "git --git-dir=/remote/r.git rev-parse refs/heads/goblins/t1"}, Env: map[string]string{"HOME": "/tmp"}, Network: core.NetworkNone, Mounts: []core.Mount{{Volume: remote, Target: "/remote", ReadOnly: true}}})
	if strings.TrimSpace(pushed) != got.HeadSHA {
		t.Fatalf("pushed = %q, want %s", pushed, got.HeadSHA)
	}
}
