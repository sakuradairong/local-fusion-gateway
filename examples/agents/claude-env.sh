#!/usr/bin/env bash
# Future template for Claude Code.
# Not currently supported because local-fusion-gateway does not implement Anthropic /v1/messages yet.

export LOCAL_FUSION_API_KEY="change-me-long-random-token"
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="$LOCAL_FUSION_API_KEY"

# Example future command:
# claude -p "Explain this repository"
