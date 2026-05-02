# syntax=docker/dockerfile:1.7

FROM node:20-alpine AS frontend
WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm install --no-audit --no-fund
# `frontend/src/index.css` does `@import "../../design-system/colors_and_type.css"`
# (tokens live outside the frontend per CLAUDE.md "Frontend / design"); the
# build context needs the sibling directory available at /design-system so
# the relative path resolves.
COPY design-system /design-system
COPY frontend/ ./
RUN npm run build

FROM python:3.12-slim AS backend
WORKDIR /app
ENV PYTHONUNBUFFERED=1 PIP_DISABLE_PIP_VERSION_CHECK=1
COPY pyproject.toml ./
COPY src ./src
RUN pip install --no-cache-dir .
COPY --from=frontend /frontend/dist /app/static
ENV GLIMMUNG_STATIC_DIR=/app/static
EXPOSE 8000
USER 1000
CMD ["python", "-m", "glimmung"]
