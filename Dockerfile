FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go manifest.json ./
RUN go test ./...
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o /out/yambridge .

FROM scratch AS binary
COPY --from=build /out/yambridge /yambridge
