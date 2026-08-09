package internal

import (
	"context"
	"fmt"
	"os"
)

type SignalError struct {
	ProcessSignal os.Signal
}

func (e SignalError) Error() string {
	return fmt.Sprintf("received signal %v", e.ProcessSignal)
}

func (e SignalError) Signal() os.Signal {
	return e.ProcessSignal
}

func contextExitCode(ctx context.Context) int {
	if cause, ok := context.Cause(ctx).(interface{ Signal() os.Signal }); ok {
		if code, ok := processSignalExitCode(cause.Signal()); ok {
			return code
		}
	}
	return 1
}

func SignalExitCode(signal os.Signal) int {
	if code, ok := processSignalExitCode(signal); ok {
		return code
	}
	return 1
}
