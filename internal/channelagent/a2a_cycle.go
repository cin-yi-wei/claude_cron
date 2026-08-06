package channelagent

import (
	"context"
	"fmt"
	"io"
	"time"
)

// RunA2ACycleOnce 執行一次完整的 A2A cycle：collect → sweep → drain →
// ensure sandbox drivers → enqueue terminal callbacks → prune。順序固定——
// collect 先跑才能讓 sweep/drain 這一輪看到剛釋出的容量，callback 排在
// collect/sweep 之後才能撿到這一輪剛進入終止狀態的 row（原本分散在
// cmd/claude-cron/main.go 裡的六行呼叫的順序理由，現在集中寫在這裡）。
//
// 這個函式存在的唯一理由是可測試性：main.go 原本把這六個呼叫直接寫在
// serve 迴圈的 goroutine 裡，一行一行各自呼叫——那組呼叫本身沒有任何測試
// 覆蓋（final review 2026-08-06 抓到的缺口），刪掉其中任何一行（例如
// EnsureSandboxDrivers）不會讓任何測試變紅，跟 DIAGNOSIS.md 記過的「功能
// 沒人呼叫」是同一種結構性缺口，只是這次反過來：呼叫存在，但沒有測試盯著
// 呼叫本身。把六步收進這一個函式、main.go 只呼叫它一次，TestA2ACycleRunsAllStages
// 才有唯一一個地方可以釘住「六步都真的發生了」——main.go 之後刪掉對這個函式
// 的呼叫（整個 A2A cycle 消失）依然不會被這個測試抓到，但那已經是「整個
// 功能不見了」這種一眼可見的差異，不是「六步裡少一步」這種容易漏看的差異。
//
// 每個階段各自的正確性已經有專屬單元測試（CollectResults/SweepTimeouts/
// DrainQueue/EnsureSandboxDrivers/EnqueueTerminalCallbacks/PruneTasks 各自
// 的 _test.go）；這裡不重複驗證那些，只驗證「這六個都被這個函式呼叫」。
//
// 非致命的階段錯誤寫進 stderr（跟 main.go 原本 fmt.Fprintf 的語意一致），
// 不中止本輪剩下的階段，也不影響 cc- binding 自己的處理。
func RunA2ACycleOnce(ctx context.Context, root string, now time.Time, sm SessionManager, ex TaskExecutor, driver *SandboxDriver, cb *CallbackDispatcher, stderr io.Writer) {
	if _, err := CollectResults(root, now); err != nil {
		fmt.Fprintf(stderr, "a2a collect: %v\n", err)
	}
	if _, _, err := SweepTimeouts(ctx, root, sm, now, driver); err != nil {
		fmt.Fprintf(stderr, "a2a sweep: %v\n", err)
	}
	if _, err := DrainQueue(ctx, root, ex); err != nil {
		fmt.Fprintf(stderr, "a2a drain: %v\n", err)
	}
	EnsureSandboxDrivers(ctx, root, driver)
	// callback 的唯一觸發點。collect / sweep 之後才掃，於是這一輪剛進入終
	// 止狀態的 row 也會被撿到。
	EnqueueTerminalCallbacks(root, cb)
	if _, err := PruneTasks(root, now); err != nil {
		fmt.Fprintf(stderr, "a2a prune: %v\n", err)
	}
}
