package channelagent

import (
	"fmt"
	"io"
	"net/http"
)

func checkHTTPResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
}
