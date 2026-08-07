package channelagent

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// fakeDocker 記錄每一次呼叫，並依序回放腳本化的結果。所有 docker 相關測試
// 共用它——測試永遠不得真的執行 docker。
type fakeDocker struct {
	calls [][]string
	envs  [][]string
	out   []string
	errs  []error
	n     int
}

func (f *fakeDocker) Run(_ context.Context, env []string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	f.envs = append(f.envs, env)
	i := f.n
	f.n++
	var o string
	var e error
	if i < len(f.out) {
		o = f.out[i]
	}
	if i < len(f.errs) {
		e = f.errs[i]
	}
	return o, e
}

// exitErr 造出一個「docker 真的跑起來、回報了非零離開碼」的錯誤，附上 stderr。
func exitErr(stderr string) error {
	return &dockerError{Args: []string{"image", "inspect"}, Stderr: stderr, Err: &exec.ExitError{}}
}

func TestDockerSaysAbsentOnlyForRealNegativeAnswers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no such image", exitErr("Error response from daemon: No such image: cc-a2a-sandbox:1"), true},
		{"no such object", exitErr("Error: No such object: cc-a2a-aa-x"), true},
		{"no such container", exitErr("Error response from daemon: No such container: cc-a2a-aa-x"), true},
		{"daemon down", exitErr("Cannot connect to the Docker daemon at unix:///var/run/docker.sock."), false},
		{"permission denied", exitErr("permission denied while trying to connect to the Docker daemon socket"), false},
		{"binary missing", &dockerError{Stderr: "", Err: exec.ErrNotFound}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := dockerSaysAbsent(c.err); got != c.want {
			t.Errorf("%s: dockerSaysAbsent = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSandboxImageAvailableThreeWay(t *testing.T) {
	// 1. 存在
	dr := &fakeDocker{out: []string{"sha256:abc\n"}, errs: []error{nil}}
	ok, err := SandboxImageAvailable(context.Background(), dr, "cc-a2a-sandbox:1")
	if err != nil || !ok {
		t.Fatalf("present image: ok=%v err=%v, want true/nil", ok, err)
	}
	want := []string{"image", "inspect", "--format", "{{.Id}}", "cc-a2a-sandbox:1"}
	if len(dr.calls) != 1 || !equalStrings(dr.calls[0], want) {
		t.Fatalf("argv = %v, want %v", dr.calls, want)
	}

	// 2. 明確不存在 → (false, nil)
	dr = &fakeDocker{errs: []error{exitErr("Error: No such image: cc-a2a-sandbox:1")}}
	ok, err = SandboxImageAvailable(context.Background(), dr, "cc-a2a-sandbox:1")
	if err != nil || ok {
		t.Fatalf("absent image: ok=%v err=%v, want false/nil", ok, err)
	}

	// 3. 問不到答案 → 必須回非 nil error，絕不可以跟「不存在」混在一起
	dr = &fakeDocker{errs: []error{exitErr("Cannot connect to the Docker daemon")}}
	ok, err = SandboxImageAvailable(context.Background(), dr, "cc-a2a-sandbox:1")
	if err == nil {
		t.Fatalf("daemon down must return an error, got ok=%v err=nil", ok)
	}
}

func TestSandboxImageAvailableRefusesEmptyImage(t *testing.T) {
	dr := &fakeDocker{}
	if _, err := SandboxImageAvailable(context.Background(), dr, ""); err == nil {
		t.Fatal("empty image name must be an error, not a docker call")
	}
	if len(dr.calls) != 0 {
		t.Fatalf("empty image name must not shell out, got %v", dr.calls)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
