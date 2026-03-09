FROM --platform=$BUILDPLATFORM golang:1.26.1 AS godoc
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN go install github.com/princjef/gomarkdoc/cmd/gomarkdoc@latest
RUN mkdir -p /godoc && \
    for pkg in admin audit auth config encryption lifecycle server storage telemetry ui; do \
        printf -- '---\ntitle: "%s"\n---\n\n' "$pkg" > /godoc/$pkg.md && \
        gomarkdoc ./internal/$pkg >> /godoc/$pkg.md && \
        sed -i '/^# '"$pkg"'$/d' /godoc/$pkg.md; \
    done

FROM --platform=$BUILDPLATFORM hugomods/hugo:latest AS build
WORKDIR /src/web
COPY web/ /src/web/
COPY docs/ /src/docs/
COPY README.md /src/README.md
COPY CHANGELOG.md /src/CHANGELOG.md
COPY --from=godoc /godoc/ /src/web/content/godoc/
# Restore our custom _index.md (godoc stage doesn't generate one anymore)
COPY web/content/godoc/_index.md /src/web/content/godoc/_index.md
RUN hugo --minify

FROM nginx:alpine
RUN apk upgrade --no-cache
COPY --from=build /src/web/public /usr/share/nginx/html
EXPOSE 80
