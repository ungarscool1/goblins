# Agent image: the Codex CLI and nothing else of interest. Pin the version, and rebuild
# deliberately: the adapter verifies what it needs at runtime and refuses a CLI without it.
FROM node:22-slim
ARG CODEX_VERSION=0.164.0
# The CLI verifies TLS against the system's certificates, which this base image does not have.
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/* \
  && npm install -g "@openai/codex@${CODEX_VERSION}" \
  && npm cache clean --force \
  && useradd --uid 10001 --create-home agent \
  && mkdir -p /codex-home /workspace && chown 10001:10001 /codex-home /workspace
USER 10001
ENV CODEX_HOME=/codex-home
WORKDIR /workspace
ENTRYPOINT []
CMD ["codex", "--version"]
