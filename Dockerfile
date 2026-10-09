FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /server .

FROM gcr.io/distroless/static
COPY --from=build /server /server
ENV PORT=8000
EXPOSE 8000
ENTRYPOINT ["/server"]
