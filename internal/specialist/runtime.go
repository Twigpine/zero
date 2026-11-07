package specialist

import (
	"errors"
	"sync"

	"github.com/Twigpine/zero/internal/background"
)

type RuntimeOptions struct {
	Manager     *background.Manager
	ManagerFunc BackgroundManagerFunc
}

type Runtime struct {
	mu          sync.Mutex
	manager     *background.Manager
	managerFunc BackgroundManagerFunc
	promptFiles map[string]string
}

func NewRuntime(options RuntimeOptions) *Runtime {
	return &Runtime{
		manager:     options.Manager,
		managerFunc: options.ManagerFunc,
		promptFiles: map[string]string{},
	}
}

func (runtime *Runtime) Manager() (*background.Manager, error) {
	if runtime == nil {
		return background.NewManager("")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.manager != nil {
		return runtime.manager, nil
	}
	managerFunc := runtime.managerFunc
	if managerFunc == nil {
		managerFunc = func() (*background.Manager, error) {
			return background.NewManager("")
		}
	}
	manager, err := managerFunc()
	if err != nil {
		return nil, err
	}
	runtime.manager = manager
	return manager, nil
}

func (runtime *Runtime) TrackPromptFile(taskID string, promptFile string) {
	if runtime == nil || taskID == "" || promptFile == "" {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.promptFiles[taskID] = promptFile
}

func (runtime *Runtime) UntrackPromptFile(taskID string) {
	if runtime == nil || taskID == "" {
		return
	}
	promptFile := ""
	runtime.mu.Lock()
	if runtime.promptFiles != nil {
		promptFile = runtime.promptFiles[taskID]
		delete(runtime.promptFiles, taskID)
	}
	runtime.mu.Unlock()
	cleanupPromptFile(promptFile)
}

func (runtime *Runtime) Close() error {
	if runtime == nil {
		return nil
	}
	var manager *background.Manager
	promptFiles := []string{}
	runtime.mu.Lock()
	manager = runtime.manager
	for taskID, promptFile := range runtime.promptFiles {
		promptFiles = append(promptFiles, promptFile)
		delete(runtime.promptFiles, taskID)
	}
	runtime.mu.Unlock()

	var errs []error
	if manager != nil {
		if err := manager.KillRunning(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, promptFile := range promptFiles {
		cleanupPromptFile(promptFile)
	}
	return errors.Join(errs...)
}
