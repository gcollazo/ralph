package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed Dockerfile
var dockerfileContent string

// ANSI color codes
const (
	colorReset = "\033[0m"
	colorDim   = "\033[2m" // Dim/faint text for Claude output
)

// StreamMessage represents a message from Claude's stream-json output
type StreamMessage struct {
	Type    string `json:"type"`
	Result  string `json:"result"` // For "result" type messages
	Message *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"` // For "assistant" type messages
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"` // For "content_block_delta" type messages (legacy)
	Event *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"` // For "stream_event" type messages with --include-partial-messages
}

const defaultMaxIterations = 10
const dockerImageName = "ralph-claude"

var debugMode bool
var freshMode bool

// version is set at build time using ldflags
var version = "dev"

const loopPrompt = `You are an autonomous coding agent. Follow these instructions:

1. Read PROGRESS.md for context on what has been done. If it doesn't exist, create it. Always in the root of the project, never move or delete this file.
2. Read TASKS.md and find the first incomplete task (marked with [ ]). Always in the root of the project, never move or delete this file.
3. Implement the task completely.
4. Run tests after implementation.
5. If tests and type checks pass, mark the task complete in TASKS.md by changing [ ] to [x].
6. Use git for version control and commit after completing each task.
7. Update PROGRESS.md with any learnings or notes.
8. Search the ENTIRE TASKS.md file for any remaining "[ ]" markers. If ANY incomplete tasks exist ANYWHERE in the file (including other features), end your turn normally and continue in the next iteration.
9. ONLY if you have verified that there are ZERO "[ ]" markers remaining in the ENTIRE TASKS.md file (all features complete), reply with EXACTLY: "<ralph>complete</ralph>"

CRITICAL: Do NOT return the completion marker after finishing just one feature. You must complete ALL features and ALL tasks across the entire TASKS.md file before returning the completion marker.`

const initPromptTemplate = `Main Task: %s

You are a software architect creating a comprehensive implementation plan. Your job is to produce a detailed, well-organized document that will guide a developer through building the software described above.

## Document Structure

Start with a **Summary** section that describes:
- What we are building (high-level overview)
- Key technologies and frameworks to use
- Core functionality and user value

Then organize the implementation into **Features**. Each feature should be a logical, cohesive unit of functionality.

## Feature Format

Assign each feature an ID in the format ` + "`XX-NNNN`" + ` where:
- XX = 2-letter project code (consistent across all features)
- NNNN = sequential feature number (0001, 0002, etc.)

For each feature include:
1. Feature title and description
2. Acceptance criteria (what "done" looks like)
3. Tasks to implement the feature
4. Testing plan (unit tests, integration tests, manual testing steps)
5. Quality checks (linter, formatter, type checker as relevant to the language)

## Task Format

Use ` + "`[ ] Task description`" + ` for incomplete tasks. Tasks can be referenced as ` + "`XX-NNNN-Y`" + ` where Y is the task number within the feature.

## Guidelines

- Break features into small, focused tasks (15-30 min each ideally)
- Each task should be independently completable and testable
- Prefer many small tasks over few large ones
- Include setup/scaffolding as the first feature
- Include a final feature for documentation and cleanup
- Consider dependencies between features and order them appropriately
- Be specific about file names, function names, and implementation details

Write the plan directly to a file named TASKS.md in the current directory. Use your file writing tool to create the file.`

const completionMarker = "<ralph>complete</ralph>"

var logger *log.Logger

// Global state for docker container cleanup
var (
	dockerContainerID   string
	dockerContainerName string
	dockerMutex         sync.Mutex
)

// getProjectName returns a sanitized project name from the current working directory
func getProjectName() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "project"
	}
	name := filepath.Base(cwd)
	// Sanitize: lowercase, alphanumeric + hyphens only, replace _ and spaces with -
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, " ", "-")
	var sanitized strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			sanitized.WriteRune(r)
		}
	}
	result := sanitized.String()
	if result == "" {
		return "project"
	}
	return result
}

// getContainerName returns the persistent container name for this project
func getContainerName() string {
	return "ralph-" + getProjectName()
}

// isContainerRunning checks if a container with the given name is running
func isContainerRunning(name string) bool {
	cmd := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name)
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == "true"
}

// containerExists checks if a container with the given name exists (running or stopped)
func containerExists(name string) bool {
	cmd := exec.Command("docker", "inspect", name)
	return cmd.Run() == nil
}

