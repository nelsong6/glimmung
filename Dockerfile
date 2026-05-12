# syntax=docker/dockerfile:1.7

FROM node:20-alpine AS frontend
WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
# `frontend/src/index.css` does `@import "../../design-system/colors_and_type.css"`
# (tokens live outside the frontend per CLAUDE.md "Frontend / design"); the
# build context needs the sibling directory available at /design-system so
# the relative path resolves.
COPY design-system /design-system
COPY frontend/ ./
RUN npm run build

FROM golang:1.23-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/glimmung ./cmd/glimmung-go

FROM alpine:3.21 AS runtime
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=backend /out/glimmung /app/glimmung
COPY --from=frontend /frontend/dist /app/static
ENV GLIMMUNG_STATIC_DIR=/app/static
EXPOSE 8000
USER 1000
CMD ["/app/glimmung"]
