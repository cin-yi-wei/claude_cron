package channelagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DefaultSandboxImage 是沙盒映像的預設 tag。tag 是「映像自己的版本號」
// （作業系統 + 工具集），不是 claude 的版本號 —— claude 與 claude-cron
// 都不烘進映像，由 bind mount 從 host 帶進去，版本永遠跟 host 一致。
const DefaultSandboxImage = "cc-a2a-sandbox:1"

// dockerRunner 是唯一執行 docker CLI 的地方，抽成介面只有一個理由：測試
// 永遠不得真的起容器。env 為 nil 時沿用 runExternalCommand 的環境處理
// （去掉 TMUX/TMUX_PANE）；非 nil 時整份取代 —— 這是把 OAuth token 放進
// 子行程「環境」而不是「argv」的機制（見 ContainerSessionManager.Start），
// argv 會出現在同一台機器上任何人的 ps 輸出裡，環境不會。
type dockerRunner interface {
	Run(ctx context.Context, env []string, args ...string) (string, error)
}

// dockerError 保留 docker 的 stderr。分類「明確不存在」與「問不到答案」
// 需要看 stderr，光看離開碼不夠：docker 對兩者都是非零離開。
type dockerError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *dockerError) Error() string {
	return fmt.Sprintf("docker %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

func (e *dockerError) Unwrap() error { return e.Err }

type execDockerRunner struct{}

func (execDockerRunner) Run(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if env == nil {
		cmd.Env = envWithout(os.Environ(), "TMUX", "TMUX_PANE")
	} else {
		cmd.Env = env
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), &dockerError{Args: args, Stderr: stderr.String(), Err: err}
	}
	return string(out), nil
}

// dockerAbsentMarkers 是 docker 用來表達「我查過了，沒有這個東西」的字串。
// 刻意只列這三條、且要求同時滿足「docker 真的執行起來並回報離開碼」：任何
// 其他失敗（daemon 沒起來、權限不足、執行檔找不到、ctx 取消）都是「問不到
// 答案」，必須讓呼叫方看到 error 而不是一個看起來很確定的 false。
var dockerAbsentMarkers = []string{"no such image", "no such object", "no such container"}

func dockerSaysAbsent(err error) bool {
	var de *dockerError
	if !errors.As(err, &de) {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(de.Err, &exitErr) {
		return false // docker 沒有真的跑起來並回報離開碼
	}
	low := strings.ToLower(de.Stderr)
	for _, m := range dockerAbsentMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// SandboxImageAvailable 回報映像是否存在。三分法與 TmuxSessionAlive 完全
// 相同：(true, nil) 存在；(false, nil) 明確不存在；(_, err) 問不到答案。
func SandboxImageAvailable(ctx context.Context, dr dockerRunner, image string) (bool, error) {
	if strings.TrimSpace(image) == "" {
		return false, errors.New("sandbox image name is empty")
	}
	if _, err := dr.Run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if dockerSaysAbsent(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