// ensureContainerRunning ensures a persistent container is running, creating it if needed
func ensureContainerRunning() (string, error) {
	name := getContainerName()
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current directory: %w", err)
	}

	// If fresh mode, remove existing container first
	if freshMode && containerExists(name) {
		logger.Printf("Fresh mode: removing existing container %s...", name)
		stopCmd := exec.Command("docker", "stop", name)
		stopCmd.Run() // Best effort
		rmCmd := exec.Command("docker", "rm", "-f", name)
		rmCmd.Run() // Best effort
	}

	// If container is running, reuse it
	if isContainerRunning(name) {
		logger.Printf("Reusing existing container: %s", name)
		return name, nil
	}

	// If container exists but is stopped, start it
	if containerExists(name) {
		logger.Printf("Starting stopped container: %s", name)
		cmd := exec.Command("docker", "start", name)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("failed to start container: %w", err)
		}
		return name, nil
	}

	// Container doesn't exist, create it (no credentials - passed at exec time)
	logger.Printf("Creating new container: %s", name)
	cmd := exec.Command("docker", "run",
		"-d",
		"--name", name,
		"-v", fmt.Sprintf("%s:/workspace", cwd),
		"-w", "/workspace",
		dockerImageName,
		"tail", "-f", "/dev/null",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to create container: %w\nOutput: %s", err, string(output))
	}

	return name, nil
}

func init() {
	logger = log.New(os.Stdout, "[ralph] ", log.Ldate|log.Ltime)
}

// getVersionString returns the full version string including Go version
func getVersionString() string {
	goVersion := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		goVersion = info.GoVersion
	}
	return fmt.Sprintf("ralph version %s (%s)", version, goVersion)
}

func main() {
	// Setup signal handling for cleanup
	setupSignalHandler()

	if len(os.Args) < 2 {
		// Default to "start" command with default max iterations, Docker enabled
		cmdStart(defaultMaxIterations, true)
		return
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// Handle version flags before other flag processing
	if cmd == "-v" || cmd == "--version" {
		fmt.Println(getVersionString())
		return
	}

	// Handle flags passed without a command (e.g., "ralph --debug")
	if strings.HasPrefix(cmd, "-") {
		// Treat as "start" command with the flag
		args = os.Args[1:]
		cmd = "start"
	}

	switch cmd {
	case "start", "run":
		if hasHelpFlag(args) {
			printStartHelp()
			return
		}
		maxIter := parseMaxIterations(args)
		useDocker := !hasNoDockerFlag(args) // Docker is default, --no-docker opts out
		debugMode = hasDebugFlag(args)
		freshMode = hasFreshFlag(args)
		cmdStart(maxIter, useDocker)
	case "init":
		if hasHelpFlag(args) {
			printInitHelp()
			return
		}
		if len(args) < 1 {
			fmt.Println("Error: missing task description")
			fmt.Println()
			printInitHelp()
			os.Exit(1)
		}
		// Filter out flags from description
		description := getDescriptionFromArgs(args)
		if description == "" {
			fmt.Println("Error: missing task description")
			fmt.Println()
			printInitHelp()
			os.Exit(1)
		}
		// Parse optional spec file
		specPath := parseSpecFlag(args)
		var specFilename string
		if specPath != "" {
			// Verify the file exists (but don't read it - Claude will read it)
			if _, err := os.Stat(specPath); os.IsNotExist(err) {
				fmt.Printf("Error: spec file '%s' not found\n", specPath)
				os.Exit(1)
			}
			specFilename = filepath.Base(specPath)
		}
		useDocker := !hasNoDockerFlag(args) // Docker is default, --no-docker opts out
		debugMode = hasDebugFlag(args)
		freshMode = hasFreshFlag(args)
		if err := runInit(description, useDocker, specFilename); err != nil {
			logger.Fatalf("Init failed: %v", err)
		}
	case "login":
		if hasHelpFlag(args) {
			printLoginHelp()
			return
		}
		cmdLogin()
	case "version":
		fmt.Println(getVersionString())
	case "help", "-h", "--help":
		printGlobalHelp()
	default:
		fmt.Printf("Error: unknown command '%s'\n\n", cmd)
		printGlobalHelp()
		os.Exit(1)
	}
}

func setupSignalHandler() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		logger.Println("Received interrupt signal, cleaning up...")
		cleanupDocker()
		os.Exit(1)
	}()
}

func cleanupDocker() {
	dockerMutex.Lock()
	defer dockerMutex.Unlock()

	// For persistent containers, just stop them (don't remove)
	if dockerContainerName != "" {
		logger.Printf("Stopping docker container %s...", dockerContainerName)
		cmd := exec.Command("docker", "stop", dockerContainerName)
		cmd.Run() // Best effort, ignore errors
		dockerContainerName = ""
		dockerContainerID = ""
		return
	}

	// Legacy cleanup for non-persistent containers
	if dockerContainerID != "" {
		logger.Printf("Stopping docker container %s...", dockerContainerID[:12])
		cmd := exec.Command("docker", "stop", dockerContainerID)
		cmd.Run() // Best effort, ignore errors

		cmd = exec.Command("docker", "rm", "-f", dockerContainerID)
		cmd.Run() // Best effort, ignore errors

		dockerContainerID = ""
	}
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "help" {
			return true
		}
	}
	return false
}

func hasNoDockerFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--no-docker" {
			return true
		}
	}
	return false
}

func hasDebugFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--debug" || arg == "-d" {
			return true
		}
	}
	return false
}

func hasFreshFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--fresh" {
			return true
		}
	}
	return false
}

func getDescriptionFromArgs(args []string) string {
	var parts []string
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "--max" || arg == "-m" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--max=") || strings.HasPrefix(arg, "-m=") {
			continue
		}
		if arg == "--spec" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--spec=") {
			continue
		}
		if arg == "--no-docker" {
			continue
		}
		if arg == "--fresh" {
			continue
		}
		if arg == "--debug" {
			continue
		}
		if arg == "-h" || arg == "--help" {
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

// parseMaxIterations parses the --max or -m flag from args
// Returns -1 for infinity, defaultMaxIterations if not specified
func parseMaxIterations(args []string) int {
	for i, arg := range args {
		var value string
		if arg == "--max" || arg == "-m" {
			if i+1 < len(args) {
				value = args[i+1]
			}
		} else if strings.HasPrefix(arg, "--max=") {
			value = strings.TrimPrefix(arg, "--max=")
		} else if strings.HasPrefix(arg, "-m=") {
			value = strings.TrimPrefix(arg, "-m=")
		}

		if value != "" {
			if value == "infinity" || value == "inf" || value == "∞" {
				return -1 // -1 represents infinity
			}
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				return n
			}
			fmt.Printf("Warning: invalid value for --max '%s', using default %d\n", value, defaultMaxIterations)
			return defaultMaxIterations
		}
	}
	return defaultMaxIterations
}

// parseSpecFlag parses the --spec flag from args
// Returns the spec file path if specified, empty string otherwise
func parseSpecFlag(args []string) string {
	for i, arg := range args {
		var value string
		if arg == "--spec" {
			if i+1 < len(args) {
				value = args[i+1]
			}
		} else if strings.HasPrefix(arg, "--spec=") {
			value = strings.TrimPrefix(arg, "--spec=")
		}
		if value != "" {
			return value
		}
	}
	return ""
}

func printGlobalHelp() {
	fmt.Println(`ralph - Automated task execution loop with Claude

Usage:
  ralph [command] [options]

Commands:
  start, run    Start the task execution loop (default if no command given)
  init          Generate TASKS.md from a task description
  login         Set up Claude API authentication
  version       Show version information
  help          Show this help message

Global Options:
  --no-docker   Run locally instead of in Docker container (default is Docker)

Authentication (required for Docker mode):
  Ralph tries authentication in this order:

  1. macOS Keychain (OAuth token) - for Claude Pro/Max subscribers:
     a. Run: claude setup-token
     b. Copy the token and store in keychain:
        security add-generic-password -s "ralph-claude-token" -a "ralph" -w "YOUR_TOKEN" -U
     (passed to container as CLAUDE_CODE_OAUTH_TOKEN)

  2. ANTHROPIC_API_KEY environment variable - for API users:
     export ANTHROPIC_API_KEY="sk-ant-api03-..."
     Get your API key from: https://console.anthropic.com/settings/keys
     (passed to container as ANTHROPIC_API_KEY)

Run 'ralph <command> --help' for more information on a specific command.

Examples:
  ralph                              Start in sandboxed Docker container (default)
  ralph start                        Start in sandboxed Docker container
  ralph start --no-docker            Run locally without Docker
  ralph init "Build a CLI"           Generate TASKS.md for building a CLI
  ralph init --spec SPEC.md "Use Go" Generate TASKS.md with spec file
  ralph version                      Show version information
  ralph help                         Show this help`)
}

