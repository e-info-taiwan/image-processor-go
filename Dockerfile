FROM golang:1.23 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/image-processor .

FROM python:3.11-slim
WORKDIR /app

# Install dependencies for Python sidecar
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Pre-download the AI model to bake it into the Docker image
RUN python -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('clip-ViT-B-32')"

# Copy Go binary
COPY --from=builder /out/image-processor /app/image-processor

# Copy Python sidecar and entrypoint
COPY vector_server.py entrypoint.sh ./
RUN chmod +x entrypoint.sh

EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
