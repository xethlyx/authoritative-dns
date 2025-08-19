# Build the application from source
FROM golang:1.25 AS build-stage

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build ./cmd/server -o /authoritative-dns

# Run the tests in the container
# FROM build-stage AS run-test-stage
# RUN go test -v ./...

# Deploy the application binary into a lean image
FROM gcr.io/distroless/base-debian12 AS build-release-stage

WORKDIR /
COPY --from=build-stage /authoritative-dns /authoritative-dns

EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/authoritative-dns"]