func printStartHelp() {
	fmt.Println(`ralph start - Start the task execution loop

Usage:
  ralph start [options]
  ralph run [options]
  ralph [options]

Options:
  -m, --max <number>  Maximum number of iterations (default: 10)
                      Use "infinity" or "inf" for unlimited iterations
  --no-docker         Run locally instead of in Docker (default is Docker)
                      When running in Docker: sandboxed, dangerous mode enabled
                      Only mounts current directory, isolates from host system
  --fresh             Destroy and recreate the Docker container
                      Useful when dependencies change or container is in bad state

Description:
  Starts the automated task execution loop. Claude will read TASKS.md,
  work on the first incomplete task, run tests, and commit changes.
  The loop continues until all tasks are marked complete or the
  maximum number of iterations is reached.

  By default, runs in a Docker container with dangerous mode enabled
  (skips all permission checks). Use --no-docker to run locally.

  The Docker container persists between runs to preserve installed
  OS-level dependencies (e.g., packages installed via apt-get).
  Container name format: ralph-{projectname}

Requirements:
  - TASKS.md must exist and not be empty (run 'ralph init' first)
  - PROGRESS.md will be created by Claude if it doesn't exist
  - Docker must be installed and running (unless using --no-docker)
  - Claude token in keychain (see 'ralph help' for setup)

Examples:
  ralph start              Run in Docker (default, sandboxed)
  ralph start --no-docker  Run locally without Docker
  ralph start --max 5      Run with maximum 5 iterations
  ralph start -m infinity  Run until all tasks complete (no limit)
  ralph start --fresh      Start with a fresh container
  ralph --max=20           Run 20 iterations in Docker

The loop will exit when:
  - Claude outputs "<ralph>complete</ralph>" (all tasks done)
  - Maximum iterations reached`)
}

func printInitHelp() {
	fmt.Println(`ralph init - Generate TASKS.md from a task description

Usage:
  ralph init [options] <task description>

Options:
  --spec <file>   Path to a specification file with detailed project info
                  The file contents inform TASKS.md generation
                  Your instructions override or add to the spec
  --no-docker     Run locally instead of in Docker (default is Docker)
  --fresh         Destroy and recreate the Docker container

Description:
  Uses Claude to generate a comprehensive TASKS.md file based on your
  project description. The generated file will include:
  - Project summary
  - Features organized by ID (XX-NNNN format)
  - Small, focused tasks for each feature
  - Testing plans and quality checks

Arguments:
  <instructions>  When used without --spec: describes what you want to build
                  When used with --spec: provides technology choices,
                  corrections, or additional details that override the spec

Examples:
  ralph init Build a REST API for user management
  ralph init --spec PRODUCT.md "Use Go and SQLite, skip the auth feature"
  ralph init --spec SPEC.md "Use React with TypeScript"
  ralph init "Create a kanban board app with drag and drop"
  ralph init --no-docker Build a web scraper

Notes:
  - Will not overwrite an existing TASKS.md file
  - Run from the directory where you want to create the project
  - By default runs in Docker (sandboxed with dangerous mode)
  - The Docker container persists between runs (container: ralph-{projectname})`)
}

func printLoginHelp() {
	fmt.Println(`ralph login - Set up Claude API authentication

Usage:
  ralph login

Description:
  Interactively set up authentication credentials for the Claude API.
  Credentials are stored securely in the macOS Keychain.

Authentication Methods:
  1. OAuth Token (for Claude Pro/Max subscribers)
     - You'll be guided through running 'claude setup-token'
     - The token will be stored in keychain as 'ralph-claude-token'

  2. API Key (for Anthropic API users)
     - Enter your API key from https://console.anthropic.com/settings/keys
     - The key will be stored in keychain as 'ralph-claude-token'

Notes:
  - Running 'ralph login' will show your current authentication status
  - You can update existing credentials by running login again
  - To manually check keychain: security find-generic-password -s "ralph-claude-token" -w
  - To manually delete: security delete-generic-password -s "ralph-claude-token"`)
}

func cmdLogin() {
	fmt.Println("Ralph Login - Claude API Authentication Setup")
	fmt.Println("==============================================")
	fmt.Println()

	// Check current authentication status
	token, envVarName, err := getClaudeToken()
	if err == nil {
		// Credentials exist
		var authType string
		if envVarName == "CLAUDE_CODE_OAUTH_TOKEN" {
			authType = "OAuth token (keychain)"
		} else {
			authType = "API key (environment variable)"
		}

		// Mask the token for display
		masked := maskToken(token)
		fmt.Printf("Current authentication: %s\n", authType)
		fmt.Printf("Token: %s\n", masked)
		fmt.Println()

		if !promptYesNo("Do you want to update your credentials?") {
			fmt.Println("No changes made.")
			return
		}
		fmt.Println()
	} else {
		fmt.Println("No authentication credentials found.")
		fmt.Println()
	}

	// Prompt user to choose authentication method
	fmt.Println("Choose authentication method:")
	fmt.Println("  1. OAuth Token (for Claude Pro/Max subscribers)")
	fmt.Println("  2. API Key (for Anthropic API users)")
	fmt.Println()

	choice := promptChoice("Enter choice (1 or 2): ", []string{"1", "2"})

	switch choice {
	case "1":
		setupOAuthToken()
	case "2":
		setupAPIKey()
	}
}

