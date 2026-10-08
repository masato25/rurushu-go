package builtin

import (
	"github.com/arborlogic/rurushu-go/jobs"
	"github.com/arborlogic/rurushu-go/tool"
)

// RegisterReadOnly installs the built-in repository inspection tools.
func RegisterReadOnly(registry *tool.Registry) {
	if registry == nil {
		return
	}
	registry.Register(&ReadTool{})
	registry.Register(&GlobTool{})
	registry.Register(&GrepTool{})
}

// RegisterExecution installs shell execution and managed-job tools.
func RegisterExecution(registry *tool.Registry, manager *jobs.Manager) {
	if registry == nil || manager == nil {
		return
	}
	registry.Register(&BashTool{Jobs: manager})
	registry.Register(&JobListTool{Jobs: manager})
	registry.Register(&JobOutputTool{Jobs: manager})
	registry.Register(&JobStopTool{Jobs: manager})
}
