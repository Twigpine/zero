package specialist

import "github.com/Twigpine/zero/internal/tools"

func RegisterTools(registry *tools.Registry, executor Executor) (*Runtime, error) {
	runtime := executor.BackgroundRuntime
	if runtime == nil {
		runtime = NewRuntime(RuntimeOptions{
			Manager:     executor.BackgroundManager,
			ManagerFunc: executor.BackgroundManagerFunc,
		})
	}
	toolExecutor := executor
	toolExecutor.BackgroundRuntime = runtime
	toolExecutor.BackgroundManagerFunc = runtime.Manager
	registry.Register(NewTaskTool(toolExecutor))
	registry.Register(newOutputToolWithManagerFunc(runtime.Manager, executor.SessionStore))
	registry.Register(newStopToolWithManagerFunc(runtime.Manager))
	registry.Register(NewGenerateTool(NewStorage(executor.Paths)))
	return runtime, nil
}
