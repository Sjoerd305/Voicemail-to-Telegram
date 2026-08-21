# Stage 1: build the TypeScript frontend
FROM node:22-alpine AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ ./
RUN mkdir -p ../internal/web/dist && npm run build

# Stage 2: build the Go binary
FROM golang:1.26-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/internal/web/dist ./internal/web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /voicemailbot ./cmd/voicemailbot

# Stage 3: minimal runtime image
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Europe/Amsterdam
COPY --from=backend /voicemailbot /usr/local/bin/voicemailbot
VOLUME ["/config", "/data"]
EXPOSE 8080
ENTRYPOINT ["voicemailbot", "-config", "/config/config.yaml"]
