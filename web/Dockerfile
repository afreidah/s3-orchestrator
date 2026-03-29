FROM --platform=$BUILDPLATFORM golang:1.26.1 AS godoc
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN go install github.com/princjef/gomarkdoc/cmd/gomarkdoc@latest
RUN mkdir -p /godoc && \
    for spec in \
        proxy store backend config counter encryption lifecycle breaker worker notify cache \
        transport/s3api:s3api transport/admin:admin transport/ui:ui transport/auth:auth transport/httputil:httputil \
        observe/telemetry:telemetry observe/audit:audit observe/event:event \
        util/bufpool:bufpool util/syncutil:syncutil util/workerpool:workerpool; \
    do \
        pkg=${spec%%:*}; \
        name=${spec##*:}; \
        printf -- '---\ntitle: "%s"\n---\n\n' "$name" > /godoc/$name.md && \
        gomarkdoc ./internal/$pkg >> /godoc/$name.md && \
        sed -i '/^# '"$name"'$/d' /godoc/$name.md; \
    done

FROM --platform=$BUILDPLATFORM hugomods/hugo:latest AS build
WORKDIR /src/web
COPY web/ /src/web/
COPY docs/ /src/docs/
COPY README.md /src/README.md
COPY CHANGELOG.md /src/CHANGELOG.md
COPY --from=godoc /godoc/ /src/web/content/godoc/
# Restore the hand-maintained index page overwritten by the godoc stage
COPY web/content/godoc/_index.md /src/web/content/godoc/_index.md
RUN hugo --minify

FROM nginx:alpine
RUN apk upgrade --no-cache
COPY --from=build /src/web/public /usr/share/nginx/html
EXPOSE 80
