FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /kms ./cmd/kms

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 app
USER app
COPY --from=build /kms /kms
EXPOSE 8080
ENTRYPOINT ["/kms"]
CMD ["-mode","api"]
