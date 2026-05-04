FROM python:3.12-slim

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends nodejs npm \
    && rm -rf /var/lib/apt/lists/*

RUN npm install --omit=dev playwright@1.56.1 \
    && npx playwright install --with-deps chromium

COPY pyproject.toml .
COPY src ./src

RUN pip install --no-cache-dir .

ENV NODE_PATH=/app/node_modules

ENTRYPOINT ["mcp-glimmung-http"]