func maskToken(token string) string {
	if len(token) <= 12 {
		return "****"
	}
	return token[:6] + "..." + token[len(token)-6:]
}

func promptYesNo(question string) bool {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("%s (y/n): ", question)
		input, err := reader.ReadString('\n')
		if err != nil {
			return false
		}
		input = strings.TrimSpace(strings.ToLower(input))
		if input == "y" || input == "yes" {
			return true
		}
		if input == "n" || input == "no" {
			return false
		}
		fmt.Println("Please enter 'y' or 'n'")
	}
}

func promptChoice(question string, valid []string) string {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print(question)
		input, err := reader.ReadString('\n')
		if err != nil {
			return valid[0]
		}
		input = strings.TrimSpace(input)
		for _, v := range valid {
			if input == v {
				return v
			}
		}
		fmt.Printf("Please enter one of: %s\n", strings.Join(valid, ", "))
	}
}

func promptString(question string) string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print(question)
	input, err := reader.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(input)
}

func setupOAuthToken() {
	fmt.Println()
	fmt.Println("OAuth Token Setup")
	fmt.Println("-----------------")
	fmt.Println()
	fmt.Println("To get your OAuth token, run the following command in your terminal:")
	fmt.Println()
	fmt.Println("  claude setup-token")
	fmt.Println()
	fmt.Println("This will open a browser for authentication and display your token.")
	fmt.Println()

	token := promptString("Paste your OAuth token here: ")
	if token == "" {
		fmt.Println("No token provided. Setup cancelled.")
		return
	}

	if err := storeInKeychain(token); err != nil {
		fmt.Printf("Error storing token: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("OAuth token stored successfully!")
	fmt.Println("You can now run 'ralph init' or 'ralph start' to use Claude.")
}

func setupAPIKey() {
	fmt.Println()
	fmt.Println("API Key Setup")
	fmt.Println("-------------")
	fmt.Println()
	fmt.Println("Get your API key from: https://console.anthropic.com/settings/keys")
	fmt.Println()

	apiKey := promptString("Enter your Anthropic API key: ")
	if apiKey == "" {
		fmt.Println("No API key provided. Setup cancelled.")
		return
	}

	// Basic validation
	if !strings.HasPrefix(apiKey, "sk-ant-") {
		fmt.Println("Warning: API key doesn't start with 'sk-ant-'. This may not be a valid Anthropic API key.")
		if !promptYesNo("Continue anyway?") {
			fmt.Println("Setup cancelled.")
			return
		}
	}

	if err := storeInKeychain(apiKey); err != nil {
		fmt.Printf("Error storing API key: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("API key stored successfully!")
	fmt.Println("You can now run 'ralph init' or 'ralph start' to use Claude.")
}

func storeInKeychain(token string) error {
	// Use -U flag to update if exists, otherwise add
	cmd := exec.Command("security", "add-generic-password",
		"-s", "ralph-claude-token",
		"-a", "ralph",
		"-w", token,
		"-U")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain error: %w\nOutput: %s", err, string(output))
	}
	return nil
}

func cmdStart(maxIterations int, useDocker bool) {
	if useDocker {
		if err := runInDocker(maxIterations); err != nil {
			logger.Fatalf("Error: %v", err)
		}
	} else {
		if err := run(maxIterations); err != nil {
			logger.Fatalf("Error: %v", err)
		}
	}
}

func checkDockerAvailable() error {
	cmd := exec.Command("docker", "info")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf(`Docker is not available or not running.

To run ralph in Docker (recommended, sandboxed):
  - Install Docker: https://docs.docker.com/get-docker/
  - Start the Docker daemon

To run locally without Docker (use with caution):
  ralph start --no-docker

WARNING: Running without Docker means Claude will have direct access to your
filesystem and can execute commands on your machine. Only use --no-docker
if you understand the risks and trust the tasks being executed.`)
	}
	return nil
}

// ensureDockerImage checks if the ralph-claude image exists, builds it if not
func ensureDockerImage() error {
	// Check if image already exists
	cmd := exec.Command("docker", "image", "inspect", dockerImageName)
	if err := cmd.Run(); err == nil {
		// Image exists
		return nil
	}

	logger.Println("Building Docker image (first run only)...")

	// Get current working directory for build context
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	// Write Dockerfile to current directory
	dockerfilePath := filepath.Join(cwd, "Dockerfile.ralph")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644); err != nil {
		return fmt.Errorf("failed to write Dockerfile: %w", err)
	}
	defer os.Remove(dockerfilePath) // Clean up after build

	// Build the image
	buildCmd := exec.Command("docker", "build",
		"-t", dockerImageName,
		"-f", dockerfilePath,
		"--build-arg", fmt.Sprintf("TZ=%s", getTimezone()),
		".")

	// Stream build output in dim color
	stdout, err := buildCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := buildCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := buildCmd.Start(); err != nil {
		return fmt.Errorf("failed to start docker build: %w", err)
	}

	// Stream output in dim color
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		reader := bufio.NewReader(stdout)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				fmt.Print(colorDim + line + colorReset)
			}
			if err != nil {
				break
			}
		}
	}()

	go func() {
		defer wg.Done()
		reader := bufio.NewReader(stderr)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				fmt.Fprint(os.Stderr, colorDim+line+colorReset)
			}
			if err != nil {
				break
			}
		}
	}()

	wg.Wait()

	if err := buildCmd.Wait(); err != nil {
		return fmt.Errorf("failed to build Docker image: %w", err)
	}

	logger.Println("Docker image built successfully")
	return nil
}

