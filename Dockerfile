FROM node:22-slim AS lisa-frontend
WORKDIR /ui
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build

FROM python:3.12-slim AS lisa-runtime
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    OPENHANDS_SUPPRESS_BANNER=1 \
    OH_PERSISTENCE_DIR=/var/lib/lisa/openhands
WORKDIR /app
COPY pyproject.toml ./pyproject.toml
COPY src ./src
COPY skills ./skills
COPY scripts/provision_lisa.py ./provision_lisa.py
COPY --from=lisa-frontend /ui/dist ./static
RUN pip install --no-cache-dir '.[openhands]'
RUN useradd --system --uid 10002 --create-home lisa && \
    mkdir -p /var/lib/lisa && chown -R lisa:lisa /var/lib/lisa /app
USER 10002:10002
EXPOSE 8082
CMD ["uvicorn", "lisa.app:app", "--host", "0.0.0.0", "--port", "8082"]
