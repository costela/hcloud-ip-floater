FROM golang:1.13-alpine AS build

ENV CGO_ENABLED=0

RUN apk add --update git

WORKDIR /app

COPY . .

RUN go build -o main -trimpath \
    -ldflags "-X main.version=$(git describe --tags --dirty --always)"


FROM alpine

RUN apk add --no-cache ca-certificates

COPY --from=build /app/main /

CMD [ "/main" ]