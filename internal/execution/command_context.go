package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sync"
)

// RunCommand runs a context-bound command in a retained process tree and
// prevents inherited output handles from blocking Wait indefinitely.
func RunCommand(ctx context.Context, command *exec.Cmd) (err error) {
	if command == nil {
		return errors.New("execution: nil command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	drains := observeOutputDrains(command)
	tree, err := prepareCommandTree(command)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, tree.close()) }()

	command.WaitDelay = processWaitDelay
	// Adapters may return exec.Command, which rejects a non-nil Cancel.
	// The watcher below also owns cancellation for commands without that hook.
	if command.Cancel != nil {
		command.Cancel = tree.cancel
	}
	if err := command.Start(); err != nil {
		_ = tree.attach(nil)
		return err
	}
	if err := tree.attach(command.Process); err != nil {
		killErr := command.Process.Kill()
		waitErr := command.Wait()
		return errors.Join(fmt.Errorf("execution: attach process tree: %w", err), killErr, waitErr)
	}
	waitComplete := make(chan struct{})
	type cancellation struct {
		err      error
		canceled bool
	}
	cancelResult := make(chan cancellation, 1)
	go func() {
		select {
		case <-ctx.Done():
			cancelResult <- cancellation{err: tree.cancel(), canceled: true}
		case <-waitComplete:
			cancelResult <- cancellation{}
		}
	}()
	waitErr := command.Wait()
	close(waitComplete)
	canceled := <-cancelResult
	if canceled.canceled {
		return errors.Join(waitErr, ctx.Err(), canceled.err)
	}
	if waitErr != nil && drains.err() != nil {
		// Cmd.Wait intentionally prefers an ExitError over a copying error. When
		// WaitDelay forcibly closes an inherited output pipe after a nonzero root
		// exit, retain that cleanup failure so result consumers cannot reconcile it
		// as an ordinary command exit.
		return errors.Join(waitErr, exec.ErrWaitDelay, tree.cancel())
	}
	if waitErr != nil {
		return errors.Join(waitErr, tree.cancel())
	}
	return waitErr
}

type outputDrains struct {
	stdout *drainObserver
	stderr *drainObserver
}

func observeOutputDrains(command *exec.Cmd) outputDrains {
	drains := outputDrains{}
	sharedOutput := sameWriter(command.Stderr, command.Stdout)
	if !isFile(command.Stdout) && command.Stdout != nil {
		drains.stdout = &drainObserver{writer: command.Stdout}
		command.Stdout = drains.stdout
	}
	if sharedOutput && drains.stdout != nil {
		command.Stderr = drains.stdout
		drains.stderr = drains.stdout
	} else if !isFile(command.Stderr) && command.Stderr != nil {
		drains.stderr = &drainObserver{writer: command.Stderr}
		command.Stderr = drains.stderr
	}
	return drains
}

func (drains outputDrains) err() error {
	if drains.stdout != nil && drains.stdout.err() != nil {
		return drains.stdout.err()
	}
	if drains.stderr != nil {
		return drains.stderr.err()
	}
	return nil
}

func isFile(writer io.Writer) bool {
	_, ok := writer.(*os.File)
	return ok
}

func sameWriter(left io.Writer, right io.Writer) bool {
	if left == nil || right == nil {
		return false
	}
	leftType := reflect.TypeOf(left)
	return leftType == reflect.TypeOf(right) && leftType.Comparable() && left == right
}

// drainObserver records an error from os/exec's output-copying goroutine.
// Cmd.Wait drops that error when process exit itself fails.
type drainObserver struct {
	writer  io.Writer
	mu      sync.Mutex
	copyErr error
}

func (observer *drainObserver) Write(data []byte) (int, error) {
	return observer.writer.Write(data)
}

func (observer *drainObserver) ReadFrom(reader io.Reader) (int64, error) {
	written, err := io.Copy(observer.writer, reader)
	if err != nil {
		observer.mu.Lock()
		observer.copyErr = err
		observer.mu.Unlock()
	}
	return written, err
}

func (observer *drainObserver) err() error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.copyErr
}
