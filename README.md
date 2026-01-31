# ralph

Automated task execution loop with Claude.

## Usage

```
ralph - Automated task execution loop with Claude

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
  ralph help                         Show this help
```

## Commands

### login

```
ralph login - Set up Claude API authentication

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
  - To manually delete: security delete-generic-password -s "ralph-claude-token"
```

### init

```
ralph init - Generate TASKS.md from a task description

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
  - The Docker container persists between runs (container: ralph-{projectname})
```

### start

```
ralph start - Start the task execution loop

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
  - PROGRESS.md will be created if it doesn't exist
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
  - Maximum iterations reached
```
