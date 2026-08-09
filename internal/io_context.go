package internal

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

var errProcessOutputIncomplete = errors.New("process output incomplete")

const (
	processRelayEnv          = "COMPILEDB_INTERNAL_PROCESS_RELAY"
	processRelayEncodingEnv  = "COMPILEDB_INTERNAL_PROCESS_RELAY_ENCODING"
	processRelayArgument     = "--compiledb-internal-process-relay"
	processPathRelayArgument = "--compiledb-internal-path-relay"
)

func init() {
	if os.Getenv(processRelayEnv) != "1" || len(os.Args) < 2 {
		return
	}
	if len(os.Args) == 3 && os.Args[1] == processPathRelayArgument {
		file, err := os.Open(os.Args[2])
		if err != nil {
			os.Exit(1)
		}
		_, err = io.Copy(os.Stdout, file)
		_ = file.Close()
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) != 2 || os.Args[1] != processRelayArgument {
		return
	}

	var input io.Reader = os.Stdin
	if os.Getenv(processRelayEncodingEnv) == EncodingGB18030 {
		input = transform.NewReader(input, simplifiedchinese.GB18030.NewDecoder())
	}
	buffer := make([]byte, 32*1024)
	for {
		count, readErr := input.Read(buffer)
		if count > 0 {
			if _, err := os.Stdout.Write(buffer[:count]); err != nil {
				os.Exit(1)
			}
			_, _ = os.Stderr.Write([]byte{'P'})
		}
		if readErr == io.EOF {
			_, _ = os.Stderr.Write([]byte{'D'})
			os.Exit(0)
		}
		if readErr != nil {
			os.Exit(1)
		}
	}
}

type outputRelay struct {
	command  *exec.Cmd
	input    *os.File
	done     chan struct{}
	progress chan struct{}
	complete chan struct{}
	err      error
	once     sync.Once
}

func relayEnvironment(encoding string) []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(name) {
		case processRelayEnv, processRelayEncodingEnv, "LD_PRELOAD", "LD_AUDIT", "DYLD_INSERT_LIBRARIES":
			continue
		}
		environment = append(environment, value)
	}
	return append(environment, processRelayEnv+"=1", processRelayEncodingEnv+"="+encoding)
}

func startOutputRelay(target *os.File, encoding string) (*outputRelay, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	command := exec.Command(executable, processRelayArgument)
	command.Stdin = reader
	command.Stdout = target
	command.Stderr = eventWriter
	command.Env = relayEnvironment(encoding)
	if err := command.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		_ = eventReader.Close()
		_ = eventWriter.Close()
		return nil, err
	}
	_ = reader.Close()
	_ = eventWriter.Close()
	relay := &outputRelay{
		command:  command,
		input:    writer,
		done:     make(chan struct{}),
		progress: make(chan struct{}, 1),
		complete: make(chan struct{}),
	}
	go func() {
		relay.err = command.Wait()
		close(relay.done)
	}()
	go func() {
		defer eventReader.Close()
		buffer := make([]byte, 128)
		for {
			count, err := eventReader.Read(buffer)
			for _, event := range buffer[:count] {
				if event == 'D' {
					close(relay.complete)
					return
				}
				if event == 'P' {
					select {
					case relay.progress <- struct{}{}:
					default:
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return relay, nil
}

func (r *outputRelay) closeInput() {
	r.once.Do(func() { _ = r.input.Close() })
}

func (r *outputRelay) stop() {
	r.closeInput()
	if r.command.Process != nil {
		_ = r.command.Process.Kill()
	}
	<-r.done
}

func (r *outputRelay) wait(ctx context.Context, timeout time.Duration) error {
	var timeoutChannel <-chan time.Time
	var timer *time.Timer
	var deadlineChannel <-chan time.Time
	var deadlineTimer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timeoutChannel = timer.C
		deadlineTimer = time.NewTimer(3 * timeout)
		defer deadlineTimer.Stop()
		deadlineChannel = deadlineTimer.C
	}
	for {
		select {
		case <-r.done:
			return r.err
		case <-r.complete:
			r.stop()
			return nil
		case <-r.progress:
			if timer != nil {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(timeout)
				timeoutChannel = timer.C
			}
		case <-ctx.Done():
			r.stop()
			return context.Cause(ctx)
		case <-timeoutChannel:
			r.stop()
			return errProcessOutputIncomplete
		case <-deadlineChannel:
			r.stop()
			return errProcessOutputIncomplete
		}
	}
}

func writeFileWithContext(ctx context.Context, file *os.File, data []byte) error {
	relay, err := startOutputRelay(file, EncodingRaw)
	if err != nil {
		return err
	}
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := relay.input.Write(data)
		relay.closeInput()
		writeDone <- writeErr
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			relay.stop()
			return err
		}
		return relay.wait(ctx, 0)
	case <-ctx.Done():
		relay.stop()
		<-writeDone
		return context.Cause(ctx)
	}
}

func readFileWithContext(ctx context.Context, file *os.File) ([]byte, error) {
	return readRelayWithContext(ctx, file)
}

func readPathWithContext(ctx context.Context, filename string) ([]byte, error) {
	return readRelayWithContext(ctx, nil, filename)
}

func readRelayWithContext(ctx context.Context, file *os.File, path ...string) ([]byte, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	arguments := []string{processRelayArgument}
	if len(path) > 0 {
		arguments = []string{processPathRelayArgument, path[0]}
	}
	command := exec.Command(executable, arguments...)
	if file != nil {
		command.Stdin = file
	}
	command.Stdout = writer
	command.Env = relayEnvironment(EncodingRaw)
	if err := command.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	_ = writer.Close()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	canceled := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = command.Process.Kill()
		case <-canceled:
		}
	}()
	data, readErr := io.ReadAll(reader)
	_ = reader.Close()
	close(canceled)
	waitErr := <-done
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if readErr != nil {
		return data, readErr
	}
	return data, waitErr
}
