FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /dashboard ./cmd/dashboard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /dashboard /dashboard
EXPOSE 8080
ENTRYPOINT ["/dashboard"]
