FROM golang:1.23 AS builder

WORKDIR /src
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/image-processor .

FROM python:3.11-slim
WORKDIR /app

# Install dependencies for Python sidecar
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy Go binary
COPY --from=builder /out/image-processor /app/image-processor

# Copy Python sidecar and entrypoint
COPY vector_server.py entrypoint.sh ./
RUN chmod +x entrypoint.sh

EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
