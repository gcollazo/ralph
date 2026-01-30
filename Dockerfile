FROM node:20

ARG TZ
ENV TZ="$TZ"

ARG CLAUDE_CODE_VERSION=latest

# Install git (already has node/npm from base image)
RUN apt-get update && apt-get install -y git && rm -rf /var/lib/apt/lists/*

# Install Claude CLI globally
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}

# Create workspace directory
RUN mkdir -p /workspace && chown node:node /workspace

# Switch to node user (home is /home/node)
USER node

WORKDIR /workspace
