FROM golang:1.26 AS godoc
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go install github.com/princjef/gomarkdoc/cmd/gomarkdoc@latest
RUN mkdir -p /godoc && \
    for pkg in admin audit auth config encryption lifecycle server storage telemetry ui; do \
        printf -- '---\ntitle: "%s"\n---\n\n' "$pkg" > /godoc/$pkg.md && \
        gomarkdoc ./internal/$pkg >> /godoc/$pkg.md; \
    done && \
    printf -- '---\ntitle: "Go API Reference"\nchapter: true\nweight: 40\n---\n\nGenerated documentation for the Go packages in this project.\n' > /godoc/_index.md

FROM hugomods/hugo:latest AS build
WORKDIR /src/web
COPY web/ /src/web/
COPY docs/ /src/docs/
COPY --from=godoc /godoc/ /src/web/content/godoc/
RUN hugo --minify

FROM nginx:alpine
COPY --from=build /src/web/public /usr/share/nginx/html
EXPOSE 80
