package builtin

import "github.com/arborlogic/rurushu-go/tool"

// RegisterReadOnly installs the built-in repository inspection tools.
func RegisterReadOnly(registry *tool.Registry) {
	if registry == nil {
		return
	}
	registry.Register(&ReadTool{})
	registry.Register(&GlobTool{})
	registry.Register(&GrepTool{})
}
