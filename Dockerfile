FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY . .

RUN go build -o /app/awp-exporter

FROM scratch AS live

COPY --from=builder /app/awp-exporter /awp-exporter

EXPOSE 6255
ENTRYPOINT ["./awp-exporter"]
