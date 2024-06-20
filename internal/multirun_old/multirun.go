package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/bazelbuild/rules_go/go/tools/bazel"
)

type multirun struct {
	commands               []command
	stdoutSink, stderrSink io.Writer
	args                   []string
	jobs                   int
	quiet                  bool
	addTag                 bool
	stopOnError            bool
}

func (m multirun) env() ([]string) {
	env := os.Environ()
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(m.stderrSink, "Failed to get current process path for multirun %s\n", err.Error())
		os.Exit(1)
	}
	env = append(env, "RUNFILES_MANIFEST_FILE=" + exe + ".runfiles_manifest")
	env = append(env, "RUNFILES_REPO_MAPPING=" + exe + ".repo_mapping")
	return env
}
	
func (m multirun) run(ctx context.Context) error {
	// minimum concurrency is 1, which is sequential
	if m.jobs < 0 {
		return fmt.Errorf("jobs must be at least 0")
	}

	if m.jobs == 0 {
		return m.unboundedExecution(ctx)
	}

	return m.boundedExecution(ctx)
}

func (m multirun) runfile(path string) (string, error) {
	fullPath, err1 := bazel.Runfile(path)
	if err1 != nil {
		strippedPath := strings.SplitN(path, "/", 2)[1]
		fullPath2, err2 := bazel.Runfile(strippedPath)
		if err2 != nil {
			fmt.Fprintf(m.stderrSink, "Failed to lookup runfile for %s %s\n", path, err1.Error())
			fmt.Fprintf(m.stderrSink, "also tried %s %s\n", strippedPath, err2.Error())
			return "", err1
		}
		fullPath = fullPath2
	}
	return fullPath, nil
}

// unboundedExecution execute multiple commands without concurrency limit
func (m multirun) unboundedExecution(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error)

	for _, cmd := range m.commands {
		fullPath, err := m.runfile(cmd.Path)
		if err != nil {
			fmt.Fprintf(m.stderrSink, "Failed to lookup runfile for %s %s\n", cmd.Path, err.Error())
			return err
		}
		p := process{
			tag:        cmd.Tag,
			path:       fullPath,
			stdoutSink: m.stdoutSink,
			stderrSink: m.stderrSink,
			args:       m.args,
			addTag:     m.addTag,
			env:        m.env(),
		}

		go func() {
			errs <- p.run(ctx)
		}()
	}

	var firstError error
	for range m.commands {
		err := <-errs
		if err != nil && firstError == nil {
			if m.stopOnError {
				cancel()
			}
			firstError = err
		}
	}

	return firstError
}

// boundedExecution execute multiple commands using a sized worker pool
func (m multirun) boundedExecution(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// errs should be buffered to avoid blocking
	// when len(m.commands) > m.jobs
	errs := make(chan error, len(m.commands))
	commands := make(chan command, len(m.commands))

	// send command to worker pool
	for _, cmd := range m.commands {
		commands <- cmd
	}
	close(commands)

	// start worker pool
	var wg sync.WaitGroup
	wg.Add(m.jobs)
	for w := 0; w < m.jobs; w++ {
		go func() {
			defer wg.Done()
			if m.spawnWorker(ctx, commands, errs) && m.stopOnError {
				cancel()
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		return err
	}

	return nil
}

// spawnWorker spawns a worker node that executes commands from the commands channel.
//
// All errors that occur will be pushed into the `errs` channel and `true` will be returned.
// If no errors occur, function returns `false`.
func (m multirun) spawnWorker(ctx context.Context, commands <-chan command, errs chan<- error) bool {
	for cmd := range commands {
		select {
		case <-ctx.Done():
			errs <- ctx.Err()
			return true
		default:
		}
		fullPath, err := m.runfile(cmd.Path)
		if err != nil {
			fmt.Fprintf(m.stderrSink, "Failed to lookup runfile for %s %s\n", cmd.Path, err.Error())
			return true
		}
		p := process{
			tag:  cmd.Tag,
			path: fullPath,
			// nil means "the process reads from the null device (os.DevNull)", see the godoc. We do this explicitly to show the intent.
			stdin:      nil,
			stdoutSink: m.stdoutSink,
			stderrSink: m.stderrSink,
			args:       m.args,
			env:        m.env(),
		}

		// when executing concurrently, tag should be added
		if m.jobs > 1 {
			p.addTag = m.addTag
		}

		// when executing sequentially, allow input from stdin
		if m.jobs == 1 {
			p.stdin = os.Stdin
		}

		if !m.quiet {
			fmt.Fprintf(m.stderrSink, "Running %s\n", cmd.Tag)
		}

		err = p.run(ctx)
		if err != nil {
			errs <- err
			if m.stopOnError {
				return true
			}
		}
	}
	return false
}
