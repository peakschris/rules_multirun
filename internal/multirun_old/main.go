package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bazelbuild/rules_go/go/tools/bazel"
)

const (
	maxLineSize = 1 * 1024 * 1024
)

func debugEnv() {
	value := os.Getenv("RUNFILES_MANIFEST_FILE")
    fmt.Println("multirun startup, RUNFILES_MANIFEST_FILE="+value)

	// Check that the files can be listed.
	entries, _ := bazel.ListRunfiles()
	for _, e := range entries {
		fmt.Println(e.ShortPath, e.Path)
	}
}

// cancelOnInterrupt calls f when os.Interrupt or SIGTERM is received.
// It ignores subsequent interrupts on purpose - program should exit correctly after the first signal.
func cancelOnInterrupt(ctx context.Context, f context.CancelFunc) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
		case <-c:
			f()
		}
	}()
}

type command struct {
	Tag string `json:"tag"`
	// path is the path of the program to execute.
	Path string `json:"path"`
	Args []string `json:"args"`
	Env []string `json:"env"`
}

type instructions struct {
	Commands    []command `json:"commands"`
	Jobs        int       `json:"jobs"`
	Quiet       bool      `json:"quiet"`
	AddTag      bool      `json:"addTag"`
	StopOnError bool      `json:"stopOnError"`
}

func readInstructions(instructionsFile string) (instructions, error) {
	content, err := ioutil.ReadFile(instructionsFile)
	if err != nil {
		return instructions{}, fmt.Errorf("failed to read instructions file %q: %v", instructionsFile, err)
	}
	var instr instructions
	if err = json.Unmarshal(content, &instr); err != nil {
		return instructions{}, fmt.Errorf("failed to parse file %q as JSON: %v", instructionsFile, err)
	}
	return instr, nil
}

func debugEnv() {
	env := os.Environ()
	for _, e := range env {
			if strings.HasPrefix(e, "RUNFILES_") || strings.HasPrefix(e, "BUILD_") || strings.HasPrefix(e, "TEST_") {
					fmt.Println(e)
			}
	}

	// Check that the files can be listed.
	entries, _ := bazel.ListRunfiles()
	for _, e := range entries {
			fmt.Println(e.ShortPath, e.Path)
	}
}

// cancelOnInterrupt calls f when os.Interrupt or SIGTERM is received.
// It ignores subsequent interrupts on purpose - program should exit correctly after the first signal.
func cancelOnInterrupt(ctx context.Context, f context.CancelFunc) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
			select {
			case <-ctx.Done():
			case <-c:
					f()
			}
	}()
}

func invokingExe() (string) {
    if runtime.GOOS == "windows" {
         exe, _ := os.Executable()
         return exe
    }
    cwd := os.Getenv("PWD")
    exe, _ := strings.CutSuffix(cwd, ".runfiles/_main")
    return exe
}

func main() {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	cancelOnInterrupt(ctx, cancelFunc)

	// Because we are invoked via symlink, we can't accept arguments.
	// The instructions are adjacent to the symlink source.
	exe := invokingExe()
	basePath, _ := strings.CutSuffix(exe, ".exe")
	instructionsFile := basePath + ".json"
	instr, err := readInstructions(instructionsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	m := multirun{
		commands:    instr.Commands,
		stdoutSink:  os.Stdout,
		stderrSink:  os.Stderr,
		jobs:        instr.Jobs,
		quiet:       instr.Quiet,
		addTag:      instr.AddTag,
		stopOnError: instr.StopOnError,
	}
	err = m.run(ctx)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Some command failed. Don't print anything
			os.Exit(exitErr.ExitCode())
		}
		// Some sort of internal failure
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
