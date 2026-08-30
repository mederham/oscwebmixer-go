FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /oscwebmixer ./cmd/oscwebmixer

FROM scratch
WORKDIR /webmixer
COPY --from=builder /oscwebmixer /webmixer/oscwebmixer
ENTRYPOINT ["/webmixer/oscwebmixer"]
