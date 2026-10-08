package builtin

import (
	"fmt"

	"github.com/masato25/rurushu-go/tool"
)

func toolError(title, format string, args ...any) *tool.Result {
	return &tool.Result{Title: title, Output: fmt.Sprintf(format, args...), IsError: true}
}
