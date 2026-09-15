FROM golang:1.27-alpine AS build
WORKDIR /src
COPY *.go .
RUN go mod init faultgen && CGO_ENABLED=0 go build -o /faultgen .

FROM gcr.io/distroless/static-debian12
COPY --from=build /faultgen /faultgen
EXPOSE 8080
ENTRYPOINT ["/faultgen"]
