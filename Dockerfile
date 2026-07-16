# Python runtime (default and backward-compatible build target).
FROM python:3.12-slim AS python

WORKDIR /app

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    TZ=Asia/Shanghai

RUN apt-get update \
    && apt-get install -y --no-install-recommends tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY app ./app

RUN mkdir -p /data
VOLUME ["/data"]
EXPOSE 8080

CMD ["uvicorn", "app.web:app", "--host", "0.0.0.0", "--port", "8080"]


# Go build stage. The SQLite driver is pure Go and needs no CGO setting.
FROM golang:1.24-bookworm AS go-builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY app/assets.go app/init.html app/dashboard.html ./app/

RUN go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/cdtalive ./cmd/cdtalive


# Go runtime image.
FROM debian:bookworm-slim AS go

WORKDIR /app

ENV TZ=Asia/Shanghai \
    CDT_CONFIG_FILE=/data/cdtalive.yaml \
    CDT_DB_FILE=/data/cdtalive.db \
    CDT_WEB_ADDR=0.0.0.0:8080

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /data

COPY --from=go-builder /out/cdtalive /app/cdtalive

VOLUME ["/data"]
EXPOSE 8080

CMD ["/app/cdtalive"]


# Keep `docker build .` backward compatible: it still produces Python.
FROM python AS final
