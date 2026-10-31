# Agent image: the Claude Code CLI and nothing else of interest. Pin the version, and
# rebuild deliberately: the adapter verifies the flags it needs at runtime.
FROM node:22-slim
ARG CLAUDE_CODE_VERSION=2.1.316
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
  && npm cache clean --force \
  && useradd --uid 10001 --create-home agent \
  && mkdir -p /claude-config /workspace && chown 10001:10001 /claude-config /workspace
USER 10001
ENV CLAUDE_CONFIG_DIR=/claude-config
WORKDIR /workspace
ENTRYPOINT []
CMD ["claude", "--version"]
