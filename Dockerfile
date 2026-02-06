FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/rlqs-server ./cmd/rlqs-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /bin/rlqs-server /usr/local/bin/rlqs-server
ENTRYPOINT ["rlqs-server"]