func getTimezone() string {
	tz := os.Getenv("TZ")
	if tz != "" {
		return tz
	}
	// Try to read from /etc/localtime link
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		// Extract timezone from path like /var/db/timezone/zoneinfo/America/New_York
		parts := strings.Split(target, "/zoneinfo/")
		if len(parts) == 2 {
			return parts[1]
		}
	}
	return "UTC"
}

// getClaudeToken retrieves the Claude authentication token and the env var name to use
// Priority: 1. ralph-claude-token keychain entry (OAuth), 2. ANTHROPIC_API_KEY env var
func getClaudeToken() (token string, envVarName string, err error) {
	// First try macOS Keychain (OAuth token for Pro/Max subscribers)
	cmd := exec.Command("security", "find-generic-password", "-s", "ralph-claude-token", "-w")
	output, cmdErr := cmd.Output()
	if cmdErr == nil {
		token = strings.TrimSpace(string(output))
		if token != "" {
			return token, "CLAUDE_CODE_OAUTH_TOKEN", nil
		}
	}

	// Fall back to ANTHROPIC_API_KEY environment variable
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key, "ANTHROPIC_API_KEY", nil
	}

	// Neither found, return error with setup instructions
	return "", "", fmt.Errorf(`could not find Claude authentication credentials

To authenticate, use one of these methods:

Option 1: OAuth token (for Claude Pro/Max subscribers)
  a. Run: claude setup-token
  b. Copy the token and store in keychain:
     security add-generic-password -s "ralph-claude-token" -a "ralph" -w "YOUR_TOKEN" -U

Option 2: API key (for Anthropic API users)
  export ANTHROPIC_API_KEY="sk-ant-api03-..."
  Get your API key from: https://console.anthropic.com/settings/keys`)
}

func runInDocker(maxIterations int) error {
	// Check Docker is available
	if err := checkDockerAvailable(); err != nil {
		return err
	}

	// Get token and determine which env var to use
	apiKey, envVarName, err := getClaudeToken()
	if err != nil {
		return err
	}

	if debugMode {
		// Show first 20 and last 10 chars of the key for debugging
		masked := apiKey
		if len(apiKey) > 30 {
			masked = apiKey[:20] + "..." + apiKey[len(apiKey)-10:]
		}
		logger.Printf("[DEBUG] Using %s: %s (length: %d)", envVarName, masked, len(apiKey))
	}

	// Ensure Docker image is built
	if err := ensureDockerImage(); err != nil {
		return err
	}

	// Check TASKS.md exists before starting container
	if err := checkTasksFile(); err != nil {
		return err
	}

	// Ensure container is running (persistent container)
	containerName, err := ensureContainerRunning()
	if err != nil {
		return fmt.Errorf("failed to ensure container running: %w", err)
	}

	// Store container name for cleanup
	dockerMutex.Lock()
	dockerContainerName = containerName
	dockerMutex.Unlock()

	if maxIterations == -1 {
		logger.Println("Starting ralph loop in Docker (unlimited iterations, dangerous mode)")
	} else {
		logger.Printf("Starting ralph loop in Docker (max %d iterations, dangerous mode)", maxIterations)
	}

	// Run the loop
	iteration := 1
	for {
		if maxIterations != -1 {
			logger.Printf("Starting iteration %d/%d (Docker)", iteration, maxIterations)
		} else {
			logger.Printf("Starting iteration %d (Docker)", iteration)
		}
		startTime := time.Now()

		output, err := runClaudeInDocker(containerName, loopPrompt, apiKey, envVarName)
		if err != nil {
			return fmt.Errorf("claude execution failed: %w", err)
		}

		duration := time.Since(startTime).Round(time.Second)
		logger.Printf("Iteration %d completed in %v", iteration, duration)

		if strings.Contains(output, completionMarker) {
			logger.Println("All tasks complete!")
			return nil
		}

		// Check if we've reached the max iterations
		if maxIterations != -1 && iteration >= maxIterations {
			logger.Printf("Maximum iterations (%d) reached. Run 'ralph start' to continue.", maxIterations)
			return nil
		}

		logger.Printf("Completion marker not found, continuing to iteration %d", iteration+1)
		iteration++
	}
}

