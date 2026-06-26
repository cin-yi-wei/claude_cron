package channelagent

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsTurnTimeout(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{context.DeadlineExceeded, true},
		{fmt.Errorf("waitOutput: %w", context.DeadlineExceeded), true},
		{errors.New("context deadline exceeded"), true},
		{errors.New("acquire lock /x/claude.lock: held by live pid 974084"), false},
		{errors.New("inject: prompt still in input box after attempt 3"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isTurnTimeout(c.err); got != c.want {
			t.Errorf("isTurnTimeout(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
