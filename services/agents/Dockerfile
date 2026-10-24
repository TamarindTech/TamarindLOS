# Agent runtime. Build from the repository root:
#   docker build -f services/agents/Dockerfile -t tamarind-agents .
FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
RUN useradd --create-home --uid 10001 agents && mkdir -p /home/agents/state && chown agents:agents /home/agents/state
WORKDIR /app
COPY services/agents/tamarind_agents ./tamarind_agents
USER agents
ENV TAMARIND_AGENTS_STATE=/home/agents/state
ENTRYPOINT ["python", "-m", "tamarind_agents"]
CMD ["run"]