// runClaudeCommand executes a Claude command, streams output to terminal, and returns the final response.
// Streaming text is displayed in real-time but not accumulated.
// Only the final response from "result" or "assistant" message types is captured and returned.
// This prevents false completion marker detection from streaming text.
func runClaudeCommand(cmd *exec.Cmd) (string, error) {
	// Create pipes for stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start command: %w", err)
	}

	// Only capture the final response, not streaming text
	var finalResponse strings.Builder
	var wg sync.WaitGroup

	// Stream stdout - parse JSON, display in real-time, only capture final response
	wg.Add(1)
	go func() {
		defer wg.Done()
		reader := bufio.NewReader(stdout)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				if debugMode {
					logger.Printf("DEBUG: Raw line: %s", strings.TrimSpace(line))
				}
				var msg StreamMessage
				if jsonErr := json.Unmarshal([]byte(line), &msg); jsonErr == nil {
					if debugMode {
						logger.Printf("DEBUG: Parsed message type: %s", msg.Type)
					}
					switch msg.Type {
					case "stream_event":
						// Streaming deltas - display but don't accumulate
						if msg.Event != nil && msg.Event.Type == "content_block_delta" && msg.Event.Delta != nil && msg.Event.Delta.Text != "" {
							fmt.Print(colorDim + msg.Event.Delta.Text + colorReset)
						}
					case "content_block_delta":
						// Legacy format - display but don't accumulate
						if msg.Delta != nil && msg.Delta.Text != "" {
							fmt.Print(colorDim + msg.Delta.Text + colorReset)
						}
					case "assistant":
						// Final message - capture for completion marker check
						if msg.Message != nil {
							for _, content := range msg.Message.Content {
								if content.Type == "text" && content.Text != "" {
									finalResponse.WriteString(content.Text)
								}
							}
						}
					case "result":
						// Final result - capture for completion marker check
						if msg.Result != "" {
							finalResponse.WriteString(msg.Result)
						}
					}
				} else if debugMode {
					logger.Printf("DEBUG: JSON parse error: %v", jsonErr)
				}
			}
			if err != nil {
				if err != io.EOF {
					logger.Printf("stdout read error: %v", err)
				}
				break
			}
		}
		fmt.Println(colorReset)
	}()

	// Stream stderr to terminal
	wg.Add(1)
	go func() {
		defer wg.Done()
		reader := bufio.NewReader(stderr)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				fmt.Fprint(os.Stderr, colorDim+line+colorReset)
			}
			if err != nil {
				if err != io.EOF {
					logger.Printf("stderr read error: %v", err)
				}
				break
			}
		}
	}()

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("command exited with error: %w", err)
	}

	return finalResponse.String(), nil
}

func runClaudeInDocker(containerName string, prompt string, apiKey string, envVarName string) (string, error) {
	// Build docker exec command - pass token at exec time for fresh credentials
	args := []string{
		"exec",
		"-e", envVarName + "=" + apiKey,
		containerName,
		"claude",
		"--dangerously-skip-permissions",
		"--print",
		"--verbose",
		"--output-format", "stream-json",
		"--include-partial-messages",
		prompt,
	}

	cmd := exec.Command("docker", args...)
	return runClaudeCommand(cmd)
}

