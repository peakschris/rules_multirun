package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io/ioutil"
    "os"
    "os/exec"
    "os/signal"
    "path/filepath"
    "runtime"
    "strings"
    "sync"
    "syscall"

	"github.com/bazelbuild/rules_go/go/tools/bazel"
)

func runfile(path string) (string, error) {
	fullPath, err1 := bazel.Runfile(path)
	if err1 != nil {
		strippedPath := strings.SplitN(path, "/", 2)[1]
		fullPath2, err2 := bazel.Runfile(strippedPath)
		if err2 != nil {
			fmt.Fprintf(os.Stderr, "Failed to lookup runfile for %s %s\n", path, err1.Error())
			fmt.Fprintf(os.Stderr, "also tried %s %s\n", strippedPath, err2.Error())
			return "", err1
		}
		fullPath = fullPath2
	}
	return fullPath, nil
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

type Command struct {
	Tag string `json:"tag"`
	Path string `json:"path"`
	Args []string `json:"args"`
	Env []string `json:"env"`
}

type Instructions struct {
	Commands []Command `json:"commands"`
	Jobs int `json:"jobs"`
    Print_command bool `json:"print_command"`
	Keep_going bool `json:"keep_going"`
    Buffer_output bool `json:"buffer_output"`
    Workspace_name string `json:"workspace_name"`
}

func readInstructions(instructionsFile string) (Instructions, error) {
	content, err := ioutil.ReadFile(instructionsFile)
	if err != nil {
		return Instructions{}, fmt.Errorf("failed to read instructions file %q: %v", instructionsFile, err)
	}
    fmt.Printf("%s\n", content)
	var instr Instructions
	if err = json.Unmarshal(content, &instr); err != nil {
		return Instructions{}, fmt.Errorf("failed to parse file %q as JSON: %v", instructionsFile, err)
	}
	return instr, nil
}

// scriptPath constructs the script path based on the workspace name and the relative path.
func scriptPath(workspaceName, path string) string {
    if filepath.IsAbs(path) {
        return path
    }
    return filepath.Join(workspaceName, path)
}

func runCommand(command Command, bufferOutput bool) (int, string, error) {
    var cmd *exec.Cmd
    args := command.Args
    env := os.Environ() // Convert map to format "key=value"
    for k, v := range command.Env {
        env = append(env, fmt.Sprintf("%s=%s", k, v))
    }

    cmdStr := command.Path + " " + strings.Join(args, " ")
    fmt.Println("Running command:", cmdStr)
    cmd = exec.Command(command.Path, args...)
    cmd.Env = env

    var stdoutBuf bytes.Buffer
    if bufferOutput {
        cmd.Stdout = &stdoutBuf
        cmd.Stderr = &stdoutBuf
    }

    err := cmd.Run() // Run and wait for the command to complete
    if err != nil {
        if exitError, ok := err.(*exec.ExitError); ok {
            return exitError.ExitCode(), stdoutBuf.String(), nil
        }
        return 0, stdoutBuf.String(), err
    }
    return 0, stdoutBuf.String(), nil
}

func performConcurrently(commands []Command, printCommand bool, bufferOutput bool) bool {
    var wg sync.WaitGroup
    success := true
    mu := &sync.Mutex{} // To safely update `success`
    fmt.Printf("%d\n", len(commands))

    for _, cmd := range commands {
        fmt.Println(cmd.Path)
        wg.Add(1)
        go func(cmd Command) {
            defer wg.Done()
            exitCode, output, err := runCommand(cmd, bufferOutput)
            if err != nil {
                fmt.Println("Error running command:", err)
                mu.Lock()
                success = false
                mu.Unlock()
                return
            }

            if printCommand {
                fmt.Println(cmd.Tag)
            }

            if bufferOutput {
                fmt.Println(output) // Print buffered output
            }

            if exitCode != 0 {
                mu.Lock()
                success = false
                mu.Unlock()
            }
        }(cmd)
    }

    wg.Wait() // Wait for all goroutines to finish
    return success
}

func performSerially(commands []Command, printCommand bool, keepGoing bool) bool {
    success := true
    for _, cmd := range commands {
        if printCommand {
            fmt.Println(cmd.Tag)
        }

        _, _, err := runCommand(cmd, false)
        if err != nil {
            if keepGoing {
                success = false
            } else {
                return false
            }
        }
    }
    return success
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

func windowsRunViaBash(command Command) (bool) {
    if runtime.GOOS == "windows" {
		if (strings.HasSuffix(command.Path, ".bash") || strings.HasSuffix(command.Path, ".bash")) {
			return true
		}
	}
	return false
}

func resolveCommands(commands []Command) ([]Command) {
    var out []Command
    bashPath := ""
    for _, command := range commands {
        path, err := runfile(command.Path)
        if err != nil {
            fmt.Fprintf(os.Stderr, "%+v\n", err)
            os.Exit(1)
        }
        command.Path = path
        if (windowsRunViaBash(command)) {
            if runtime.GOOS == "windows" && bashPath == "" {
                bash, err := exec.LookPath("bash.exe")
                if err != nil {
                    fmt.Errorf("error: bash.exe not found in PATH")
                    os.Exit(1)
                }
                bashPath = bash
            }
            command.Args = append([]string{"-c", command.Path + " \"$@\"", "--"}, command.Args...)
            command.Path = bashPath
        }
        out = append(out, command)
    }
    return out
}

func main() {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	cancelOnInterrupt(ctx, cancelFunc)
    debugEnv()

	// Because we are invoked via a symlink, we cannot accept any command line args
	// The instructions file is always adjacent to the symlink location
	exe := invokingExe()
	basePath, _ := strings.CutSuffix(exe, ".exe")
    //manifestFile := exe + ".runfiles_manifest"
	instructionsFile := basePath + ".json"
    fmt.Println(instructionsFile)
	instr, err := readInstructions(instructionsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}

    parallel := instr.Jobs == 0
    printCommand := instr.Print_command
    commands := resolveCommands(instr.Commands)
    var success bool
    if parallel {
        fmt.Println("calling performConcurrently")
        success = performConcurrently(commands, printCommand, instr.Buffer_output)
    } else {
        success = performSerially(commands, printCommand, instr.Keep_going)
    }

    if success {
        os.Exit(0)
    } else {
        os.Exit(1)
    }
}
