FROM golang:1.27-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=${VERSION}" -o /oscwebmixer ./cmd/oscwebmixer

FROM scratch
ARG VERSION=dev
LABEL org.opencontainers.image.version="${VERSION}"
WORKDIR /webmixer
COPY --from=builder /oscwebmixer /webmixer/oscwebmixer
ENTRYPOINT ["/webmixer/oscwebmixer"]