func runInit(description string, useDocker bool, specFilename string) error {
	logger.Printf("Initializing project with task: %s", description)

	// Check if TASKS.md already exists
	if _, err := os.Stat("TASKS.md"); err == nil {
		return fmt.Errorf("TASKS.md already exists, refusing to overwrite")
	}

	// Build the prompt based on whether spec file is provided
	var prompt string
	if specFilename != "" {
		// When spec is provided, tell Claude to read it instead of embedding contents
		prompt = fmt.Sprintf(`First, read the specification file "%s" in the current directory. This file contains the project specification.

<instructions>
%s
</instructions>

The specification file contains the project details. The <instructions> contain corrections, overrides, and technical guidance from the user that take precedence over the spec. When there are conflicts, always follow <instructions>.

`, specFilename, description)

		// Append the architect instructions (without "Main Task: %s")
		prompt += `You are a software architect creating a comprehensive implementation plan. Your job is to produce a detailed, well-organized document that will guide a developer through building the software described above.

## Document Structure

Start with a **Summary** section that describes:
- What we are building (high-level overview)
- Key technologies and frameworks to use
- Core functionality and user value

Then organize the implementation into **Features**. Each feature should be a logical, cohesive unit of functionality.

## Feature Format

Assign each feature an ID in the format ` + "`XX-NNNN`" + ` where:
- XX = 2-letter project code (consistent across all features)
- NNNN = sequential feature number (0001, 0002, etc.)

For each feature include:
1. Feature title and description
2. Acceptance criteria (what "done" looks like)
3. Tasks to implement the feature
4. Testing plan (unit tests, integration tests, manual testing steps)
5. Quality checks (linter, formatter, type checker as relevant to the language)

## Task Format

Use ` + "`[ ] Task description`" + ` for incomplete tasks. Tasks can be referenced as ` + "`XX-NNNN-Y`" + ` where Y is the task number within the feature.

## Guidelines

- Break features into small, focused tasks (15-30 min each ideally)
- Each task should be independently completable and testable
- Prefer many small tasks over few large ones
- Include setup/scaffolding as the first feature
- Include a final feature for documentation and cleanup
- Consider dependencies between features and order them appropriately
- Be specific about file names, function names, and implementation details

Write the plan directly to a file named TASKS.md in the current directory. Use your file writing tool to create the file.`

		// Add instruction to reference spec file in output
		prompt += fmt.Sprintf("\n\nInclude a note at the top of TASKS.md (after the title) referencing the specification file (%s) that informed this plan.", specFilename)
	} else {
		// No spec - use original template as-is
		prompt = fmt.Sprintf(initPromptTemplate, description)
	}

	logger.Println("Generating TASKS.md with Claude...")
	startTime := time.Now()

	var err error

	if useDocker {
		// Check Docker is available
		if err := checkDockerAvailable(); err != nil {
			return err
		}

		// Get token and determine which env var to use
		apiKey, envVarName, err := getClaudeToken()
		if err != nil {
			return err
		}

		// Ensure Docker image is built
		if err := ensureDockerImage(); err != nil {
			return err
		}

		// Ensure container is running (persistent container)
		containerName, err := ensureContainerRunning()
		if err != nil {
			return fmt.Errorf("failed to ensure container running: %w", err)
		}

		// Store container name for cleanup
		dockerMutex.Lock()
		dockerContainerName = containerName
		dockerMutex.Unlock()

		_, err = runClaudeInDocker(containerName, prompt, apiKey, envVarName)
	} else {
		_, err = runClaudeWithPrompt(prompt)
	}

	if err != nil {
		return fmt.Errorf("failed to generate tasks: %w", err)
	}

	logger.Printf("TASKS.md created successfully in %v", time.Since(startTime).Round(time.Second))
	return nil
}

func run(maxIterations int) error {
	if maxIterations == -1 {
		logger.Println("Starting ralph loop (unlimited iterations)")
	} else {
		logger.Printf("Starting ralph loop (max %d iterations)", maxIterations)
	}

	// Check TASKS.md exists and is not empty
	if err := checkTasksFile(); err != nil {
		return err
	}
	logger.Println("TASKS.md validated")

	// Run the ralph loop
	iteration := 1
	for {
		if maxIterations != -1 {
			logger.Printf("Starting iteration %d/%d", iteration, maxIterations)
		} else {
			logger.Printf("Starting iteration %d", iteration)
		}
		startTime := time.Now()

		output, err := runClaudeWithPrompt(loopPrompt)
		if err != nil {
			return fmt.Errorf("claude execution failed: %w", err)
		}

		duration := time.Since(startTime).Round(time.Second)
		logger.Printf("Iteration %d completed in %v", iteration, duration)

		if strings.Contains(output, completionMarker) {
			logger.Println("All tasks complete!")
			return nil
		}

		// Check if we've reached the max iterations
		if maxIterations != -1 && iteration >= maxIterations {
			logger.Printf("Maximum iterations (%d) reached. Run 'ralph start' to continue.", maxIterations)
			return nil
		}

		logger.Printf("Completion marker not found, continuing to iteration %d", iteration+1)
		iteration++
	}
}

func checkTasksFile() error {
	info, err := os.Stat("TASKS.md")
	if os.IsNotExist(err) {
		return fmt.Errorf("TASKS.md does not exist. Run 'ralph init <description>' to create one")
	}
	if err != nil {
		return fmt.Errorf("failed to check TASKS.md: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("TASKS.md is empty")
	}
	return nil
}

func runClaudeWithPrompt(prompt string) (string, error) {
	cmd := exec.Command("claude", "--print", "--verbose", "--output-format", "stream-json", "--include-partial-messages", prompt)
	return runClaudeCommand(cmd)
}
