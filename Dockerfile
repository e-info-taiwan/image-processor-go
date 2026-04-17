FROM golang:1.23 AS builder

WORKDIR /src
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/image-processor .

FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=builder /out/image-processor /image-processor

EXPOSE 8080
ENTRYPOINT ["/image-processor"]
