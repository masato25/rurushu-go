package builtin

import (
	"fmt"

	"github.com/arborlogic/rurushu-go/tool"
)

func toolError(title, format string, args ...any) *tool.Result {
	return &tool.Result{Title: title, Output: fmt.Sprintf(format, args...), IsError: true}
}
